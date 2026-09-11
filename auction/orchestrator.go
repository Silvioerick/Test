package auction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type NoticeKind string

const (
	NoticeAwaitingRegistration NoticeKind = "awaiting_registration" // vencedor precisa cadastrar endereço
	NoticeRegistrationExpired  NoticeKind = "registration_expired"  // não cadastrou a tempo
	NoticeAssigned             NoticeKind = "assigned"              // cobrança criada, tem X min pra pagar
	NoticeDefaulted            NoticeKind = "defaulted"             // não pagou a tempo
	NoticeUnsold               NoticeKind = "unsold"                // ninguém pagou, produto voltou pro estoque
	NoticePaid                 NoticeKind = "paid"                  // pagamento confirmado
	// NoticeChargeFailed: não conseguimos gerar a cobrança (provedor fora
	// do ar). NÃO é calote do cliente — exige olhar humano.
	NoticeChargeFailed NoticeKind = "charge_failed"
	// NoticeStaleLot: o lote fechou no Redis mas o Postgres não soube a
	// tempo e a reconciliação teve de resolver. Sinal de fila entupida.
	NoticeStaleLot NoticeKind = "stale_lot"
	// NoticePaidAfterExpiry: dinheiro entrou depois de o produto já ter
	// voltado pro estoque. Alarme de reconciliação manual/estorno.
	NoticePaidAfterExpiry NoticeKind = "paid_after_expiry"
)

// ErrNoActiveLot: chegou mensagem num canal que não tem leilão rolando.
var ErrNoActiveLot = errors.New("auction: nenhum lote aberto nesse canal")

// Notice é o gancho para avisos de grupo — o que muda o estado comercial do
// lote, não os lances em si (esses já saem pelo OnEvent do Engine).
type Notice struct {
	Kind   NoticeKind
	LotID  string
	JID    string
	Amount int64
	// Detail traz contexto legível para os avisos de alarme (motivo da
	// falha de cobrança, id da cobrança órfã, etc.).
	Detail string
}

// Orchestrator liga o motor de lances (Redis) à parte durável do negócio:
// cadastro de cliente, frete, Postgres e o provedor de cobrança (HubPay,
// Asaas, ...). Use HandleEvent como Options.OnEvent do Engine.
//
// Eventos de LANCE e AVISO passam por uma fila que pode descartar sob
// pressão (o Redis continua sendo a fonte da verdade do lance em si).
// Eventos de FECHAMENTO nunca são descartados: é neles que se decide quem
// paga quanto. Se mesmo assim um se perder (processo morto entre o
// fechamento no Redis e a escrita no Postgres), o RunReconciler encontra o
// lote e conclui o fluxo.
//
// Fluxo depois que o lote fecha com vencedor:
//  1. Cadastro incompleto (sem endereço) -> manda link de cadastro, lote
//     fica "awaiting_registration". Sem prazo cumprido -> calote, produto
//     volta pro estoque (sem segundo colocado).
//  2. Cadastro completo -> calcula frete pelo CEP, aplica desconto se for
//     o 2º+ item que essa pessoa ganha no dia, cria a cobrança e manda.
//  3. Sem prazo de pagamento cumprido -> mesma regra de calote acima.
type Orchestrator struct {
	store          *Store
	pay            PaymentProvider
	notify         Notifier
	shipping       ShippingCalculator
	discountRule   ShippingDiscountRule
	paymentWindow  time.Duration
	registerWindow time.Duration
	providerName   string
	registerURL    string         // fallback; o painel tem precedência
	settings       *SettingsStore // configuração editável no painel (pode ser nil)
	maxChargeTries int
	bidAnnounceGap time.Duration
	lastAnnounce   sync.Map // lotID -> time.Time do último aviso no grupo
	queue          chan Event
	onNotice       func(context.Context, Notice)
	logf           func(string, ...any)
	channels       sync.Map // lotID -> channelJID, para avisar o grupo
}

type OrchestratorOptions struct {
	PaymentWindow      time.Duration // padrão usado quando CreateAuction não especifica um. Padrão geral: 15 min
	RegistrationWindow time.Duration // prazo pra preencher endereço depois de ganhar. Padrão: igual ao PaymentWindow
	ProviderName       string        // rótulo gravado em payment_orders.provider, ex. "hubpay" ou "asaas"
	RegisterURL        string        // prefixo do link de cadastro; o token é concatenado no final
	// Settings, quando presente, deixa o prefixo do link de cadastro ser
	// editado no painel em vez de ficar preso em variável de ambiente.
	Settings  *SettingsStore
	QueueSize int // padrão 256
	// MaxChargeAttempts: quantas vezes tentar gerar a cobrança antes de
	// desistir e liberar o produto. Padrão 5.
	MaxChargeAttempts int
	// BidAnnounceInterval: intervalo mínimo entre avisos de lance NO GRUPO.
	// Um leilão disputado gera dezenas de lances por minuto; mandar um
	// aviso por lance entope o grupo e é o tipo de tráfego que derruba um
	// número comercial no WhatsApp. O aviso privado de "você foi superado"
	// não é afetado, e o último lance antes do fechamento sempre sai no
	// aviso de encerramento. Padrão 3s; use -1 para avisar todos.
	BidAnnounceInterval time.Duration
	// OnNotice recebe mudanças de estado comercial (aguardando cadastro,
	// calote, pago, não vendido) para você postar no grupo. Não pode bloquear.
	OnNotice func(context.Context, Notice)
	Logf     func(string, ...any)
}

func NewOrchestrator(store *Store, pay PaymentProvider, notify Notifier, shipping ShippingCalculator, o OrchestratorOptions) *Orchestrator {
	if o.PaymentWindow <= 0 {
		o.PaymentWindow = 15 * time.Minute
	}
	if o.RegistrationWindow <= 0 {
		o.RegistrationWindow = o.PaymentWindow
	}
	if o.ProviderName == "" {
		o.ProviderName = "default"
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.MaxChargeAttempts <= 0 {
		o.MaxChargeAttempts = 5
	}
	switch {
	case o.BidAnnounceInterval < 0:
		o.BidAnnounceInterval = 0
	case o.BidAnnounceInterval == 0:
		o.BidAnnounceInterval = 3 * time.Second
	}
	if o.OnNotice == nil {
		o.OnNotice = func(context.Context, Notice) {}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Orchestrator{
		store: store, pay: pay, notify: notify, shipping: shipping,
		discountRule:  DefaultShippingDiscountRule(),
		paymentWindow: o.PaymentWindow, registerWindow: o.RegistrationWindow,
		providerName: o.ProviderName, registerURL: o.RegisterURL, settings: o.Settings,
		maxChargeTries: o.MaxChargeAttempts, bidAnnounceGap: o.BidAnnounceInterval,
		queue: make(chan Event, o.QueueSize), onNotice: o.OnNotice, logf: o.Logf,
	}
}

// SetDiscountRule troca a regra de desconto de frete (padrão: grátis a
// partir do 2º item ganho no mesmo dia).
func (o *Orchestrator) SetDiscountRule(r ShippingDiscountRule) { o.discountRule = r }

// Run processa a fila de eventos. Chame uma vez, num goroutine, com um
// contexto de vida longa (ex.: o mesmo da aplicação).
func (o *Orchestrator) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-o.queue:
			o.process(ctx, ev)
		}
	}
}

// closedEnqueueTimeout: quanto o watcher do lote aceita esperar para
// entregar um fechamento antes de deixar o reconciliador resolver.
const closedEnqueueTimeout = 5 * time.Second

// HandleEvent é o Options.OnEvent do Engine.
//
// Lance e aviso: não bloqueiam quem está dando lance; sob pressão são
// descartados e logados (o valor vencedor não depende mais disso — veja
// process/EventClosed, que usa o valor vindo do próprio Redis).
//
// Fechamento: NUNCA é descartado. Ele roda no watcher do lote, não no
// caminho do lance, então esperar aqui não atrasa ninguém que esteja
// dando lance.
func (o *Orchestrator) HandleEvent(ctx context.Context, ev Event) {
	if ev.Kind == EventClosed {
		t := time.NewTimer(closedEnqueueTimeout)
		defer t.Stop()
		select {
		case o.queue <- ev:
		case <-ctx.Done():
			o.logf("auction: contexto cancelado ao enfileirar fechamento de %s; reconciliador assume", ev.AuctionID)
		case <-t.C:
			o.logf("auction: fila entupida por %s ao enfileirar fechamento de %s; reconciliador assume",
				closedEnqueueTimeout, ev.AuctionID)
		}
		return
	}
	select {
	case o.queue <- ev:
	default:
		o.logf("auction: fila do orquestrador cheia, evento %s/%s perdido", ev.Kind, ev.AuctionID)
	}
}

func (o *Orchestrator) process(ctx context.Context, ev Event) {
	var err error
	switch ev.Kind {
	case EventBid:
		err = o.store.RecordBid(ctx, ev.AuctionID, ev.Bidder, "", ev.Amount, ev.MsgID)
		o.announceBid(ctx, ev)
	case EventWarning:
		o.announceWarning(ctx, ev)
	case EventClosed:
		o.announceClosed(ctx, ev)
		if ev.Bidder == "" {
			err = o.store.CloseNoBids(ctx, ev.AuctionID)
			break
		}
		// ev.Amount vem do Redis, que é a autoridade sobre o lance
		// vencedor. Antes isto relia lots.current_amount do Postgres — se
		// um evento de lance tivesse sido descartado, o vencedor era
		// cobrado por um valor menor do que o que ele deu.
		err = o.onWinner(ctx, ev.AuctionID, ev.Bidder, ev.Amount)
	}
	if err != nil {
		o.logf("auction: processar evento %s/%s: %v", ev.Kind, ev.AuctionID, err)
	}
}

// --- avisos no grupo ---------------------------------------------------

// channelFor descobre (e memoriza) em qual canal de WhatsApp o lote está
// sendo disputado, para os avisos de lance e contagem regressiva.
func (o *Orchestrator) channelFor(ctx context.Context, lotID string) string {
	if v, ok := o.channels.Load(lotID); ok {
		return v.(string)
	}
	ch, _, err := o.store.LotChannel(ctx, lotID)
	if err != nil {
		return ""
	}
	o.channels.Store(lotID, ch)
	return ch
}

func (o *Orchestrator) announceBid(ctx context.Context, ev Event) {
	ch := o.channelFor(ctx, ev.AuctionID)
	if ch == "" {
		return
	}
	// O privado de "você foi superado" sempre sai; o aviso no grupo é que
	// é limitado, para não inundar a conversa.
	defer o.announceOutbid(ctx, ev)
	if !o.mayAnnounce(ev.AuctionID) {
		return
	}
	text := fmt.Sprintf("Novo lance: %s (%s). Faltam %s!",
		formatCents(ev.Amount), shortJID(ev.Bidder), formatRemaining(ev.Remaining))
	if ev.Extended {
		text += " Tempo estendido."
	}
	if err := o.notify.SendText(ctx, ch, text); err != nil {
		o.logf("auction: avisar grupo sobre lance em %s: %v", ev.AuctionID, err)
	}
}

func (o *Orchestrator) announceOutbid(ctx context.Context, ev Event) {
	if ev.PrevLeader == "" || ev.PrevLeader == ev.Bidder {
		return
	}
	msg := fmt.Sprintf("Seu lance foi superado: agora está em %s. Responda com um valor ou \"+\" para cobrir.",
		formatCents(ev.Amount))
	if err := o.notify.SendText(ctx, ev.PrevLeader, msg); err != nil {
		o.logf("auction: avisar superado em %s: %v", ev.AuctionID, err)
	}
}

// mayAnnounce limita a frequência dos avisos de lance no grupo.
func (o *Orchestrator) mayAnnounce(lotID string) bool {
	if o.bidAnnounceGap <= 0 {
		return true
	}
	now := time.Now()
	if v, ok := o.lastAnnounce.Load(lotID); ok {
		if now.Sub(v.(time.Time)) < o.bidAnnounceGap {
			return false
		}
	}
	o.lastAnnounce.Store(lotID, now)
	return true
}

func (o *Orchestrator) announceWarning(ctx context.Context, ev Event) {
	ch := o.channelFor(ctx, ev.AuctionID)
	if ch == "" {
		return
	}
	if err := o.notify.SendText(ctx, ch,
		fmt.Sprintf("Faltam %s para encerrar!", formatRemaining(ev.Remaining))); err != nil {
		o.logf("auction: avisar contagem regressiva em %s: %v", ev.AuctionID, err)
	}
}

func (o *Orchestrator) announceClosed(ctx context.Context, ev Event) {
	ch := o.channelFor(ctx, ev.AuctionID)
	o.channels.Delete(ev.AuctionID)
	o.lastAnnounce.Delete(ev.AuctionID)
	if ch == "" {
		return
	}
	text := "Leilão encerrado sem lances."
	if ev.Bidder != "" {
		text = fmt.Sprintf("Arrematado por %s — %s! Já te chamei no privado.",
			formatCents(ev.Amount), shortJID(ev.Bidder))
	}
	if err := o.notify.SendText(ctx, ch, text); err != nil {
		o.logf("auction: avisar fechamento de %s: %v", ev.AuctionID, err)
	}
}

// --- abertura -----------------------------------------------------------

// CreateAuction cria o lote nas duas camadas: registro durável no Postgres
// (com a janela de pagamento desse lote específico) e o relógio de verdade
// no Redis. paymentWindow <= 0 usa o padrão do Orchestrator. channelJID é
// o grupo/número onde o leilão acontece — é ele que o webhook do WhatsApp
// usa para saber a qual lote uma mensagem pertence.
func (o *Orchestrator) CreateAuction(ctx context.Context, engine *Engine, lotID string, productID int64, cfg Config, paymentWindow time.Duration, channelJID string) (time.Time, error) {
	if paymentWindow <= 0 {
		paymentWindow = o.paymentWindow
	}
	if err := cfg.normalize(); err != nil {
		return time.Time{}, err
	}
	if err := o.store.CreateLot(ctx, lotID, productID, cfg, paymentWindow, channelJID); err != nil {
		return time.Time{}, err
	}
	endsAt, err := engine.Create(ctx, lotID, cfg)
	if err != nil {
		// Desfaz a reserva: sem o relógio no Redis o lote nunca correria, e
		// o produto ficaria preso em 'in_auction' para sempre.
		if rbErr := o.store.AbortLot(ctx, lotID); rbErr != nil {
			o.logf("auction: lote %s falhou no redis e não consegui desfazer no postgres: %v", lotID, rbErr)
			return time.Time{}, fmt.Errorf("criar lote no redis: %w (rollback falhou: %v)", err, rbErr)
		}
		return time.Time{}, fmt.Errorf("criar lote no redis: %w", err)
	}
	if channelJID != "" {
		o.channels.Store(lotID, channelJID)
	}
	return endsAt, nil
}

// --- vencedor -----------------------------------------------------------

// onWinner decide se já pode cobrar (cadastro completo) ou se precisa
// mandar o link de cadastro primeiro.
func (o *Orchestrator) onWinner(ctx context.Context, lotID, jid string, amount int64) error {
	info, err := o.store.GetOrCreateParticipant(ctx, jid, "")
	if err != nil {
		return fmt.Errorf("buscar participante: %w", err)
	}
	if err := o.store.SetWinningAmount(ctx, lotID, amount); err != nil {
		return fmt.Errorf("gravar lance vencedor: %w", err)
	}
	if !info.Registered {
		return o.sendRegistrationLink(ctx, lotID, info.ID, jid)
	}
	return o.chargeWinner(ctx, lotID, info.ID, jid, info.Customer, info.CEP, amount)
}

func (o *Orchestrator) sendRegistrationLink(ctx context.Context, lotID string, participantID int64, jid string) error {
	token, err := o.store.OpenRegistration(ctx, lotID, participantID, o.registerWindow)
	if err != nil {
		return fmt.Errorf("abrir cadastro: %w", err)
	}
	minutes := int(o.registerWindow.Minutes())
	text := fmt.Sprintf("Parabéns, você venceu o lote! Complete seu cadastro (endereço e documento) em até %d min pra gente calcular o frete e gerar o pagamento: %s%s",
		minutes, o.registerPrefix(ctx), token)
	if err := o.notify.SendText(ctx, jid, text); err != nil {
		return fmt.Errorf("enviar link de cadastro: %w", err)
	}
	o.onNotice(ctx, Notice{Kind: NoticeAwaitingRegistration, LotID: lotID, JID: jid})
	return nil
}

// chargeWinner calcula frete + desconto e gera a cobrança. Chamado tanto
// logo após o fechamento (cadastro já existia) quanto pela conclusão do
// cadastro público. bidAmount vem do Redis (fechamento) ou do lote já
// gravado (conclusão de cadastro).
func (o *Orchestrator) chargeWinner(ctx context.Context, lotID string, participantID int64, jid string, customer Customer, cep string, bidAmount int64) error {
	shippingCents, err := o.shipping.Quote(ctx, cep)
	if err != nil {
		return fmt.Errorf("calcular frete: %w", err)
	}
	priorToday, err := o.store.CountPaidToday(ctx, participantID)
	if err != nil {
		return fmt.Errorf("contar itens do dia: %w", err)
	}
	discountCents := o.discountRule.Apply(shippingCents, priorToday+1)

	orderID, total, err := o.store.AssignClaimant(ctx, lotID, participantID, bidAmount, shippingCents, discountCents)
	if err != nil {
		return fmt.Errorf("registrar cobrança: %w", err)
	}
	return o.issueCharge(ctx, orderID, lotID, jid, customer, total, bidAmount, shippingCents, discountCents)
}

// issueCharge gera a cobrança no provedor e manda pro cliente. Se o
// provedor falhar, o erro fica gravado no pedido e o worker de retry tenta
// de novo — o prazo de pagamento só começa a correr quando a cobrança
// existe de fato (ver Store.AttachCharge).
func (o *Orchestrator) issueCharge(ctx context.Context, orderID int64, lotID, jid string, customer Customer, total, bidAmount, shippingCents, discountCents int64) error {
	reference := fmt.Sprintf("lot-%s-order-%d", lotID, orderID)
	charge, err := o.pay.CreateCharge(ctx, reference, total, customer)
	if err != nil {
		cause := err.Error()
		if rErr := o.store.RecordChargeFailure(ctx, orderID, cause); rErr != nil {
			o.logf("auction: gravar falha de cobrança do pedido %d: %v", orderID, rErr)
		}
		return fmt.Errorf("gerar cobrança (%s): %w", o.providerName, err)
	}
	if err := o.store.AttachCharge(ctx, orderID, o.providerName, charge); err != nil {
		return fmt.Errorf("gravar cobrança: %w", err)
	}
	caption := fmt.Sprintf("Total a pagar: %s (produto %s + frete %s%s). Pague dentro do prazo ou o produto volta pro estoque.",
		formatCents(total), formatCents(bidAmount), formatCents(shippingCents),
		discountNote(discountCents))
	if err := o.notify.SendCharge(ctx, jid, caption, charge); err != nil {
		return fmt.Errorf("enviar cobrança: %w", err)
	}
	o.onNotice(ctx, Notice{Kind: NoticeAssigned, LotID: lotID, JID: jid, Amount: total})
	return nil
}

// registerPrefix resolve o prefixo do link de cadastro: painel primeiro,
// depois o que veio na construção (env).
func (o *Orchestrator) registerPrefix(ctx context.Context) string {
	if o.settings != nil {
		if v := o.settings.Get(ctx, SetRegisterURL); v != "" {
			return v
		}
	}
	return o.registerURL
}

func discountNote(discountCents int64) string {
	if discountCents <= 0 {
		return ""
	}
	return fmt.Sprintf(" - desconto %s por já ter ganhado outro item hoje", formatCents(discountCents))
}

// ErrChargePending: o cadastro foi gravado com sucesso, mas a cobrança
// não pôde ser gerada agora (provedor fora do ar). O pedido fica pendente
// e o RetryFailedCharges resolve — o cliente NÃO deve ver isso como
// falha, porque não há nada que ele possa fazer e os dados dele já estão
// salvos.
var ErrChargePending = errors.New("auction: cadastro salvo, cobrança será gerada em instantes")

// CompleteRegistration é chamado pelo handler HTTP público quando a pessoa
// termina de preencher o formulário de endereço.
//
// A ordem aqui importa. O token é de uso único, então tudo que pode
// falhar POR CULPA DO QUE A PESSOA DIGITOU acontece antes de consumi-lo —
// senão um CEP fora da área de entrega queimaria o link e a pessoa
// ficaria sem como corrigir, vendo "link expirado" na segunda tentativa.
//
// Depois de consumido, o que falha é problema nosso (provedor de cobrança
// fora do ar), e aí o cadastro vale e a cobrança entra na fila de retry.
func (o *Orchestrator) CompleteRegistration(ctx context.Context, token string, form CustomerForm) error {
	// 1. Token válido? Sem consumir.
	if _, err := o.store.GetRegistrationContext(ctx, token); err != nil {
		return err
	}
	// 2. O CEP é atendido? Falhar aqui deixa o link intacto.
	if _, err := o.shipping.Quote(ctx, form.CEP); err != nil {
		return fmt.Errorf("calcular frete: %w", err)
	}
	// 3. Agora sim: consome o token e grava o cadastro.
	rc, err := o.store.CompleteRegistration(ctx, token, form)
	if err != nil {
		return err
	}
	customer := Customer{Name: form.Name, Document: form.Document, Email: form.Email, Phone: rc.JID}
	if err := o.chargeWinner(ctx, rc.LotID, rc.ParticipantID, rc.JID, customer, form.CEP, rc.BidAmount); err != nil {
		o.logf("auction: cadastro do lote %s salvo mas a cobrança falhou (vai para retry): %v", rc.LotID, err)
		return fmt.Errorf("%w: %v", ErrChargePending, err)
	}
	return nil
}

// --- pagamento ----------------------------------------------------------

// OnPaymentWebhook é chamado pelo handler HTTP do webhook do provedor assim
// que uma cobrança é confirmada.
//
// O retorno diz ao handler o que responder ao provedor:
//   - nil ................. 200, processado (ou reentrega de algo já pago).
//   - ErrNoOrder .......... 5xx, para o provedor REENVIAR: o pagamento
//     chegou antes de o charge_id ter sido gravado.
//   - ErrOrderNotPending .. 200 com alarme: dinheiro entrou depois de o
//     produto já ter voltado pro estoque.
func (o *Orchestrator) OnPaymentWebhook(ctx context.Context, chargeID string) error {
	paid, err := o.store.MarkPaid(ctx, chargeID)
	switch {
	case errors.Is(err, ErrOrderAlreadyPaid):
		return nil
	case errors.Is(err, ErrNoOrder):
		o.logf("auction: pagamento %s confirmado mas nenhum pedido tem esse charge_id ainda — pedindo reentrega ao provedor", chargeID)
		return ErrNoOrder
	case errors.Is(err, ErrOrderNotPending):
		o.logf("auction: ALARME pagamento %s confirmado para pedido que já expirou — produto pode ter voltado pro estoque; reconciliação/estorno manual: %v", chargeID, err)
		o.onNotice(ctx, Notice{Kind: NoticePaidAfterExpiry, Detail: chargeID})
		return nil
	case err != nil:
		return err
	}
	o.onNotice(ctx, Notice{Kind: NoticePaid, LotID: paid.LotID})
	o.logf("auction: lote %s pago pelo participante %d", paid.LotID, paid.ParticipantID)
	return nil
}

// ExpireDuePayments varre pedidos de pagamento vencidos e não pagos:
// registra o calote e devolve o produto pro estoque (sem segundo
// colocado). Quem nunca chegou a receber uma cobrança NÃO é marcado como
// caloteiro. Seguro rodar em várias réplicas (FOR UPDATE SKIP LOCKED
// dentro de transação).
func (o *Orchestrator) ExpireDuePayments(ctx context.Context) (int, error) {
	due, err := o.store.ExpireDuePayments(ctx, 50)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		if d.HadCharge {
			o.onNotice(ctx, Notice{Kind: NoticeDefaulted, LotID: d.LotID, Amount: d.Amount})
		} else {
			o.logf("auction: pedido %d do lote %s venceu sem cobrança gerada — não é calote do cliente", d.OrderID, d.LotID)
			o.onNotice(ctx, Notice{Kind: NoticeChargeFailed, LotID: d.LotID, Amount: d.Amount})
		}
		o.onNotice(ctx, Notice{Kind: NoticeUnsold, LotID: d.LotID})
	}
	return len(due), nil
}

// ExpireDueRegistrations varre links de cadastro vencidos (ganhou mas não
// preencheu endereço a tempo) com a mesma lógica de calote.
func (o *Orchestrator) ExpireDueRegistrations(ctx context.Context) (int, error) {
	due, err := o.store.ExpireDueRegistrations(ctx, 50)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		o.onNotice(ctx, Notice{Kind: NoticeRegistrationExpired, LotID: d.LotID})
		o.onNotice(ctx, Notice{Kind: NoticeUnsold, LotID: d.LotID})
	}
	return len(due), nil
}

// RetryFailedCharges tenta de novo gerar as cobranças que falharam por
// culpa do provedor. Sem isto, o "pedido fica pendente pra retry" do
// código original era mentira: ninguém tentava de novo e o cliente virava
// caloteiro por um boleto que nunca recebeu.
func (o *Orchestrator) RetryFailedCharges(ctx context.Context) (int, error) {
	pending, err := o.store.PendingWithoutCharge(ctx, 20, o.maxChargeTries)
	if err != nil {
		return 0, err
	}
	for _, p := range pending {
		err := o.issueCharge(ctx, p.OrderID, p.LotID, p.JID, p.Customer,
			p.Amount, p.BidAmount, p.ShippingCents, p.DiscountCents)
		if err != nil {
			o.logf("auction: retry de cobrança do pedido %d: %v", p.OrderID, err)
			continue
		}
		o.logf("auction: cobrança do pedido %d gerada no retry", p.OrderID)
	}
	// Quem esgotou as tentativas: libera o produto em vez de deixar preso.
	exhausted, err := o.store.ExhaustedCharges(ctx, 20, o.maxChargeTries)
	if err != nil {
		return len(pending), err
	}
	for _, p := range exhausted {
		released, err := o.store.FailCharge(ctx, p.OrderID, p.LotID)
		if err != nil {
			o.logf("auction: desistir da cobrança do pedido %d: %v", p.OrderID, err)
			continue
		}
		if released {
			o.logf("auction: ALARME desisti de cobrar o pedido %d do lote %s após %d tentativas; produto de volta ao estoque",
				p.OrderID, p.LotID, o.maxChargeTries)
			o.onNotice(ctx, Notice{Kind: NoticeChargeFailed, LotID: p.LotID, JID: p.JID, Amount: p.Amount})
			o.onNotice(ctx, Notice{Kind: NoticeUnsold, LotID: p.LotID})
		}
	}
	return len(pending), nil
}

// Reconcile é a rede de proteção do fechamento: procura lotes que o
// Postgres ainda acha abertos, confere o estado real no Redis e conclui o
// fluxo se o lote já fechou lá. Cobre o caso de o processo morrer entre o
// fechamento no Redis e a escrita no Postgres — antes, o lote e o produto
// ficavam presos para sempre, sem cobrança e sem aviso.
func (o *Orchestrator) Reconcile(ctx context.Context, engine *Engine, olderThan time.Duration) (int, error) {
	ids, err := o.store.StaleOpenLots(ctx, olderThan, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		snap, err := engine.Get(ctx, id)
		if err != nil {
			// Chave sumiu do Redis (TTL): não dá pra saber o vencedor.
			// Deixa aberto e grita — decisão humana, nunca silêncio.
			o.logf("auction: ALARME lote %s aberto no postgres e ausente no redis; precisa de decisão manual: %v", id, err)
			o.onNotice(ctx, Notice{Kind: NoticeStaleLot, LotID: id, Detail: "ausente no redis"})
			continue
		}
		if snap.Status != "closed" {
			continue // ainda correndo de verdade
		}
		// Reivindica antes de concluir: sem isso, a fila e o reconciliador
		// podiam processar o mesmo lote e gerar dois links de cadastro.
		claimed, err := o.store.ClaimStaleLot(ctx, id)
		if err != nil {
			o.logf("auction: reivindicar lote %s: %v", id, err)
			continue
		}
		if !claimed {
			continue // outra réplica (ou a fila) chegou primeiro
		}
		o.logf("auction: reconciliando lote %s, fechado no redis mas aberto no postgres", id)
		o.onNotice(ctx, Notice{Kind: NoticeStaleLot, LotID: id, Detail: "fechamento reconciliado"})
		var cErr error
		if snap.Leader == "" {
			cErr = o.store.CloseNoBids(ctx, id)
		} else {
			cErr = o.onWinner(ctx, id, snap.Leader, snap.Current)
		}
		if cErr != nil {
			o.logf("auction: reconciliar lote %s: %v", id, cErr)
			// Devolve para 'open' para a próxima passada tentar de novo.
			if rErr := o.store.ReleaseStaleLotClaim(ctx, id); rErr != nil {
				o.logf("auction: devolver reivindicação do lote %s: %v", id, rErr)
			}
			continue
		}
		n++
	}
	return n, nil
}

// RunExpiryWorker chama as varreduras periódicas até o contexto ser
// cancelado. Rode em toda réplica; o SKIP LOCKED evita duplicidade.
// engine pode ser nil para desligar a reconciliação.
func (o *Orchestrator) RunExpiryWorker(ctx context.Context, engine *Engine, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-purge.C:
			if err := o.store.PurgeExpiredAuth(ctx, 7*24*time.Hour); err != nil {
				o.logf("auction: limpeza de sessões/códigos: %v", err)
			}
		case <-t.C:
			if _, err := o.ExpireDuePayments(ctx); err != nil {
				o.logf("auction: varredura de pagamentos vencidos: %v", err)
			}
			if _, err := o.ExpireDueRegistrations(ctx); err != nil {
				o.logf("auction: varredura de cadastros vencidos: %v", err)
			}
			if _, err := o.RetryFailedCharges(ctx); err != nil {
				o.logf("auction: retry de cobranças: %v", err)
			}
			if engine != nil {
				if _, err := o.Reconcile(ctx, engine, MaxDuration+time.Minute); err != nil {
					o.logf("auction: reconciliação de lotes: %v", err)
				}
			}
		}
	}
}

// SendText encaminha uma mensagem simples pelo Notifier configurado — usado
// pelo login por código da área do cliente, que não é um evento de leilão.
func (o *Orchestrator) SendText(ctx context.Context, jid, text string) error {
	return o.notify.SendText(ctx, jid, text)
}

func formatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%sR$ %d,%02d", sign, c/100, c%100)
}

func formatRemaining(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%dmin%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// shortJID mostra o número sem o sufixo do WhatsApp, com os dígitos do
// meio escondidos — o grupo inteiro lê essas mensagens.
func shortJID(jid string) string {
	n := jid
	for i := 0; i < len(n); i++ {
		if n[i] == '@' {
			n = n[:i]
			break
		}
	}
	if len(n) <= 6 {
		return n
	}
	return n[:4] + "****" + n[len(n)-2:]
}
