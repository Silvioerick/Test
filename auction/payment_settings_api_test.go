package auction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPI_PaymentSettings_NotConfigured_Returns501(t *testing.T) {
	db := testDB(t)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	api := NewAPIServer(db, eng, orch, "segredo-do-painel") // sem WithPaymentSettings
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	status, _ := doJSON(t, srv, "GET", "/api/settings/payment", "segredo-do-painel", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("esperava 501 sem PaymentSettingsStore ligado, got %d", status)
	}
}

func TestAPI_PaymentSettings_FullFlow(t *testing.T) {
	db := testDB(t)
	db.Exec(`TRUNCATE payment_settings`)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	enc := testEncryptor(t)
	ps := NewPaymentSettingsStore(db, enc)
	api := NewAPIServer(db, eng, orch, "segredo-do-painel").WithPaymentSettings(ps)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	// exige token de admin (é configuração sensível, não é rota pública)
	if status, _ := doJSON(t, srv, "GET", "/api/settings/payment", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("esperava 401 sem token de admin, got %d", status)
	}

	fake, _ := fakeAsaas(t)
	status, out := doJSON(t, srv, "POST", "/api/settings/payment", "segredo-do-painel", map[string]any{
		"provider": "asaas", "api_key": "minha-chave-de-verdade-123456", "base_url": fake.URL, "activate": true,
	})
	if status != http.StatusOK {
		t.Fatalf("cadastrar chave: %d %v", status, out)
	}

	status, listBody := doJSON(t, srv, "GET", "/api/settings/payment", "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("listar: %d", status)
	}
	_ = listBody // doJSON só decodifica objeto; a resposta real é array — checa no banco abaixo

	list, err := ps.List(context.Background())
	if err != nil || len(list) != 1 || !list[0].Active {
		t.Fatalf("provedor deveria estar cadastrado e ativo: %+v err=%v", list, err)
	}
	if list[0].MaskedKey == "minha-chave-de-verdade-123456" {
		t.Fatal("a chave nunca pode aparecer em texto puro, nem pra admin")
	}

	// a cobrança de verdade passa a usar o que foi configurado no painel
	dbPay := NewDBPaymentProvider(ps)
	res, err := dbPay.CreateCharge(context.Background(), "ref-x", 2000, Customer{Name: "Y", Document: "12345678900"})
	if err != nil || res.ChargeID == "" {
		t.Fatalf("cobrança via configuração do painel falhou: %v %+v", err, res)
	}

	// ativar um provedor não cadastrado deve dar erro claro, não 500 genérico
	status, out = doJSON(t, srv, "POST", "/api/settings/payment/activate", "segredo-do-painel", map[string]any{"provider": "stripe"})
	if status != http.StatusBadRequest {
		t.Fatalf("esperava 400 pra provedor não cadastrado, got %d %v", status, out)
	}
}
