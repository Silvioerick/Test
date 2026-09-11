package auction

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Cada teste aqui corresponde a um defeito encontrado na revisão. Antes da
// correção eles falhavam; são a rede que impede a volta de cada um.

// --- 1. Webhook de pagamento sem assinatura não pode pagar nada ----------

func TestRegression_WebhookSemAssinaturaNaoPaga(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	api := NewAPIServer(db, eng, o, "admin").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	prod := mkProduct(t, db, "iPhone")
	if _, err := o.CreateAuction(ctx, eng, "lot-sec", prod, Config{
		StartPrice: 100000, MinIncrement: 1000,
		Duration: 100 * time.Millisecond, ExtendTo: 20 * time.Millisecond,
	}, 0, ""); err != nil {
		t.Fatal(err)
	}
	eng.PlaceBid(ctx, "lot-sec", "golpista@s.whatsapp.net", 150000, false, "m1")
	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id='lot-sec'`).Scan(&s)
		return s == "awaiting_registration"
	})
	if err := o.CompleteRegistration(ctx, tokenForLot(t, db, "lot-sec"), sampleForm("01310-000")); err != nil {
		t.Fatal(err)
	}
	var chargeID string
	db.QueryRow(`SELECT charge_id FROM payment_orders WHERE lot_id='lot-sec'`).Scan(&chargeID)

	// Ataque: o próprio comprador conhece o charge_id (vem na URL do
	// checkout) e tenta confirmar o pagamento sem pagar.
	body, _ := json.Marshal(map[string]string{"charge_id": chargeID, "status": "paid"})
	resp, err := http.Post(srv.URL+"/api/webhooks/hubpay", "application/json", bytesReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("webhook sem assinatura devia dar 401, deu %d", resp.StatusCode)
	}
	var lotStatus, prodStatus string
	db.QueryRow(`SELECT status FROM lots WHERE id='lot-sec'`).Scan(&lotStatus)
	db.QueryRow(`SELECT status FROM products WHERE id=$1`, prod).Scan(&prodStatus)
	if lotStatus == "paid" || prodStatus == "sold" {
		t.Fatalf("produto saiu de graça: lote=%s produto=%s", lotStatus, prodStatus)
	}

	// Com assinatura válida, o mesmo webhook funciona.
	if st, out := postHubPay(t, srv, map[string]any{"charge_id": chargeID, "status": "paid"}); st != http.StatusOK {
		t.Fatalf("webhook assinado devia passar: %d %v", st, out)
	}
	db.QueryRow(`SELECT status FROM lots WHERE id='lot-sec'`).Scan(&lotStatus)
	if lotStatus != "paid" {
		t.Fatalf("webhook assinado não marcou como pago: %s", lotStatus)
	}
}

func TestRegression_WebhookAsaasExigeToken(t *testing.T) {
	db := testDB(t)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(map[string]any{
		"event": "PAYMENT_RECEIVED", "payment": map[string]string{"id": "pay_1"},
	})
	resp, err := http.Post(srv.URL+"/api/webhooks/asaas", "application/json", bytesReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("webhook asaas sem token devia dar 401, deu %d", resp.StatusCode)
	}
}

// Falha fechada: sem segredo configurado o webhook é recusado, nunca aceito.
func TestRegression_WebhookSemSegredoConfiguradoFalhaFechado(t *testing.T) {
	db := testDB(t)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin") // sem WithWebhookAuth
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(map[string]string{"charge_id": "x", "status": "paid"})
	resp, err := http.Post(srv.URL+"/api/webhooks/hubpay", "application/json", bytesReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("sem segredo o webhook devia dar 503, deu %d", resp.StatusCode)
	}
}

// --- 2. Valor cobrado é o do Redis, não o que sobrou no Postgres --------

func TestRegression_EventoDeLanceDescartadoNaoBaixaOValorCobrado(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := NewOrchestrator(NewStore(db), newMockPay(), &mockNotify{}, mockShipping{price: 1000},
		OrchestratorOptions{
			PaymentWindow: 10 * time.Second, RegistrationWindow: 10 * time.Second,
			ProviderName: "mock", QueueSize: 1, Logf: t.Logf,
		})
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	prod := mkProduct(t, db, "Notebook")
	if _, err := o.CreateAuction(ctx, eng, "lot-drop", prod, Config{
		StartPrice: 100000, MinIncrement: 1000,
		Duration: 1500 * time.Millisecond, ExtendTo: 100 * time.Millisecond,
	}, 0, ""); err != nil {
		t.Fatal(err)
	}
	// Fila de tamanho 1 e ninguém consumindo: o lance vencedor é descartado.
	eng.PlaceBid(ctx, "lot-drop", "a@s.whatsapp.net", 100000, false, "m1")
	eng.PlaceBid(ctx, "lot-drop", "b@s.whatsapp.net", 500000, false, "m2")
	go o.Run(ctx)
	go eng.Run(ctx)

	waitFor(t, 8*time.Second, func() bool {
		var n int
		db.QueryRow(`SELECT count(*) FROM registration_tokens WHERE lot_id='lot-drop'`).Scan(&n)
		return n == 1
	})
	if err := o.CompleteRegistration(ctx, tokenForLot(t, db, "lot-drop"), sampleForm("01310-000")); err != nil {
		t.Fatal(err)
	}
	var cobrado int64
	db.QueryRow(`SELECT amount FROM payment_orders WHERE lot_id='lot-drop'`).Scan(&cobrado)
	snap, _ := eng.Get(ctx, "lot-drop")
	if want := snap.Current + 1000; cobrado != want {
		t.Fatalf("cobrado %d, esperado %d (lance vencedor %d + frete 1000)", cobrado, want, snap.Current)
	}
}

// --- 3. Fechamento não se perde; e se perder, o reconciliador resolve ---

func TestRegression_FechamentoNaoEhDescartado(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := NewOrchestrator(NewStore(db), newMockPay(), &mockNotify{}, mockShipping{price: 1000},
		OrchestratorOptions{
			PaymentWindow: 10 * time.Second, RegistrationWindow: 10 * time.Second,
			ProviderName: "mock", QueueSize: 1, Logf: t.Logf,
		})
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	prod := mkProduct(t, db, "TV")
	if _, err := o.CreateAuction(ctx, eng, "lot-close", prod, Config{
		StartPrice: 100000, MinIncrement: 1000,
		Duration: 800 * time.Millisecond, ExtendTo: 100 * time.Millisecond,
	}, 0, ""); err != nil {
		t.Fatal(err)
	}
	eng.PlaceBid(ctx, "lot-close", "a@s.whatsapp.net", 100000, false, "m1")
	go eng.Run(ctx)
	// Consumidor só entra depois: o fechamento tem de ESPERAR na fila em
	// vez de ser descartado como era antes.
	time.Sleep(1500 * time.Millisecond)
	go o.Run(ctx)

	waitFor(t, 8*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id='lot-close'`).Scan(&s)
		return s == "awaiting_registration"
	})
}

// Rede de proteção: mesmo perdendo o fechamento, o reconciliador conclui.
func TestRegression_ReconciliadorRecuperaLoteTravado(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notices := &noticeLog{}
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, notices)
	rdb := setup(t)
	// OnEvent vazio: simula o fechamento perdido de vez.
	eng := New(rdb, Options{OnEvent: func(context.Context, Event) {}})
	prod := mkProduct(t, db, "Console")
	if _, err := o.CreateAuction(ctx, eng, "lot-stuck", prod, Config{
		StartPrice: 100000, MinIncrement: 1000,
		Duration: 300 * time.Millisecond, ExtendTo: 50 * time.Millisecond,
	}, 0, ""); err != nil {
		t.Fatal(err)
	}
	eng.PlaceBid(ctx, "lot-stuck", "a@s.whatsapp.net", 250000, false, "m1")
	go eng.Run(ctx)
	waitFor(t, 3*time.Second, func() bool {
		snap, err := eng.Get(ctx, "lot-stuck")
		return err == nil && snap.Status == "closed"
	})
	var lotStatus string
	db.QueryRow(`SELECT status FROM lots WHERE id='lot-stuck'`).Scan(&lotStatus)
	if lotStatus != "open" {
		t.Fatalf("pré-condição: o lote deveria ter ficado preso em open, está %s", lotStatus)
	}

	n, err := o.Reconcile(ctx, eng, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reconciliador deveria ter resolvido 1 lote, resolveu %d", n)
	}
	db.QueryRow(`SELECT status FROM lots WHERE id='lot-stuck'`).Scan(&lotStatus)
	if lotStatus != "awaiting_registration" {
		t.Fatalf("lote não foi concluído pela reconciliação: %s", lotStatus)
	}
	// E o valor gravado é o do Redis, não zero.
	var amount int64
	db.QueryRow(`SELECT current_amount FROM lots WHERE id='lot-stuck'`).Scan(&amount)
	if amount != 250000 {
		t.Fatalf("valor vencedor reconciliado errado: %d", amount)
	}
}

// --- 4. Idempotência de verdade no RecordBid ----------------------------

func TestRegression_ReentregaNaoInflaContador(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	prod := mkProduct(t, db, "Item")
	if err := st.CreateLot(ctx, "lot-dup", prod, Config{StartPrice: 100, MinIncrement: 10}, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.RecordBid(ctx, "lot-dup", "a@s.whatsapp.net", "", 500, "mesma-msg"); err != nil {
			t.Fatal(err)
		}
	}
	var nBids, bidCount int
	db.QueryRow(`SELECT count(*) FROM bids WHERE lot_id='lot-dup'`).Scan(&nBids)
	db.QueryRow(`SELECT bid_count FROM lots WHERE id='lot-dup'`).Scan(&bidCount)
	if nBids != 1 || bidCount != 1 {
		t.Fatalf("reentrega contabilizada: bids=%d bid_count=%d (esperado 1 e 1)", nBids, bidCount)
	}
	// E o valor nunca anda para trás.
	if err := st.RecordBid(ctx, "lot-dup", "b@s.whatsapp.net", "", 200, "outra-msg"); err != nil {
		t.Fatal(err)
	}
	var cur int64
	db.QueryRow(`SELECT current_amount FROM lots WHERE id='lot-dup'`).Scan(&cur)
	if cur != 500 {
		t.Fatalf("current_amount regrediu para %d", cur)
	}
}

// --- 5. Pagamento que chega antes do charge_id pede reentrega -----------

func TestRegression_WebhookAntesDoAttachPedeReentrega(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	prod := mkProduct(t, db, "Item")
	st.CreateLot(ctx, "lot-race", prod, Config{StartPrice: 100, MinIncrement: 10}, time.Minute, "")
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "")
	orderID, _, err := st.AssignClaimant(ctx, "lot-race", info.ID, 1000, 100, 0)
	if err != nil {
		t.Fatal(err)
	}

	// PIX confirmado antes de o charge_id ter sido gravado.
	status, _ := postHubPay(t, srv, map[string]any{"charge_id": "ch-atrasado", "status": "paid"})
	if status != http.StatusConflict {
		t.Fatalf("esperava 409 para o provedor reenviar, got %d", status)
	}

	// O provedor reenvia depois que o attach acontece — aí sim é aceito.
	if err := st.AttachCharge(ctx, orderID, "mock", ChargeResult{ChargeID: "ch-atrasado"}); err != nil {
		t.Fatal(err)
	}
	if status, out := postHubPay(t, srv, map[string]any{"charge_id": "ch-atrasado", "status": "paid"}); status != http.StatusOK {
		t.Fatalf("reentrega devia ser aceita: %d %v", status, out)
	}
	var s string
	db.QueryRow(`SELECT status FROM payment_orders WHERE id=$1`, orderID).Scan(&s)
	if s != "paid" {
		t.Fatalf("pagamento perdido: pedido está %s", s)
	}
	// E uma terceira entrega do mesmo webhook é idempotente.
	if status, _ := postHubPay(t, srv, map[string]any{"charge_id": "ch-atrasado", "status": "paid"}); status != http.StatusOK {
		t.Fatalf("reentrega idempotente devia dar 200, got %d", status)
	}
}

// --- 6. Provedor fora do ar não gera caloteiro --------------------------

func TestRegression_ProvedorForaDoArNaoViraCalote(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	notices := &noticeLog{}
	pay := newMockPay()
	o := newTestOrchestrator(t, db, pay, &mockNotify{}, notices)
	st := NewStore(db)
	prod := mkProduct(t, db, "Item")
	st.CreateLot(ctx, "lot-down", prod, Config{StartPrice: 100, MinIncrement: 10}, 200*time.Millisecond, "")
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "")
	// Pedido criado sem cobrança: provedor estava fora do ar.
	st.AssignClaimant(ctx, "lot-down", info.ID, 1000, 100, 0)
	time.Sleep(400 * time.Millisecond)
	if _, err := o.ExpireDuePayments(ctx); err != nil {
		t.Fatal(err)
	}
	var nDefaults int
	db.QueryRow(`SELECT count(*) FROM lot_defaults WHERE lot_id='lot-down'`).Scan(&nDefaults)
	if nDefaults != 0 {
		t.Fatalf("cliente marcado como caloteiro sem nunca ter recebido cobrança (%d calotes)", nDefaults)
	}
	if !containsKind(notices.kinds(), NoticeChargeFailed) {
		t.Fatalf("faltou o alarme de falha de cobrança: %v", notices.kinds())
	}
}

// O retry gera a cobrança que faltava e reinicia o prazo do cliente.
func TestRegression_RetryGeraCobrancaQueFaltava(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pay := newMockPay()
	notify := &mockNotify{}
	o := newTestOrchestrator(t, db, pay, notify, &noticeLog{})
	st := NewStore(db)
	prod := mkProduct(t, db, "Item")
	st.CreateLot(ctx, "lot-retry", prod, Config{StartPrice: 100, MinIncrement: 10}, time.Hour, "")
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "Fulano")
	orderID, _, err := st.AssignClaimant(ctx, "lot-retry", info.ID, 1000, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.RecordChargeFailure(ctx, orderID, "provedor fora do ar (simulado)")

	if _, err := o.RetryFailedCharges(ctx); err != nil {
		t.Fatal(err)
	}
	var charge sql.NullString
	db.QueryRow(`SELECT charge_id FROM payment_orders WHERE id=$1`, orderID).Scan(&charge)
	if !charge.Valid {
		t.Fatal("o retry não gerou a cobrança")
	}
	if notify.count("a@s.whatsapp.net") == 0 {
		t.Fatal("cliente não recebeu a cobrança gerada no retry")
	}
}

// --- 7. Asaas não duplica cliente --------------------------------------

func TestRegression_AsaasReusaCliente(t *testing.T) {
	var created, lookups int32
	mux := http.NewServeMux()
	seen := map[string]string{}
	mux.HandleFunc("/v3/customers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&lookups, 1)
			ref := r.URL.Query().Get("externalReference")
			if id, ok := seen[ref]; ok {
				json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": id}}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		n := atomic.AddInt32(&created, 1)
		id := fmt.Sprintf("cus_%d", n)
		seen[fmt.Sprint(in["externalReference"])] = id
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("/v3/payments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"id": "pay_1", "invoiceUrl": "https://x"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewAsaasProvider(srv.URL, "k")
	c := Customer{Name: "Fulano", Document: "12345678900", Phone: "5511999999999@s.whatsapp.net"}
	if _, err := p.CreateCharge(context.Background(), "lot-a-order-1", 1000, c); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateCharge(context.Background(), "lot-b-order-2", 2000, c); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("a mesma pessoa virou %d clientes na Asaas", created)
	}
}

func TestRegression_AsaasValorSemErroDePontoFlutuante(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		want  float64
	}{{1234567, 12345.67}, {1, 0.01}, {100, 1.0}, {999999999, 9999999.99}} {
		if got := centsToReais(tc.cents); got != tc.want {
			t.Errorf("centsToReais(%d) = %v, esperado %v", tc.cents, got, tc.want)
		}
	}
}

// --- 8. Um produto, um leilão -------------------------------------------

func TestRegression_ProdutoNaoEntraEmDoisLeiloes(t *testing.T) {
	db := testDB(t)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	api := NewAPIServer(db, eng, o, "admin")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	prod := mkProduct(t, db, "Item único")

	body := map[string]any{
		"product_id": prod, "start_price_cents": 1000, "min_increment_cents": 100,
		"duration_seconds": 60, "extend_seconds": 5,
	}
	if st, _ := doJSON(t, srv, "POST", "/api/lots", "admin", body); st != http.StatusCreated {
		t.Fatalf("primeiro leilão devia ser criado, got %d", st)
	}
	if st, _ := doJSON(t, srv, "POST", "/api/lots", "admin", body); st != http.StatusConflict {
		t.Fatalf("segundo leilão do mesmo produto devia dar 409, got %d", st)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM lots WHERE product_id=$1 AND status='open'`, prod).Scan(&n)
	if n != 1 {
		t.Fatalf("%d leilões abertos para o mesmo produto físico", n)
	}
}

// --- 9. Painel sem token não abre ---------------------------------------

func TestRegression_AdminTokenVazioFalhaFechado(t *testing.T) {
	db := testDB(t)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "") // esqueceram o token
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/customers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("base de clientes (CPF, endereço) ficou pública com ADMIN_TOKEN vazio")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("esperava 503, got %d", resp.StatusCode)
	}
}

// A checagem de rota pública enxerga o caminho já normalizado.
func TestRegression_TravessiaDeCaminhoNaoViraRotaPublica(t *testing.T) {
	db := testDB(t)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "segredo")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/api/customer/../customers", "/api/public/registration/../../customers"} {
		resp, err := cli.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s devolveu 200 sem token", p)
		}
	}
}

// --- 10. Validação de configuração do lote ------------------------------

func TestRegression_ExtendToExplicitoMaiorQueDuracaoEhErro(t *testing.T) {
	c := Config{StartPrice: 100, MinIncrement: 10, Duration: 5 * time.Second, ExtendTo: 15 * time.Second}
	if err := c.normalize(); err == nil {
		t.Fatal("ExtendTo maior que a duração deveria ser recusado")
	}
	// O padrão, porém, se ajusta a lotes curtos em vez de quebrar.
	d := Config{StartPrice: 100, MinIncrement: 10, Duration: 5 * time.Second}
	if err := d.normalize(); err != nil {
		t.Fatalf("duração curta com ExtendTo padrão deveria funcionar: %v", err)
	}
	if d.ExtendTo > d.Duration {
		t.Fatalf("ExtendTo padrão não foi ajustado: %s > %s", d.ExtendTo, d.Duration)
	}
}

// --- 11. Teto de lance barra o troll ------------------------------------

func TestRegression_TetoDeLanceRecusaValorAbsurdo(t *testing.T) {
	ctx := context.Background()
	rdb := setup(t)
	e := New(rdb, Options{})
	if _, err := e.Create(ctx, "lot-cap", Config{
		StartPrice: 10000, MinIncrement: 500, Duration: 30 * time.Second, MaxBid: 100000,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.PlaceBid(ctx, "lot-cap", "troll@s.whatsapp.net", 500000000000, false, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != TooHigh {
		t.Fatalf("lance de R$5 bilhões deveria ser recusado, got %s", res.Status)
	}
	if res.Amount != 100000 {
		t.Fatalf("resposta deveria informar o teto, got %d", res.Amount)
	}
	// Abaixo do teto continua valendo.
	res, err = e.PlaceBid(ctx, "lot-cap", "serio@s.whatsapp.net", 20000, false, "m2")
	if err != nil || res.Status != Accepted {
		t.Fatalf("lance normal recusado: %s %v", res.Status, err)
	}
}

// --- 12. Caloteiro reincidente é barrado --------------------------------

func TestRegression_CaloteiroEhBarrado(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	st := NewStore(db)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	prod := mkProduct(t, db, "Item")
	if _, err := o.CreateAuction(ctx, eng, "lot-black", prod, Config{
		StartPrice: 1000, MinIncrement: 100, Duration: 30 * time.Second,
	}, 0, "grupo@g.us"); err != nil {
		t.Fatal(err)
	}
	caloteiro := "mau@s.whatsapp.net"
	info, _ := st.GetOrCreateParticipant(ctx, caloteiro, "")
	db.Exec(`INSERT INTO lot_defaults (lot_id, participant_id) VALUES ('lot-black', $1)`, info.ID)

	out, err := o.HandleInbound(ctx, eng, InboundMessage{
		MsgID: "m1", From: caloteiro, Channel: "grupo@g.us", Text: "50,00",
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Status == Accepted {
		t.Fatal("lance de caloteiro foi aceito")
	}
	var n int64
	db.QueryRow(`SELECT bid_count FROM lots WHERE id='lot-black'`).Scan(&n)
	if n != 0 {
		t.Fatalf("lance de caloteiro foi contabilizado: %d", n)
	}
}

// --- 13. A ponta do WhatsApp existe e funciona --------------------------

func TestRegression_MensagemDeWhatsAppViraLance(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notify := &mockNotify{}
	o := newTestOrchestrator(t, db, newMockPay(), notify, &noticeLog{})
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	go eng.Run(ctx)
	api := NewAPIServer(db, eng, o, "admin").WithWhatsApp("segredo-wa", 0)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	prod := mkProduct(t, db, "Relógio")
	if _, err := o.CreateAuction(ctx, eng, "lot-wa", prod, Config{
		StartPrice: 5000, MinIncrement: 500, Duration: 30 * time.Second,
	}, 0, "grupo-leilao@g.us"); err != nil {
		t.Fatal(err)
	}

	post := func(token, from, text, id string) int {
		body, _ := json.Marshal(map[string]any{
			"id": id, "participant": from, "chat": "grupo-leilao@g.us", "text": text,
		})
		req, _ := http.NewRequest("POST", srv.URL+"/api/webhooks/whatsapp", bytesReader(body))
		req.Header.Set("X-Webhook-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if st := post("errado", "a@s.whatsapp.net", "60,00", "m0"); st != http.StatusUnauthorized {
		t.Fatalf("webhook do whatsapp sem segredo válido devia dar 401, got %d", st)
	}
	if st := post("segredo-wa", "a@s.whatsapp.net", "60,00", "m1"); st != http.StatusOK {
		t.Fatalf("mensagem válida devia ser aceita, got %d", st)
	}
	snap, err := eng.Get(ctx, "lot-wa")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Current != 6000 || snap.Leader != "a@s.whatsapp.net" {
		t.Fatalf("lance não registrou: current=%d leader=%s", snap.Current, snap.Leader)
	}
	// "+" cobre o mínimo seguinte.
	if st := post("segredo-wa", "b@s.whatsapp.net", "+", "m2"); st != http.StatusOK {
		t.Fatalf("lance com + devia ser aceito, got %d", st)
	}
	snap, _ = eng.Get(ctx, "lot-wa")
	if snap.Current != 6500 || snap.Leader != "b@s.whatsapp.net" {
		t.Fatalf("lance + errado: current=%d leader=%s", snap.Current, snap.Leader)
	}
	// Reentrega da mesma mensagem não vira lance novo.
	post("segredo-wa", "b@s.whatsapp.net", "+", "m2")
	snap, _ = eng.Get(ctx, "lot-wa")
	if snap.Bids != 2 {
		t.Fatalf("reentrega virou lance novo: %d lances", snap.Bids)
	}
	// Conversa normal do grupo é ignorada.
	if st := post("segredo-wa", "c@s.whatsapp.net", "boa noite pessoal", "m3"); st != http.StatusOK {
		t.Fatalf("mensagem comum devia ser ignorada com 200, got %d", st)
	}
	snap, _ = eng.Get(ctx, "lot-wa")
	if snap.Bids != 2 {
		t.Fatalf("conversa normal virou lance: %d lances", snap.Bids)
	}
}

// --- 14. Logout revoga a sessão -----------------------------------------

func TestRegression_LogoutRevogaSessao(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "Fulano")
	token, err := st.CreateSession(ctx, info.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := doJSON(t, srv, "GET", "/api/customer/me", token, nil); st != http.StatusOK {
		t.Fatalf("sessão válida devia funcionar, got %d", st)
	}
	if st, _ := doJSON(t, srv, "POST", "/api/customer/logout", token, nil); st != http.StatusOK {
		t.Fatalf("logout devia funcionar, got %d", st)
	}
	if st, _ := doJSON(t, srv, "GET", "/api/customer/me", token, nil); st != http.StatusUnauthorized {
		t.Fatalf("sessão revogada continuou valendo: %d", st)
	}
}

// --- 15. OTP uniforme ---------------------------------------------------

func TestRegression_CodigoDeLoginSemViesDeModulo(t *testing.T) {
	counts := map[rune]int{}
	const n = 4000
	for i := 0; i < n; i++ {
		c, err := randomDigits(6)
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 6 {
			t.Fatalf("código com %d dígitos", len(c))
		}
		for _, r := range c {
			counts[r]++
		}
	}
	total := n * 6
	expected := float64(total) / 10
	for d := '0'; d <= '9'; d++ {
		got := float64(counts[d])
		if got < expected*0.88 || got > expected*1.12 {
			t.Errorf("dígito %c saiu %v vezes, esperado ~%v — distribuição enviesada", d, got, expected)
		}
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// --- 16. Retry não desiste antes da hora --------------------------------

func TestRegression_RetryNaoDesisteDePedidoComTentativasSobrando(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	pay := newMockPay()
	pay.fail = true // provedor continua fora do ar
	o := newTestOrchestrator(t, db, pay, &mockNotify{}, &noticeLog{})
	st := NewStore(db)
	prod := mkProduct(t, db, "Item")
	st.CreateLot(ctx, "lot-keep", prod, Config{StartPrice: 100, MinIncrement: 10}, time.Hour, "")
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "Fulano")
	orderID, _, err := st.AssignClaimant(ctx, "lot-keep", info.ID, 1000, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	st.RecordChargeFailure(ctx, orderID, "1a falha")

	// Uma passada do worker: tenta de novo e falha, mas NÃO pode desistir
	// (o padrão são 5 tentativas).
	if _, err := o.RetryFailedCharges(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	db.QueryRow(`SELECT status, charge_attempts FROM payment_orders WHERE id=$1`, orderID).Scan(&status, &attempts)
	if status != "pending" {
		t.Fatalf("pedido com tentativas sobrando virou %s", status)
	}
	if attempts < 2 {
		t.Fatalf("a tentativa não foi contabilizada: %d", attempts)
	}

	// Esgotando as tentativas, aí sim desiste — e como 'failed', não calote.
	db.Exec(`UPDATE payment_orders SET charge_attempts = 5 WHERE id=$1`, orderID)
	if _, err := o.RetryFailedCharges(ctx); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT status FROM payment_orders WHERE id=$1`, orderID).Scan(&status)
	if status != "failed" {
		t.Fatalf("depois de esgotar as tentativas o pedido devia virar failed, está %s", status)
	}
	var nDefaults, prodAvailable int
	db.QueryRow(`SELECT count(*) FROM lot_defaults WHERE lot_id='lot-keep'`).Scan(&nDefaults)
	db.QueryRow(`SELECT count(*) FROM products WHERE id=$1 AND status='available'`, prod).Scan(&prodAvailable)
	if nDefaults != 0 {
		t.Errorf("falha nossa virou calote do cliente")
	}
	if prodAvailable != 1 {
		t.Errorf("produto não voltou pro estoque")
	}
}

// --- 17. Avisos no grupo não inundam a conversa -------------------------

func TestRegression_AvisosDeLanceNaoInundamOGrupo(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notify := &mockNotify{}
	o := NewOrchestrator(NewStore(db), newMockPay(), notify, mockShipping{price: 1000},
		OrchestratorOptions{
			PaymentWindow: time.Hour, RegistrationWindow: time.Hour, ProviderName: "mock",
			BidAnnounceInterval: time.Hour, // nada além do 1º aviso deve sair
			Logf:                t.Logf,
		})
	go o.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: o.HandleEvent})
	prod := mkProduct(t, db, "Disputado")
	grupo := "grupo@g.us"
	if _, err := o.CreateAuction(ctx, eng, "lot-flood", prod, Config{
		StartPrice: 1000, MinIncrement: 100, Duration: 30 * time.Second,
	}, 0, grupo); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		from := fmt.Sprintf("p%d@s.whatsapp.net", i%2)
		if _, err := eng.PlaceBid(ctx, "lot-flood", from, int64(2000+100*i), false, fmt.Sprintf("m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, func() bool {
		var n int64
		db.QueryRow(`SELECT bid_count FROM lots WHERE id='lot-flood'`).Scan(&n)
		return n == 10
	})
	if got := notify.count(grupo); got > 1 {
		t.Fatalf("10 lances geraram %d mensagens no grupo — inundação", got)
	}
	// Mas quem foi superado continua sendo avisado no privado, sempre.
	if notify.count("p0@s.whatsapp.net")+notify.count("p1@s.whatsapp.net") < 5 {
		t.Fatal("avisos privados de 'você foi superado' foram engolidos junto")
	}
}

// --- 18. Token de sessão não fica em claro no banco ---------------------

func TestRegression_TokenDeSessaoNaoFicaEmClaroNoBanco(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "Fulano")
	token, err := st.CreateSession(ctx, info.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT token FROM sessions WHERE participant_id=$1`, info.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Fatal("o token de sessão está gravado em texto puro — um dump do banco vira passe livre")
	}
	// E o token em claro continua funcionando pela aplicação.
	got, err := st.GetSession(ctx, token)
	if err != nil || got != info.ID {
		t.Fatalf("sessão válida não foi reconhecida: %v %d", err, got)
	}
}

// --- 19. O link de cadastro não queima por erro do cliente -------------

// CEP fora da área de entrega não pode consumir o token: a pessoa precisa
// poder corrigir e reenviar. Antes, o token era consumido antes do
// cálculo de frete e a segunda tentativa dava "link expirado".
func TestRegression_CEPNaoAtendidoNaoQueimaOLink(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	// Sem nenhuma zona cadastrada, qualquer CEP é "não atendido".
	o := NewOrchestrator(st, newMockPay(), &mockNotify{}, NewZoneShipping(db),
		OrchestratorOptions{PaymentWindow: time.Hour, ProviderName: "mock", Logf: t.Logf})

	prod := mkProduct(t, db, "Item")
	if err := st.CreateLot(ctx, "lot-cep", prod, Config{StartPrice: 1000, MinIncrement: 100}, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "")
	// Lance vencedor, como o fechamento gravaria.
	if err := st.SetWinningAmount(ctx, "lot-cep", 6100); err != nil {
		t.Fatal(err)
	}
	token, err := st.OpenRegistration(ctx, "lot-cep", info.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	err = o.CompleteRegistration(ctx, token, sampleForm("99999-999"))
	if !errors.Is(err, ErrNoShippingZone) {
		t.Fatalf("esperava ErrNoShippingZone, veio %v", err)
	}
	// O token continua válido — é isso que importa.
	if _, err := st.GetRegistrationContext(ctx, token); err != nil {
		t.Fatalf("o link foi queimado por um erro que é do cliente corrigir: %v", err)
	}

	// Com uma zona que cobre o CEP, o mesmo link funciona.
	if _, err := db.Exec(`INSERT INTO shipping_zones (name, cep_prefix, price_cents) VALUES ('SP','01',2500)`); err != nil {
		t.Fatal(err)
	}
	if err := o.CompleteRegistration(ctx, token, sampleForm("01310-100")); err != nil {
		t.Fatalf("segunda tentativa com CEP válido deveria passar: %v", err)
	}
	var total int64
	db.QueryRow(`SELECT amount FROM payment_orders WHERE lot_id='lot-cep'`).Scan(&total)
	if total != 6100+2500 {
		t.Fatalf("total errado: %d", total)
	}
}

// Provedor fora do ar depois do cadastro salvo NÃO é erro do cliente: ele
// vê sucesso, e a cobrança entra na fila de retry.
func TestRegression_CobrancaPendenteNaoViraErroPraoCliente(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	st := NewStore(db)
	pay := newMockPay()
	pay.fail = true
	o := newTestOrchestrator(t, db, pay, &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	prod := mkProduct(t, db, "Item")
	st.CreateLot(ctx, "lot-pend", prod, Config{StartPrice: 1000, MinIncrement: 100}, time.Hour, "")
	info, _ := st.GetOrCreateParticipant(ctx, "a@s.whatsapp.net", "")
	token, err := st.OpenRegistration(ctx, "lot-pend", info.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	status, out := doJSON(t, srv, "POST", "/api/public/registration/"+token, "", map[string]string{
		"Name": "Fulano de Tal", "Document": "12345678900", "CEP": "01310-100",
		"Street": "Rua X", "Number": "10", "City": "São Paulo", "State": "SP",
	})
	if status != http.StatusOK {
		t.Fatalf("cliente deveria ver sucesso, veio %d %v", status, out)
	}
	if out["charge_pending"] != true {
		t.Fatalf("resposta deveria sinalizar cobrança pendente: %v", out)
	}
	// E o pedido existe, pendente, para o retry pegar.
	var n int
	db.QueryRow(`SELECT count(*) FROM payment_orders WHERE lot_id='lot-pend' AND status='pending' AND charge_id IS NULL`).Scan(&n)
	if n != 1 {
		t.Fatalf("pedido não ficou pendente para retry: %d", n)
	}
}
