// Package auction implementa um motor de leilão relâmpago (1–3 min) com
// soft close, pensado para ser alimentado por mensagens de WhatsApp.
//
// Toda decisão de tempo usa o relógio do Redis (TIME dentro dos scripts Lua),
// então réplicas com relógios diferentes nunca discordam sobre quem venceu, e
// um lance que chega depois do fechamento é recusado mesmo que o encerramento
// ainda não tenha sido processado. Requer Redis >= 7.
//
// Uso:
//
//	eng := auction.New(rdb, auction.Options{
//		NodeID:   hostname,                                  // único por réplica
//		Warnings: []time.Duration{30 * time.Second, 10 * time.Second},
//		OnEvent:  notifier.Enqueue,                          // não pode bloquear
//	})
//	go eng.Run(ctx)
//
//	eng.Create(ctx, "lote-42", auction.Config{StartPrice: 10000, MinIncrement: 500})
//
//	// webhook do DigiGO, mensagem privada (um lote ativo por número do bot):
//	if cents, plus, ok := auction.ParseBid(msg.Text); ok {
//		res, err := eng.PlaceBid(ctx, lotAtivo, msg.Sender, cents, plus, msg.ID)
//		// responder no privado conforme res.Status
//	}
package auction

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	MaxDuration     = 3 * time.Minute
	DefaultDuration = time.Minute
	DefaultExtendTo = 15 * time.Second
	leaseTTL        = 3 * time.Second
	dedupTTL        = 10 * time.Minute
)

const openSet = "auctions:open"

func keyAuction(id string) string { return "auction:{" + id + "}" }
func keyBids(id string) string    { return "auction:{" + id + "}:bids" }
func keyOwner(id string) string   { return "auction:{" + id + "}:owner" }
func keyDedup(id, msg string) string {
	return "auction:{" + id + "}:msg:" + msg
}

// Config de um lote. Valores monetários em centavos.
type Config struct {
	StartPrice   int64
	MinIncrement int64
	// Duração inicial. Padrão 1 min, máximo 3 min.
	Duration time.Duration
	// Lance recebido com menos que isso restando faz o relógio voltar para
	// esse valor (não soma). Padrão 15s.
	ExtendTo time.Duration
	// Teto absoluto desde a abertura, incluindo prorrogações. 0 = sem teto.
	HardCap time.Duration
	// MaxBid é o lance máximo aceito no lote, em centavos. 0 = sem teto.
	// Existe para impedir que uma mensagem trolada ("5000000000") ganhe o
	// lote e o trave até virar calote — o lance é recusado na hora, dentro
	// do mesmo script Lua que decide todo o resto.
	MaxBid int64
}

type Status string

const (
	Accepted       Status = "ok"
	TooLow         Status = "too_low"         // Amount = mínimo exigido
	AlreadyLeading Status = "already_leading" // Amount = lance atual
	Closed         Status = "closed"
	NotFound       Status = "not_found"
	Duplicate      Status = "duplicate" // mesma mensagem reentregue
	TooHigh        Status = "too_high"  // acima do teto do lote. Amount = teto
)

type BidResult struct {
	Status     Status
	Amount     int64
	EndsAt     time.Time
	Remaining  time.Duration
	Extended   bool
	PrevLeader string // quem acabou de ser superado ("" no primeiro lance)
}

type EventKind string

const (
	EventBid     EventKind = "bid"
	EventWarning EventKind = "warning"
	EventClosed  EventKind = "closed"
)

type Event struct {
	Kind       EventKind
	AuctionID  string
	Bidder     string // bid: autor do lance; closed: vencedor ("" = sem lances)
	PrevLeader string // bid: superado
	Amount     int64
	Bids       int64
	MsgID      string // bid: id da mensagem de origem, para persistência idempotente
	EndsAt     time.Time
	Remaining  time.Duration
	Extended   bool
}

type Snapshot struct {
	Status    string
	Current   int64
	Leader    string
	Bids      int64
	MinNext   int64
	EndsAt    time.Time
	Remaining time.Duration
}

type Options struct {
	NodeID   string
	Warnings []time.Duration
	TTL      time.Duration // retenção das chaves no Redis. Padrão 24h
	OnEvent  func(context.Context, Event)
	Logf     func(format string, args ...any)
}

type Engine struct {
	rdb      redis.UniversalClient
	node     string
	warnings []time.Duration
	ttl      time.Duration
	onEvent  func(context.Context, Event)
	logf     func(string, ...any)
	running  sync.Map
	kick     chan struct{}
}

func New(rdb redis.UniversalClient, o Options) *Engine {
	w := append([]time.Duration(nil), o.Warnings...)
	sort.Slice(w, func(i, j int) bool { return w[i] > w[j] })
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.NodeID == "" {
		o.NodeID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.OnEvent == nil {
		o.OnEvent = func(context.Context, Event) {}
	}
	return &Engine{
		rdb: rdb, node: o.NodeID, warnings: w, ttl: o.TTL,
		onEvent: o.OnEvent, logf: o.Logf, kick: make(chan struct{}, 1),
	}
}

const luaNow = `
local function s(n) return string.format('%d', n) end
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

// KEYS[1]=auction  ARGV: start, inc, duration_ms, extend_ms, hardcap_ms, ttl_s
var createScript = redis.NewScript(luaNow + `
if redis.call('EXISTS', KEYS[1]) == 1 then return {'exists'} end
local ends = now + tonumber(ARGV[3])
local cap = 0
if tonumber(ARGV[5]) > 0 then cap = now + tonumber(ARGV[5]) end
redis.call('HSET', KEYS[1], 'status', 'open', 'start_price', ARGV[1], 'min_inc', ARGV[2],
  'extend_ms', ARGV[4], 'cap_at', s(cap), 'started_at', s(now), 'ends_at', s(ends),
  'current', '0', 'leader', '', 'bids', '0', 'max_bid', ARGV[7])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[6]))
return {'ok', ends, now}
`)

// KEYS[1]=auction KEYS[2]=bids KEYS[3]=dedup
// ARGV: bidder, amount, msg_id, plus('1'|'0')
var bidScript = redis.NewScript(luaNow + `
local a = KEYS[1]
local f = redis.call('HMGET', a, 'status','ends_at','cap_at','extend_ms','current','leader','bids','start_price','min_inc','max_bid')
if not f[1] then return {'not_found'} end
if ARGV[3] ~= '' and not redis.call('SET', KEYS[3], '1', 'NX', 'EX', ` + strconv.Itoa(int(dedupTTL.Seconds())) + `) then
  return {'duplicate'}
end
if f[1] ~= 'open' then return {'closed'} end
local ends = tonumber(f[2])
if now >= ends then return {'closed'} end
local current, leader, nbids = tonumber(f[5]), f[6], tonumber(f[7])
local minimum = tonumber(f[8])
if nbids > 0 then minimum = current + tonumber(f[9]) end
if leader == ARGV[1] then return {'already_leading', current} end
local amount = tonumber(ARGV[2])
if ARGV[4] == '1' then amount = minimum end
if amount < minimum then return {'too_low', minimum} end
local maxbid = tonumber(f[10]) or 0
if maxbid > 0 and amount > maxbid then return {'too_high', maxbid} end
local extended = 0
local ext = tonumber(f[4])
if ends - now < ext then
  local ne = now + ext
  local cap = tonumber(f[3])
  if cap > 0 and ne > cap then ne = cap end
  if ne > ends then ends = ne; extended = 1 end
end
redis.call('HSET', a, 'current', s(amount), 'leader', ARGV[1], 'bids', s(nbids + 1), 'ends_at', s(ends))
redis.call('XADD', KEYS[2], '*', 'bidder', ARGV[1], 'amount', s(amount), 'at', s(now), 'msg', ARGV[3])
local ttl = redis.call('TTL', a)
if ttl > 0 then redis.call('EXPIRE', KEYS[2], ttl) end
return {'ok', amount, ends, extended, leader, now}
`)

// KEYS[1]=auction
var closeScript = redis.NewScript(luaNow + `
local f = redis.call('HMGET', KEYS[1], 'status','ends_at','current','leader','bids')
if not f[1] then return {'not_found'} end
if f[1] ~= 'open' then return {'already_closed'} end
local ends = tonumber(f[2])
if now < ends then return {'pending', ends, now} end
redis.call('HSET', KEYS[1], 'status', 'closed', 'closed_at', s(now))
return {'closed', tonumber(f[3]), f[4], tonumber(f[5]), ends}
`)

// KEYS[1]=owner  ARGV: node, ttl_ms
var leaseScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur == false or cur == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0
`)

var ErrExists = errors.New("auction: lote já existe")

func (c *Config) normalize() error {
	if c.Duration == 0 {
		c.Duration = DefaultDuration
	}
	if c.ExtendTo == 0 {
		c.ExtendTo = DefaultExtendTo
		if c.Duration > 0 && c.ExtendTo > c.Duration {
			// O padrão se ajusta a lotes curtos em vez de recusá-los; um
			// ExtendTo explícito maior que a duração continua sendo erro.
			c.ExtendTo = c.Duration
		}
	}
	switch {
	case c.StartPrice <= 0 || c.MinIncrement <= 0:
		return errors.New("auction: StartPrice e MinIncrement devem ser > 0")
	case c.Duration < 0 || c.Duration > MaxDuration:
		return fmt.Errorf("auction: duração deve ficar entre 1s e %s", MaxDuration)
	case c.ExtendTo < 0:
		return errors.New("auction: ExtendTo inválido")
	case c.ExtendTo > c.Duration:
		// Sem isso, um lote de 5s com o ExtendTo padrão de 15s já nascia
		// prorrogando: o primeiro lance esticava o leilão para além da
		// duração contratada.
		return fmt.Errorf("auction: ExtendTo (%s) não pode ser maior que a duração (%s)", c.ExtendTo, c.Duration)
	case c.HardCap != 0 && c.HardCap < c.Duration:
		return errors.New("auction: HardCap menor que a duração")
	case c.MaxBid < 0:
		return errors.New("auction: MaxBid inválido")
	case c.MaxBid > 0 && c.MaxBid < c.StartPrice:
		return errors.New("auction: MaxBid menor que o preço inicial")
	}
	return nil
}

// Create abre o lote imediatamente; o relógio começa a correr no Redis.
func (e *Engine) Create(ctx context.Context, id string, c Config) (time.Time, error) {
	if err := c.normalize(); err != nil {
		return time.Time{}, err
	}
	res, err := createScript.Run(ctx, e.rdb, []string{keyAuction(id)},
		c.StartPrice, c.MinIncrement, c.Duration.Milliseconds(),
		c.ExtendTo.Milliseconds(), c.HardCap.Milliseconds(), int64(e.ttl.Seconds()),
		c.MaxBid).Slice()
	if err != nil {
		return time.Time{}, err
	}
	if toStr(res[0]) == "exists" {
		return time.Time{}, ErrExists
	}
	if err := e.rdb.SAdd(ctx, openSet, id).Err(); err != nil {
		// O lote existe no Redis mas não entrou no conjunto observado:
		// ninguém emitiria avisos nem o encerraria. Desfaz para não deixar
		// um lote fantasma correndo.
		e.rdb.Del(ctx, keyAuction(id))
		return time.Time{}, fmt.Errorf("auction: registrar lote no conjunto aberto: %w", err)
	}
	select {
	case e.kick <- struct{}{}:
	default:
	}
	return time.UnixMilli(toInt(res[1])), nil
}

// PlaceBid registra um lance. plus=true significa "lance mínimo seguinte"
// (mensagem "+"), e amount é ignorado. msgID deduplica reentregas do WhatsApp.
func (e *Engine) PlaceBid(ctx context.Context, id, bidder string, amount int64, plus bool, msgID string) (BidResult, error) {
	p := "0"
	if plus {
		p = "1"
	} else if amount <= 0 {
		return BidResult{Status: TooLow}, nil
	}
	res, err := bidScript.Run(ctx, e.rdb,
		[]string{keyAuction(id), keyBids(id), keyDedup(id, msgID)},
		bidder, amount, msgID, p).Slice()
	if err != nil {
		return BidResult{}, err
	}
	r := BidResult{Status: Status(toStr(res[0]))}
	switch r.Status {
	case TooLow, AlreadyLeading, TooHigh:
		r.Amount = toInt(res[1])
	case Accepted:
		ends, now := toInt(res[2]), toInt(res[5])
		r.Amount = toInt(res[1])
		r.EndsAt = time.UnixMilli(ends)
		r.Remaining = time.Duration(ends-now) * time.Millisecond
		r.Extended = toInt(res[3]) == 1
		r.PrevLeader = toStr(res[4])
		e.onEvent(ctx, Event{
			Kind: EventBid, AuctionID: id, Bidder: bidder, PrevLeader: r.PrevLeader,
			Amount: r.Amount, MsgID: msgID, EndsAt: r.EndsAt, Remaining: r.Remaining, Extended: r.Extended,
		})
	}
	return r, nil
}

func (e *Engine) Get(ctx context.Context, id string) (Snapshot, error) {
	h, err := e.rdb.HGetAll(ctx, keyAuction(id)).Result()
	if err != nil {
		return Snapshot{}, err
	}
	if len(h) == 0 {
		return Snapshot{}, redis.Nil
	}
	now, err := e.rdb.Time(ctx).Result()
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{
		Status:  h["status"],
		Current: toInt(h["current"]),
		Leader:  h["leader"],
		Bids:    toInt(h["bids"]),
		EndsAt:  time.UnixMilli(toInt(h["ends_at"])),
	}
	s.MinNext = toInt(h["start_price"])
	if s.Bids > 0 {
		s.MinNext = s.Current + toInt(h["min_inc"])
	}
	if s.Status == "open" && s.EndsAt.After(now) {
		s.Remaining = s.EndsAt.Sub(now)
	}
	return s, nil
}

// Run mantém um watcher para cada lote aberto. Pode rodar em várias réplicas:
// um lease por lote garante que só uma emite avisos e encerra, e outra assume
// em até leaseTTL se ela cair.
func (e *Engine) Run(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		ids, err := e.rdb.SMembers(ctx, openSet).Result()
		if err != nil && ctx.Err() == nil {
			e.logf("auction: listar lotes abertos: %v", err)
		}
		for _, id := range ids {
			if _, loaded := e.running.LoadOrStore(id, struct{}{}); !loaded {
				go e.watch(ctx, id)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-e.kick:
		}
	}
}

func (e *Engine) watch(ctx context.Context, id string) {
	defer e.running.Delete(id)
	var lastEnds int64
	fired := 0
	for ctx.Err() == nil {
		own, err := leaseScript.Run(ctx, e.rdb, []string{keyOwner(id)}, e.node, leaseTTL.Milliseconds()).Int()
		if err != nil || own == 0 {
			if err != nil {
				e.logf("auction %s: lease: %v", id, err)
			}
			sleep(ctx, leaseTTL/3)
			continue
		}
		res, err := closeScript.Run(ctx, e.rdb, []string{keyAuction(id)}).Slice()
		if err != nil {
			e.logf("auction %s: close: %v", id, err)
			sleep(ctx, 200*time.Millisecond)
			continue
		}
		switch toStr(res[0]) {
		case "pending":
		case "closed":
			e.rdb.SRem(ctx, openSet, id)
			e.onEvent(ctx, Event{
				Kind: EventClosed, AuctionID: id, Amount: toInt(res[1]),
				Bidder: toStr(res[2]), Bids: toInt(res[3]), EndsAt: time.UnixMilli(toInt(res[4])),
			})
			return
		default:
			e.rdb.SRem(ctx, openSet, id)
			return
		}

		ends, now := toInt(res[1]), toInt(res[2])
		remaining := time.Duration(ends-now) * time.Millisecond
		if ends != lastEnds {
			// Relógio novo (abertura ou prorrogação): avisos que já ficaram
			// para trás são descartados; o evento do lance já informa o tempo.
			lastEnds = ends
			fired = 0
			for fired < len(e.warnings) && e.warnings[fired] >= remaining {
				fired++
			}
		}
		warn := false
		for fired < len(e.warnings) && remaining <= e.warnings[fired] {
			fired++
			warn = true
		}
		if warn {
			e.onEvent(ctx, Event{
				Kind: EventWarning, AuctionID: id,
				EndsAt: time.UnixMilli(ends), Remaining: remaining,
			})
		}

		wait := remaining
		if fired < len(e.warnings) {
			wait = remaining - e.warnings[fired]
		}
		if wait > leaseTTL/3 {
			wait = leaseTTL / 3
		}
		if wait < 5*time.Millisecond {
			wait = 5 * time.Millisecond
		}
		sleep(ctx, wait)
	}
}

// ParseBid interpreta a mensagem do participante:
// "+" ou "mais" = lance mínimo seguinte; "150", "R$ 150", "150,50",
// "1.500", "1.500,00", "lance 200" = valor em centavos.
func ParseBid(text string) (cents int64, plus bool, ok bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.TrimPrefix(s, "lance")
	s = strings.ReplaceAll(s, "r$", "")
	s = strings.Join(strings.Fields(s), "")
	if s == "+" || s == "mais" {
		return 0, true, true
	}
	if s == "" || len(s) > 16 {
		return 0, false, false
	}
	if strings.Contains(s, ",") {
		s = strings.ReplaceAll(s, ".", "")
		s = strings.Replace(s, ",", ".", 1)
	} else if i := strings.LastIndexByte(s, '.'); i >= 0 && len(s)-i-1 == 3 {
		s = strings.ReplaceAll(s, ".", "")
	}
	ip, frac, _ := strings.Cut(s, ".")
	if ip == "" || len(frac) > 2 || !digits(ip) || !digits(frac) {
		return 0, false, false
	}
	for len(frac) < 2 {
		frac += "0"
	}
	n, err := strconv.ParseInt(ip+frac, 10, 64)
	if err != nil || n <= 0 {
		return 0, false, false
	}
	return n, false, true
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
