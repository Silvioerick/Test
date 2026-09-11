package auction

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type rec struct {
	mu sync.Mutex
	ev []Event
}

func (r *rec) add(_ context.Context, e Event) { r.mu.Lock(); r.ev = append(r.ev, e); r.mu.Unlock() }
func (r *rec) count(k EventKind) (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ev {
		if e.Kind == k {
			n++
		}
	}
	return
}

func setup(t *testing.T) *redis.Client {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"})
	rdb.FlushAll(context.Background())
	return rdb
}

func TestRules(t *testing.T) {
	ctx := context.Background()
	rdb := setup(t)
	r := &rec{}
	e := New(rdb, Options{OnEvent: r.add})
	if _, err := e.Create(ctx, "a", Config{StartPrice: 10000, MinIncrement: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(ctx, "a", Config{StartPrice: 1, MinIncrement: 1}); err != ErrExists {
		t.Fatal("esperava ErrExists", err)
	}
	chk := func(bidder string, amt int64, plus bool, msg string, want Status, wantAmt int64) BidResult {
		t.Helper()
		res, err := e.PlaceBid(ctx, "a", bidder, amt, plus, msg)
		if err != nil || res.Status != want || res.Amount != wantAmt {
			t.Fatalf("%s %d: got %+v err=%v, want %s %d", bidder, amt, res, err, want, wantAmt)
		}
		return res
	}
	chk("ana", 9999, false, "m1", TooLow, 10000)
	chk("ana", 10000, false, "m2", Accepted, 10000)
	chk("ana", 10000, false, "m2", Duplicate, 0)
	chk("ana", 20000, false, "m3", AlreadyLeading, 10000)
	chk("bia", 10400, false, "m4", TooLow, 10500)
	res := chk("bia", 0, true, "m5", Accepted, 10500)
	if res.PrevLeader != "ana" || res.Extended {
		t.Fatalf("prev/extended errado: %+v", res)
	}
	chk("ana", 0, false, "m6", TooLow, 0)
	chk("x", 1, false, "m7", TooLow, 11000)
	if _, err := e.PlaceBid(ctx, "zzz", "ana", 100, false, "m8"); err != nil {
		t.Fatal(err)
	}
	s, _ := e.Get(ctx, "a")
	if s.Current != 10500 || s.Leader != "bia" || s.Bids != 2 || s.MinNext != 11000 || s.Remaining < 55*time.Second {
		t.Fatalf("snapshot errado %+v", s)
	}
	if r.count(EventBid) != 2 {
		t.Fatal("eventos de lance", r.ev)
	}
}

func TestSoftCloseResetNotAdd(t *testing.T) {
	ctx := context.Background()
	rdb := setup(t)
	e := New(rdb, Options{})
	e.Create(ctx, "b", Config{StartPrice: 100, MinIncrement: 1, Duration: 1500 * time.Millisecond, ExtendTo: time.Second})
	time.Sleep(700 * time.Millisecond) // ~800ms restando: fora da janela? não, < 1s → prorroga
	var last BidResult
	ext := false
	for i := 0; i < 5; i++ { // rajada
		last, _ = e.PlaceBid(ctx, "b", fmt.Sprint("u", i), 0, true, fmt.Sprint("r", i))
		if last.Status != Accepted {
			t.Fatal(last)
		}
		ext = ext || last.Extended
	}
	if !ext || last.Remaining > time.Second || last.Remaining < 950*time.Millisecond {
		t.Fatalf("rajada deveria resetar p/ ~1s, não somar: %+v", last)
	}
}

func TestHardCap(t *testing.T) {
	ctx := context.Background()
	rdb := setup(t)
	e := New(rdb, Options{})
	e.Create(ctx, "c", Config{StartPrice: 100, MinIncrement: 1, Duration: time.Second, ExtendTo: 2 * time.Second, HardCap: 1200 * time.Millisecond})
	res, _ := e.PlaceBid(ctx, "c", "ana", 0, true, "")
	if res.Remaining > 1200*time.Millisecond {
		t.Fatalf("passou do teto: %+v", res)
	}
}

func TestCloseWarningsAndTwoReplicas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdb := setup(t)
	r := &rec{}
	opts := func(n string) Options {
		return Options{NodeID: n, OnEvent: r.add, Warnings: []time.Duration{1500 * time.Millisecond, 500 * time.Millisecond}}
	}
	e1, e2 := New(rdb, opts("n1")), New(rdb, opts("n2"))
	go e1.Run(ctx)
	go e2.Run(ctx)
	e1.Create(ctx, "d", Config{StartPrice: 100, MinIncrement: 10, Duration: 2 * time.Second, ExtendTo: time.Second})
	time.Sleep(1700 * time.Millisecond)                     // avisos de 1.5s e ... ~300ms restando
	res, _ := e2.PlaceBid(ctx, "d", "bia", 150, false, "x") // prorroga p/ 1s
	if !res.Extended {
		t.Fatalf("esperava prorrogação: %+v", res)
	}
	time.Sleep(1400 * time.Millisecond)
	after, _ := e1.PlaceBid(ctx, "d", "ana", 999, false, "y")
	if after.Status != Closed {
		t.Fatalf("lance após fechar aceito: %+v", after)
	}
	time.Sleep(300 * time.Millisecond)
	if n := r.count(EventClosed); n != 1 {
		t.Fatalf("encerramentos: %d", n)
	}
	var closed Event
	var warns []time.Duration
	for _, ev := range r.ev {
		switch ev.Kind {
		case EventClosed:
			closed = ev
		case EventWarning:
			warns = append(warns, ev.Remaining.Round(100*time.Millisecond))
		}
	}
	if closed.Bidder != "bia" || closed.Amount != 150 || closed.Bids != 1 {
		t.Fatalf("vencedor errado %+v", closed)
	}
	// relógio original: 1.5s e 0.5s; após a prorrogação p/ 1s: só 0.5s.
	// Com duas réplicas, qualquer duplicação daria mais de 3.
	t.Logf("avisos: %v", warns)
	if len(warns) != 3 {
		t.Fatalf("avisos duplicados ou faltando: %v", warns)
	}
	if rdb.SIsMember(ctx, openSet, "d").Val() {
		t.Fatal("lote continuou no set de abertos")
	}
}

func TestParseBid(t *testing.T) {
	cases := map[string]int64{
		"150": 15000, "R$ 150": 15000, "r$150,5": 15050, "150,50": 15050,
		"1.500": 150000, "1.500,00": 150000, "lance 200": 20000, "150.5": 15050,
		"1.500.000": 150000000,
	}
	for in, want := range cases {
		got, plus, ok := ParseBid(in)
		if !ok || plus || got != want {
			t.Errorf("%q: %d %v %v want %d", in, got, plus, ok, want)
		}
	}
	for _, in := range []string{"+", "mais", " MAIS "} {
		if _, plus, ok := ParseBid(in); !ok || !plus {
			t.Errorf("%q deveria ser +", in)
		}
	}
	for _, in := range []string{"", "oi", "1e5", "150,555", "-10", "0", "inf", "15o"} {
		if _, _, ok := ParseBid(in); ok {
			t.Errorf("%q deveria falhar", in)
		}
	}
}
