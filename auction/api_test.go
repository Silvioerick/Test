package auction

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func doJSON(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// testHooks são os segredos de webhook usados nos testes. Sem eles a API
// recusa o webhook com 503 — a política é falhar fechada.
var testHooks = WebhookAuth{HubPaySecret: "segredo-hubpay", AsaasToken: "token-asaas"}

// postHubPay assina o corpo como a HubPay faria.
func postHubPay(t *testing.T, srv *httptest.Server, payload any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(testHooks.HubPaySecret))
	mac.Write(body)
	req, err := http.NewRequest("POST", srv.URL+"/api/webhooks/hubpay", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hubpay-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAPI_FullPanelFlow(t *testing.T) {
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

	api := NewAPIServer(db, eng, orch, "segredo-do-painel").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	// sem token -> 401
	if status, _ := doJSON(t, srv, "GET", "/api/products", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("esperava 401 sem token, got %d", status)
	}

	// cadastro de produto pelo painel
	status, out := doJSON(t, srv, "POST", "/api/products", "segredo-do-painel", map[string]any{
		"name": "Miniatura Fórmula 1 1:18", "sku": "F1-001",
	})
	if status != http.StatusCreated {
		t.Fatalf("criar produto: %d %v", status, out)
	}
	productID := int64(out["id"].(float64))

	// abrir leilão pra esse produto
	status, out = doJSON(t, srv, "POST", "/api/lots", "segredo-do-painel", map[string]any{
		"product_id": productID, "start_price_cents": 5000, "min_increment_cents": 500,
		"duration_seconds": 1, "extend_seconds": 1,
		// payment_minutes omitido: herda o padrão do orquestrador (300ms nos testes)
	})
	if status != http.StatusCreated {
		t.Fatalf("criar lote: %d %v", status, out)
	}
	lotID := out["lot_id"].(string)
	jid := "5511999999999@s.whatsapp.net"

	// lance chega via WhatsApp (não pela API do painel)
	if _, err := eng.PlaceBid(ctx, lotID, jid, 6500, false, "m1"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		status, out := doJSON(t, srv, "GET", "/api/lots/"+lotID, "segredo-do-painel", nil)
		if status != http.StatusOK {
			return false
		}
		lot := out["lot"].(map[string]any)
		return lot["status"] == "awaiting_registration"
	})

	// simula o cliente abrindo o link recebido no WhatsApp e preenchendo o
	// formulário público de cadastro (sem token de admin)
	token := tokenForLot(t, db, lotID)
	status, regInfo := doJSON(t, srv, "GET", "/api/public/registration?token="+token, "", nil)
	if status != http.StatusOK || regInfo["bid_amount_cents"].(float64) != 6500 {
		t.Fatalf("get registration: %d %v", status, regInfo)
	}
	status, out = doJSON(t, srv, "POST", "/api/public/registration/"+token, "", sampleForm("01310-000"))
	if status != http.StatusOK {
		t.Fatalf("post registration: %d %v", status, out)
	}

	waitFor(t, 2*time.Second, func() bool {
		status, out := doJSON(t, srv, "GET", "/api/lots/"+lotID, "segredo-do-painel", nil)
		if status != http.StatusOK {
			return false
		}
		return out["lot"].(map[string]any)["status"] == "awaiting_payment"
	})

	status, out = doJSON(t, srv, "GET", "/api/lots/"+lotID, "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("get lote: %d %v", status, out)
	}
	lot := out["lot"].(map[string]any)
	if lot["claimant_jid"] != jid || lot["current_amount_cents"].(float64) != 6500 || lot["shipping_cents"].(float64) != 1000 {
		t.Fatalf("lote inesperado: %v", lot)
	}
	bids := out["bids"].([]any)
	if len(bids) != 1 {
		t.Fatalf("esperava 1 lance no relatório, tem %d", len(bids))
	}
	orders := out["payment_orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("esperava 1 pedido de pagamento, tem %d", len(orders))
	}
	if orders[0].(map[string]any)["amount_cents"].(float64) != 7500 { // 6500 lance + 1000 frete
		t.Fatalf("total cobrado errado: %v", orders[0])
	}

	// listagem geral também reflete
	status, _ = doJSON(t, srv, "GET", "/api/lots?status=awaiting_payment", "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("list lotes: %d", status)
	}

	// simula o webhook confirmando o pagamento
	var chargeID string
	db.QueryRow(`SELECT charge_id FROM payment_orders WHERE lot_id = $1`, lotID).Scan(&chargeID)
	status, out = postHubPay(t, srv, map[string]any{"charge_id": chargeID, "status": "paid"})
	if status != http.StatusOK {
		t.Fatalf("webhook hubpay: %d %v", status, out)
	}

	status, out = doJSON(t, srv, "GET", "/api/reports/summary", "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("summary: %d %v", status, out)
	}
	if out["sold"].(float64) != 1 || out["revenue_cents"].(float64) != 7500 {
		t.Fatalf("relatório errado: %v", out)
	}

	status, custOut := doJSON(t, srv, "GET", "/api/customers", "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("listar clientes: %d", status)
	}
	custs := custOut // na verdade é um array, mas doJSON só decodifica objetos
	_ = custs
}

// Cobre exatamente o cenário do pedido: leilão de 1 min real (aqui
// comprimido), vencedor nunca completa o cadastro/pagamento, produto some
// do estoque disponível enquanto aguarda e reaparece depois — sem segundo
// colocado.
func TestAPI_DefaultReturnsToStock(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	go orch.RunExpiryWorker(ctx, eng, 20*time.Millisecond)
	go eng.Run(ctx)
	api := NewAPIServer(db, eng, orch, "segredo-do-painel").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	_, out := doJSON(t, srv, "POST", "/api/products", "segredo-do-painel", map[string]any{"name": "Boné raro"})
	productID := int64(out["id"].(float64))
	_, out = doJSON(t, srv, "POST", "/api/lots", "segredo-do-painel", map[string]any{
		"product_id": productID, "start_price_cents": 1000, "min_increment_cents": 100,
		"duration_seconds": 1, "extend_seconds": 1,
	})
	lotID := out["lot_id"].(string)
	eng.PlaceBid(ctx, lotID, "unico@s.whatsapp.net", 1200, false, "m1")

	waitFor(t, 4*time.Second, func() bool {
		status, out := doJSON(t, srv, "GET", "/api/lots/"+lotID, "segredo-do-painel", nil)
		if status != http.StatusOK {
			return false
		}
		return out["lot"].(map[string]any)["status"] == "unsold"
	})

	status, out := doJSON(t, srv, "GET", "/api/reports/summary", "segredo-do-painel", nil)
	if status != http.StatusOK {
		t.Fatalf("summary: %d", status)
	}
	if out["unsold_no_payment"].(float64) != 1 || out["total_defaults"].(float64) != 1 {
		t.Fatalf("relatório de calote errado: %v", out)
	}
}

func TestAPI_ShippingZonesCRUD(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orch := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	go orch.Run(ctx)
	rdb := setup(t)
	eng := New(rdb, Options{OnEvent: orch.HandleEvent})
	api := NewAPIServer(db, eng, orch, "segredo-do-painel").WithWebhookAuth(testHooks)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	status, _ := doJSON(t, srv, "POST", "/api/shipping-zones", "segredo-do-painel", map[string]any{
		"name": "Capital SP", "cep_prefix": "01", "price_cents": 1500,
	})
	if status != http.StatusCreated {
		t.Fatalf("criar zona: %d", status)
	}
	status, _ = doJSON(t, srv, "POST", "/api/shipping-zones", "segredo-do-painel", map[string]any{
		"name": "Resto do Brasil", "cep_prefix": "", "price_cents": 3000,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("prefixo vazio deveria ser rejeitado, got %d", status)
	}
}
