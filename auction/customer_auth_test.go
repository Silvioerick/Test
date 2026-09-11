package auction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

func lastSentTo(n *mockNotify, jid string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.sent) - 1; i >= 0; i-- {
		if len(n.sent[i]) > len(jid) && n.sent[i][:len(jid)] == jid {
			return n.sent[i]
		}
	}
	return ""
}

var digitsRe = regexp.MustCompile(`\b(\d{6})\b`)

func extractCode(t *testing.T, text string) string {
	t.Helper()
	m := digitsRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("não achei o código de 6 dígitos em: %q", text)
	}
	return m[1]
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"(11) 99999-9999":   "5511999999999@s.whatsapp.net",
		"11999999999":       "5511999999999@s.whatsapp.net",
		"+55 11 99999-9999": "5511999999999@s.whatsapp.net",
		"5511999999999":     "5511999999999@s.whatsapp.net",
	}
	for in, want := range cases {
		got, err := NormalizePhone(in)
		if err != nil || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v — want %q", in, got, err, want)
		}
	}
	if _, err := NormalizePhone("123"); err == nil {
		t.Error("telefone curto demais deveria falhar")
	}
}

func TestCustomerLogin_HappyPath(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	orch := newTestOrchestrator(t, db, pay, notify, notices)
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	api := NewAPIServer(db, eng, orch, "segredo-do-painel")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	phone := "(11) 98888-7777"
	status, _ := doJSON(t, srv, "POST", "/api/customer/login/request", "", map[string]any{"phone": phone})
	if status != http.StatusOK {
		t.Fatalf("login/request: %d", status)
	}
	jid, _ := NormalizePhone(phone)
	text := lastSentTo(notify, jid)
	if text == "" {
		t.Fatal("código não foi enviado por WhatsApp")
	}
	code := extractCode(t, text)

	// código errado não loga e conta como tentativa
	status, _ = doJSON(t, srv, "POST", "/api/customer/login/verify", "", map[string]any{"phone": phone, "code": "000000"})
	if status != http.StatusUnauthorized {
		t.Fatalf("código errado deveria dar 401, got %d", status)
	}

	status, out := doJSON(t, srv, "POST", "/api/customer/login/verify", "", map[string]any{"phone": phone, "code": code})
	if status != http.StatusOK {
		t.Fatalf("login/verify: %d %v", status, out)
	}
	token, _ := out["session_token"].(string)
	if token == "" {
		t.Fatal("sessão não veio no login")
	}

	// sem sessão -> 401
	if status, _ := doJSON(t, srv, "GET", "/api/customer/me", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("esperava 401 sem sessão, got %d", status)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/customer/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("me: %v status=%v", err, resp)
	}
	resp.Body.Close()

	// código já usado não pode logar de novo
	status, _ = doJSON(t, srv, "POST", "/api/customer/login/verify", "", map[string]any{"phone": phone, "code": code})
	if status != http.StatusUnauthorized {
		t.Fatalf("código reutilizado deveria falhar, got %d", status)
	}
}

func TestCustomerLogin_TooManyAttemptsLocksCode(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	api := NewAPIServer(db, eng, orch, "segredo-do-painel")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	phone := "11977776666"
	doJSON(t, srv, "POST", "/api/customer/login/request", "", map[string]any{"phone": phone})
	for i := 0; i < loginMaxAttempts; i++ {
		doJSON(t, srv, "POST", "/api/customer/login/verify", "", map[string]any{"phone": phone, "code": "111111"})
	}
	// mesmo se o código certo chegasse agora, o código já devia estar bloqueado
	jid, _ := NormalizePhone(phone)
	var attempts int
	db.QueryRow(`SELECT attempts FROM login_codes WHERE participant_id = (SELECT id FROM participants WHERE whatsapp_jid=$1)`, jid).Scan(&attempts)
	if attempts < loginMaxAttempts {
		t.Fatalf("esperava >= %d tentativas registradas, tem %d", loginMaxAttempts, attempts)
	}
}

func TestCustomerLogin_RateLimited(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	api := NewAPIServer(db, eng, orch, "segredo-do-painel")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	phone := "11955554444"
	for i := 0; i < loginCodeMaxTries; i++ {
		status, _ := doJSON(t, srv, "POST", "/api/customer/login/request", "", map[string]any{"phone": phone})
		if status != http.StatusOK {
			t.Fatalf("pedido %d deveria passar, got %d", i, status)
		}
	}
	status, _ := doJSON(t, srv, "POST", "/api/customer/login/request", "", map[string]any{"phone": phone})
	if status != http.StatusTooManyRequests {
		t.Fatalf("4º pedido deveria ser bloqueado por rate limit, got %d", status)
	}
}

// Cobre o pedido original: extrato completo do cliente — lances, se
// ganhou, valor pago ou não pago — via a própria sessão dele, cobrindo um
// lote pago e um lote em que ele NÃO venceu.
func TestCustomerHistory_ShowsWinsLossesAndPaymentStatus(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pay, notify, notices := newMockPay(), &mockNotify{}, &noticeLog{}
	orch := newTestOrchestrator(t, db, pay, notify, notices)
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	go orch.RunExpiryWorker(ctx, eng, 30*time.Millisecond)
	go eng.Run(ctx)
	api := NewAPIServer(db, eng, orch, "segredo-do-painel")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	jid := "5511933332222@s.whatsapp.net"
	phone := "5511933332222"

	// lote 1: essa pessoa GANHA e PAGA
	prod1 := mkProduct(t, db, "Item que ela ganhou")
	orch.CreateAuction(ctx, eng, "lot-h1", prod1, Config{StartPrice: 1000, MinIncrement: 100, Duration: 200 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-h1", jid, 1500, false, "m1")
	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id='lot-h1'`).Scan(&s)
		return s == "awaiting_registration"
	})
	orch.CompleteRegistration(ctx, tokenForLot(t, db, "lot-h1"), sampleForm("01310-000"))
	var charge1 string
	db.QueryRow(`SELECT charge_id FROM payment_orders WHERE lot_id='lot-h1'`).Scan(&charge1)
	orch.OnPaymentWebhook(ctx, charge1)

	// lote 2: essa pessoa dá lance mas PERDE (outra pessoa cobre o lance dela)
	prod2 := mkProduct(t, db, "Item que ela perdeu")
	orch.CreateAuction(ctx, eng, "lot-h2", prod2, Config{StartPrice: 1000, MinIncrement: 100, Duration: 200 * time.Millisecond, ExtendTo: 20 * time.Millisecond}, 0, "")
	eng.PlaceBid(ctx, "lot-h2", jid, 1200, false, "m2")
	eng.PlaceBid(ctx, "lot-h2", "outro@s.whatsapp.net", 2000, false, "m3")
	waitFor(t, 2*time.Second, func() bool {
		var s string
		db.QueryRow(`SELECT status FROM lots WHERE id='lot-h2'`).Scan(&s)
		return s == "awaiting_registration"
	})

	// login e conferir o extrato
	doJSON(t, srv, "POST", "/api/customer/login/request", "", map[string]any{"phone": phone})
	code := extractCode(t, lastSentTo(notify, jid))
	_, out := doJSON(t, srv, "POST", "/api/customer/login/verify", "", map[string]any{"phone": phone, "code": code})
	token := out["session_token"].(string)

	req, _ := http.NewRequest("GET", srv.URL+"/api/customer/history", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hist []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("esperava 2 lotes no extrato, veio %d: %v", len(hist), hist)
	}
	byLot := map[string]map[string]any{}
	for _, h := range hist {
		byLot[h["lot_id"].(string)] = h
	}
	won := byLot["lot-h1"]
	if won["won"] != true || won["payment_status"] != "paid" || won["your_bid_cents"].(float64) != 1500 {
		t.Fatalf("lote ganho/pago errado: %v", won)
	}
	lost := byLot["lot-h2"]
	if lost["won"] != false || lost["your_bid_cents"].(float64) != 1200 {
		t.Fatalf("lote perdido errado: %v", lost)
	}
	if _, hasCharge := lost["charge_cents"]; hasCharge {
		t.Fatalf("lote perdido não deveria ter cobrança nenhuma pra essa pessoa: %v", lost)
	}
}
