package auction

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

const testDSN = "postgresql://postgres:postgres@127.0.0.1:5432/auction_test?sslmode=disable"

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tbl := range []string{
		"payment_orders", "registration_tokens", "lot_defaults", "bids",
		"lots", "products", "participants", "shipping_zones", "app_settings",
		"admin_sessions", "admin_login_attempts", "admin_users",
	} {
		if _, err := db.Exec("TRUNCATE " + tbl + " RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	return db
}

func mkProduct(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO products (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func tokenForLot(t *testing.T, db *sql.DB, lotID string) string {
	t.Helper()
	var token string
	if err := db.QueryRow(`SELECT token FROM registration_tokens WHERE lot_id = $1`, lotID).Scan(&token); err != nil {
		t.Fatalf("token de cadastro não encontrado pro lote %s: %v", lotID, err)
	}
	return token
}

func sampleForm(cep string) CustomerForm {
	return CustomerForm{
		Name: "Fulano de Tal", Document: "12345678900", Email: "fulano@example.com",
		CEP: cep, Street: "Rua das Flores", Number: "100",
		Neighborhood: "Centro", City: "São Paulo", State: "SP",
	}
}

// mockPay implementa PaymentProvider sem bater em rede nenhuma.
type mockPay struct {
	mu      sync.Mutex
	seq     int64
	amounts map[string]int64 // chargeID -> amount cobrado
	fail    bool
}

func newMockPay() *mockPay { return &mockPay{amounts: map[string]int64{}} }

func (m *mockPay) CreateCharge(_ context.Context, reference string, amount int64, _ Customer) (ChargeResult, error) {
	if m.fail {
		return ChargeResult{}, fmt.Errorf("provedor indisponível (simulado)")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("charge-%d", m.seq)
	m.amounts[id] = amount
	return ChargeResult{ChargeID: id, PixCode: "00020126PIXQRCODE" + id}, nil
}

// mockNotify grava mensagens em vez de chamar o DigiGO de verdade.
type mockNotify struct {
	mu   sync.Mutex
	sent []string // "jid:texto-ou-legenda"
}

func (n *mockNotify) SendText(_ context.Context, jid, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, jid+":"+text)
	return nil
}
func (n *mockNotify) SendCharge(_ context.Context, jid, caption string, _ ChargeResult) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, jid+":"+caption)
	return nil
}
func (n *mockNotify) count(jid string) (c int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.sent {
		if len(s) >= len(jid) && s[:len(jid)] == jid {
			c++
		}
	}
	return
}

// mockShipping devolve um valor fixo, pra deixar as contas de teste previsíveis.
type mockShipping struct{ price int64 }

func (m mockShipping) Quote(context.Context, string) (int64, error) { return m.price, nil }

type noticeLog struct {
	mu sync.Mutex
	ev []Notice
}

func (l *noticeLog) add(_ context.Context, n Notice) {
	l.mu.Lock()
	l.ev = append(l.ev, n)
	l.mu.Unlock()
}
func (l *noticeLog) kinds() (ks []NoticeKind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.ev {
		ks = append(ks, e.Kind)
	}
	return
}

func newTestOrchestrator(t *testing.T, db *sql.DB, pay *mockPay, notify *mockNotify, notices *noticeLog) *Orchestrator {
	return NewOrchestrator(NewStore(db), pay, notify, mockShipping{price: 1000}, OrchestratorOptions{
		PaymentWindow:      300 * time.Millisecond, // curto pra testar expiração sem esperar minutos
		RegistrationWindow: 300 * time.Millisecond,
		ProviderName:       "mock",
		RegisterURL:        "https://example.test/cadastro/",
		OnNotice:           notices.add,
		Logf:               t.Logf,
	})
}

func TestFullFlow_RegistersThenPays(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	o := newTestOrchestrator(t, db, pay, notify, notices)
	go o.Run(ctx)

	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Miniatura F1 1:18")
	jid := "5511999990001@s.whatsapp.net"

	if _, err := o.CreateAuction(ctx, eng, "lot-1", prod, Config{
		StartPrice: 5000, MinIncrement: 500, Duration: 400 * time.Millisecond, ExtendTo: 100 * time.Millisecond,
	}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.PlaceBid(ctx, "lot-1", jid, 6000, false, "m1"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		var status string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-1'`).Scan(&status)
		return status == "awaiting_registration"
	})
	if !containsKind(notices.kinds(), NoticeAwaitingRegistration) {
		t.Fatal("esperava aviso de aguardando cadastro")
	}
	if notify.count(jid) == 0 {
		t.Fatal("vencedor não recebeu o link de cadastro")
	}

	token := tokenForLot(t, db, "lot-1")
	if err := o.CompleteRegistration(ctx, token, sampleForm("01310-000")); err != nil {
		t.Fatal(err)
	}

	var lotStatus string
	var shipping, discount int64
	db.QueryRow(`SELECT status, shipping_cents, discount_cents FROM lots WHERE id = 'lot-1'`).Scan(&lotStatus, &shipping, &discount)
	if lotStatus != "awaiting_payment" || shipping != 1000 || discount != 0 {
		t.Fatalf("esperado awaiting_payment/frete 1000/desconto 0, got %s/%d/%d", lotStatus, shipping, discount)
	}

	var chargeID string
	var amount int64
	var claimant sql.NullInt64
	db.QueryRow(`SELECT charge_id, amount FROM payment_orders WHERE lot_id = 'lot-1'`).Scan(&chargeID, &amount)
	db.QueryRow(`SELECT claimant_id FROM lots WHERE id = 'lot-1'`).Scan(&claimant)
	if amount != 7000 { // 6000 do lance + 1000 de frete, sem desconto (1º item do dia)
		t.Fatalf("total cobrado errado: %d", amount)
	}

	if err := o.OnPaymentWebhook(ctx, chargeID); err != nil {
		t.Fatal(err)
	}
	// webhook duplicado não pode quebrar nem reprocessar
	if err := o.OnPaymentWebhook(ctx, chargeID); err != nil {
		t.Fatal(err)
	}

	var finalStatus, prodStatus string
	var owner int64
	db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-1'`).Scan(&finalStatus)
	db.QueryRow(`SELECT status, owner_id FROM products WHERE id = $1`, prod).Scan(&prodStatus, &owner)
	if finalStatus != "paid" || prodStatus != "sold" || owner != claimant.Int64 {
		t.Fatalf("esperado pago/vendido/dono=%d, got lot=%s prod=%s owner=%d", claimant.Int64, finalStatus, prodStatus, owner)
	}
	if !containsKind(notices.kinds(), NoticePaid) {
		t.Fatal("esperava aviso de pago")
	}
}

func TestSecondItemSameDay_GetsFreeShipping(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	o := newTestOrchestrator(t, db, pay, notify, notices)
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	jid := "cliente-fiel@s.whatsapp.net"

	// primeiro lote: paga frete cheio
	prod1 := mkProduct(t, db, "Item 1")
	o.CreateAuction(ctx, eng, "lot-a", prod1, Config{StartPrice: 1000, MinIncrement: 100, Duration: 200 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-a", jid, 1000, false, "m1")
	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-a'`).Scan(&s)
		return s == "awaiting_registration"
	})
	o.CompleteRegistration(ctx, tokenForLot(t, db, "lot-a"), sampleForm("01310-000"))
	var charge1 string
	db.QueryRow(`SELECT charge_id FROM payment_orders WHERE lot_id = 'lot-a'`).Scan(&charge1)
	o.OnPaymentWebhook(ctx, charge1)

	// segundo lote no mesmo dia: já está cadastrado (não pede endereço de
	// novo) e o frete deve sair de graça.
	prod2 := mkProduct(t, db, "Item 2")
	o.CreateAuction(ctx, eng, "lot-b", prod2, Config{StartPrice: 1000, MinIncrement: 100, Duration: 200 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-b", jid, 2000, false, "m2")
	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-b'`).Scan(&s)
		return s == "awaiting_payment"
	})
	var shipping, discount, amount int64
	db.QueryRow(`SELECT shipping_cents, discount_cents FROM lots WHERE id = 'lot-b'`).Scan(&shipping, &discount)
	db.QueryRow(`SELECT amount FROM payment_orders WHERE lot_id = 'lot-b'`).Scan(&amount)
	if shipping != 1000 || discount != 1000 || amount != 2000 {
		t.Fatalf("esperava frete 1000 totalmente descontado (total = só o lance 2000), got frete=%d desconto=%d total=%d",
			shipping, discount, amount)
	}
}

func TestRegistrationExpires_ReturnsToStock(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	o := newTestOrchestrator(t, db, pay, notify, notices)
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go o.RunExpiryWorker(ctx, eng, 30*time.Millisecond)
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Camisa autografada")

	o.CreateAuction(ctx, eng, "lot-2", prod, Config{
		StartPrice: 1000, MinIncrement: 100, Duration: 150 * time.Millisecond, ExtendTo: 20 * time.Millisecond,
	}, 0, "")
	eng.PlaceBid(ctx, "lot-2", "sumido@s.whatsapp.net", 1500, false, "m1")

	// nunca completa o cadastro -> token expira -> produto solto
	waitFor(t, 3*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-2'`).Scan(&s)
		return s == "unsold"
	})
	var pstatus string
	db.QueryRow(`SELECT status FROM products WHERE id = $1`, prod).Scan(&pstatus)
	if pstatus != "available" {
		t.Fatalf("produto deveria voltar available, está %s", pstatus)
	}
	ks := notices.kinds()
	if !containsKind(ks, NoticeRegistrationExpired) || !containsKind(ks, NoticeUnsold) {
		t.Fatalf("esperava avisos de cadastro expirado + não vendido, got %v", ks)
	}
	var pendingOrders int
	db.QueryRow(`SELECT count(*) FROM payment_orders WHERE lot_id = 'lot-2'`).Scan(&pendingOrders)
	if pendingOrders != 0 {
		t.Fatal("não deveria ter chegado a criar cobrança sem cadastro completo")
	}
}

func TestPaymentExpiresAfterRegistration_ReturnsToStock(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	o := newTestOrchestrator(t, db, pay, notify, notices)
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go o.RunExpiryWorker(ctx, eng, 30*time.Millisecond)
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Boné edição limitada")

	o.CreateAuction(ctx, eng, "lot-3", prod, Config{StartPrice: 500, MinIncrement: 100, Duration: 150 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-3", "registra-mas-nao-paga@s.whatsapp.net", 800, false, "m1")

	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-3'`).Scan(&s)
		return s == "awaiting_registration"
	})
	if err := o.CompleteRegistration(ctx, tokenForLot(t, db, "lot-3"), sampleForm("20010-000")); err != nil {
		t.Fatal(err)
	}
	// registrou, cobrança foi gerada, mas nunca paga -> expira -> solta produto
	waitFor(t, 3*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM products WHERE id = $1`, prod).Scan(&s)
		return s == "available"
	})
	var lotStatus string
	db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-3'`).Scan(&lotStatus)
	if lotStatus != "unsold" {
		t.Fatalf("lote deveria estar unsold, está %s", lotStatus)
	}
	if !containsKind(notices.kinds(), NoticeUnsold) {
		t.Fatal("esperava aviso de não vendido")
	}
}

func TestNoBids_ClosesCleanly(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Sem interessados")
	o.CreateAuction(ctx, eng, "lot-4", prod, Config{StartPrice: 100, MinIncrement: 10, Duration: 100 * time.Millisecond}, 0, "")

	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-4'`).Scan(&s)
		return s == "unsold"
	})
	var pstatus string
	db.QueryRow(`SELECT status FROM products WHERE id = $1`, prod).Scan(&pstatus)
	if pstatus != "available" {
		t.Fatalf("produto deveria voltar a available, está %s", pstatus)
	}
}

// Duas réplicas do worker de expiração rodando ao mesmo tempo não podem
// processar o mesmo pedido de pagamento vencido duas vezes.
func TestExpiryWorker_NoDoubleProcessingAcrossReplicas(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := testDB(t)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	o1 := newTestOrchestrator(t, db, pay, notify, notices)
	o2 := NewOrchestrator(NewStore(db), pay, notify, mockShipping{price: 1000}, OrchestratorOptions{
		PaymentWindow: 300 * time.Millisecond, RegistrationWindow: 300 * time.Millisecond,
		OnNotice: notices.add, Logf: t.Logf,
	})
	go o1.Run(ctx)
	go o2.Run(ctx)

	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o1.HandleEvent})
	go o1.RunExpiryWorker(ctx, eng, 20*time.Millisecond)
	go o2.RunExpiryWorker(ctx, eng, 20*time.Millisecond)
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Peça rara")
	o1.CreateAuction(ctx, eng, "lot-5", prod, Config{StartPrice: 100, MinIncrement: 10, Duration: 100 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-5", "a@s.whatsapp.net", 200, false, "m1")

	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-5'`).Scan(&s)
		return s == "awaiting_registration"
	})
	if err := o1.CompleteRegistration(ctx, tokenForLot(t, db, "lot-5"), sampleForm("30130-000")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-5'`).Scan(&s)
		return s == "unsold"
	})
	var expired int
	db.QueryRow(`SELECT count(*) FROM payment_orders WHERE lot_id = 'lot-5' AND status = 'expired'`).Scan(&expired)
	if expired != 1 {
		t.Fatalf("deveria ter exatamente 1 pedido expirado (sem duplicidade entre réplicas), total=%d", expired)
	}
	nDefault, nUnsold := 0, 0
	for _, k := range notices.kinds() {
		if k == NoticeDefaulted {
			nDefault++
		}
		if k == NoticeUnsold {
			nUnsold++
		}
	}
	if nDefault != 1 || nUnsold != 1 {
		t.Fatalf("cada aviso deveria disparar 1 vez só, got defaulted=%d unsold=%d", nDefault, nUnsold)
	}
}

func TestProviderDown_KeepsOrderPendingForRetry(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay := newMockPay()
	o := newTestOrchestrator(t, db, pay, &mockNotify{}, &noticeLog{})
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	prod := mkProduct(t, db, "Item com provedor fora do ar")
	o.CreateAuction(ctx, eng, "lot-6", prod, Config{StartPrice: 100, MinIncrement: 10, Duration: 100 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-6", "x@s.whatsapp.net", 200, false, "m1")

	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id = 'lot-6'`).Scan(&s)
		return s == "awaiting_registration"
	})
	pay.fail = true // simula o provedor caindo bem na hora de gerar a cobrança
	if err := o.CompleteRegistration(ctx, tokenForLot(t, db, "lot-6"), sampleForm("40010-000")); err == nil {
		t.Fatal("esperava erro do provedor")
	}
	var n int
	var status string
	var charge sql.NullString
	db.QueryRow(`SELECT count(*) FROM payment_orders WHERE lot_id = 'lot-6'`).Scan(&n)
	db.QueryRow(`SELECT status, charge_id FROM payment_orders WHERE lot_id = 'lot-6'`).Scan(&status, &charge)
	if n != 1 || status != "pending" || charge.Valid {
		t.Fatalf("pedido deveria existir pendente sem charge_id pra retry, got n=%d status=%s charge=%v", n, status, charge)
	}
}

func containsKind(ks []NoticeKind, want NoticeKind) bool {
	for _, k := range ks {
		if k == want {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condição não satisfeita a tempo")
}
