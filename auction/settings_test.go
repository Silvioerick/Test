package auction

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testSettings(t *testing.T, db *sql.DB) *SettingsStore {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	enc, err := NewEncryptor(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM app_settings`); err != nil {
		t.Fatal(err)
	}
	return NewSettingsStore(db, enc)
}

// O que o admin salva no painel tem de ganhar da variável de ambiente.
func TestSettings_PainelGanhaDoEnv(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	ctx := context.Background()

	t.Setenv("WHATSAPP_API_URL", "https://vindo-do-env.example")
	if got := s.Get(ctx, SetWhatsAppAPIURL); got != "https://vindo-do-env.example" {
		t.Fatalf("sem nada salvo, deveria herdar do env, got %q", got)
	}
	if err := s.Set(ctx, SetWhatsAppAPIURL, "https://vindo-do-painel.example"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(ctx, SetWhatsAppAPIURL); got != "https://vindo-do-painel.example" {
		t.Fatalf("o painel deveria ganhar do env, got %q", got)
	}
	// Apagar no painel devolve o controle ao env.
	if err := s.Set(ctx, SetWhatsAppAPIURL, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(ctx, SetWhatsAppAPIURL); got != "https://vindo-do-env.example" {
		t.Fatalf("apagado no painel, deveria voltar ao env, got %q", got)
	}
}

// Segredo vai cifrado para o banco e nunca volta em claro pela API.
func TestSettings_SegredoCifradoENuncaVazaNaAPI(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	ctx := context.Background()
	const token = "token-super-secreto-do-digigo-123456"

	if err := s.Set(ctx, SetWhatsAppAPIToken, token); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var plain any
	if err := db.QueryRow(`SELECT value_enc, value FROM app_settings WHERE key=$1`,
		SetWhatsAppAPIToken).Scan(&stored, &plain); err != nil {
		t.Fatal(err)
	}
	if plain != nil {
		t.Fatal("segredo gravado na coluna em claro")
	}
	if bytesContains(stored, []byte(token)) {
		t.Fatal("o token aparece em claro dentro do blob cifrado")
	}
	if got := s.Get(ctx, SetWhatsAppAPIToken); got != token {
		t.Fatalf("não consegui decifrar de volta: %q", got)
	}

	views, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Key != SetWhatsAppAPIToken {
			continue
		}
		if v.Value != "" {
			t.Fatalf("segredo exposto em Value: %q", v.Value)
		}
		if v.Masked == "" || v.Masked == token {
			t.Fatalf("máscara errada: %q", v.Masked)
		}
		if v.Source != "painel" {
			t.Fatalf("fonte deveria ser painel, é %q", v.Source)
		}
	}
}

// A tela precisa dizer de onde cada valor veio, para o admin saber o que
// ainda está preso em variável de ambiente.
func TestSettings_ListaMostraAOrigem(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	ctx := context.Background()
	t.Setenv("HUBPAY_WEBHOOK_SECRET", "segredo-do-env")
	if err := s.Set(ctx, SetRegisterURL, "https://painel.example/cadastro/"); err != nil {
		t.Fatal(err)
	}
	views, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, v := range views {
		src[v.Key] = v.Source
	}
	if src[SetRegisterURL] != "painel" {
		t.Errorf("register_url deveria vir do painel, veio de %q", src[SetRegisterURL])
	}
	if src[SetHubPayWebhookSecret] != "env" {
		t.Errorf("segredo do hubpay deveria vir do env, veio de %q", src[SetHubPayWebhookSecret])
	}
	if src[SetAsaasWebhookToken] != "vazio" {
		t.Errorf("asaas não configurado deveria ser vazio, é %q", src[SetAsaasWebhookToken])
	}
}

// Chave inventada é erro de quem chamou, não 500.
func TestSettings_ChaveDesconhecidaEhRecusada(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	if err := s.Set(context.Background(), "coisa.inventada", "x"); err == nil {
		t.Fatal("chave desconhecida deveria ser recusada")
	}
}

// O segredo do webhook salvo no painel passa a valer de verdade na
// verificação de assinatura, sem reiniciar o processo.
func TestSettings_SegredoDoPainelValeNoWebhook(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	ctx := context.Background()
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin").WithSettings(s)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]string{"charge_id": "x", "status": "paid"})

	// Nada configurado: falha fechada.
	resp, err := http.Post(srv.URL+"/api/webhooks/hubpay", "application/json", bytesReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("sem segredo deveria dar 503, deu %d", resp.StatusCode)
	}

	// Admin salva o segredo pelo painel — sem reiniciar nada.
	if err := s.Set(ctx, SetHubPayWebhookSecret, "segredo-novo"); err != nil {
		t.Fatal(err)
	}
	// Agora assinatura errada é 401 (o segredo passou a existir)...
	resp, err = http.Post(srv.URL+"/api/webhooks/hubpay", "application/json", bytesReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("com segredo salvo e assinatura ausente deveria dar 401, deu %d", resp.StatusCode)
	}
	// ...e a assinatura certa com o segredo NOVO é aceita.
	req, _ := http.NewRequest("POST", srv.URL+"/api/webhooks/hubpay", bytesReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hubpay-Signature", hmacHex("segredo-novo", body))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 409 = assinatura OK, cobrança inexistente (pede reentrega). O que
	// importa é que NÃO foi recusado por assinatura.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("segredo do painel não foi aceito: %d", resp.StatusCode)
	}
}

// Os endpoints do painel: listar mostra máscara, gravar aplica.
func TestSettings_EndpointsDoPainel(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	o := newTestOrchestrator(t, db, newMockPay(), &mockNotify{}, &noticeLog{})
	api := NewAPIServer(db, New(setup(t), Options{}), o, "admin").WithSettings(s)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	// Exige token de admin.
	if st, _ := doJSON(t, srv, "GET", "/api/settings/app", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("sem token deveria dar 401, deu %d", st)
	}
	if st, _ := doJSON(t, srv, "POST", "/api/settings/app", "admin", map[string]string{
		"key": SetWhatsAppAPIURL, "value": "https://digigo.example/send",
	}); st != http.StatusOK {
		t.Fatalf("gravar deveria funcionar, deu %d", st)
	}
	if st, _ := doJSON(t, srv, "POST", "/api/settings/app", "admin", map[string]string{
		"key": "nao.existe", "value": "x",
	}); st != http.StatusBadRequest {
		t.Fatalf("chave desconhecida deveria dar 400, deu %d", st)
	}
	if got := s.Get(context.Background(), SetWhatsAppAPIURL); got != "https://digigo.example/send" {
		t.Fatalf("valor não aplicou: %q", got)
	}
}

// O notificador resolve o gateway no banco a cada envio.
func TestSettings_NotificadorLeDoBanco(t *testing.T) {
	db := testDB(t)
	s := testSettings(t, db)
	ctx := context.Background()
	got := make(chan map[string]any, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if in == nil {
			in = map[string]any{}
		}
		in["_token"] = r.Header.Get("token")
		in["_path"] = r.URL.Path
		got <- in
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":200,"success":true,"data":{"Id":"X"}}`))
	}))
	t.Cleanup(gw.Close)

	n := NewDBNotifier(s).WithLogger(t.Logf)
	// Sem configuração, falha explicitamente em vez de fingir que enviou.
	if err := n.SendText(ctx, "a@s.whatsapp.net", "oi"); err == nil {
		t.Fatal("sem gateway configurado deveria dar erro")
	}
	if err := s.Set(ctx, SetWhatsAppAPIURL, gw.URL); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, SetWhatsAppAPIToken, "tok-123"); err != nil {
		t.Fatal(err)
	}
	if err := n.SendText(ctx, "a@s.whatsapp.net", "oi"); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	// Contrato real do DigiGO: POST /chat/send/text com {Phone, Body} e
	// autenticação no header "token" — não o {to, text} + Bearer genérico.
	if msg["_path"] != "/chat/send/text" {
		t.Fatalf("rota errada: %v", msg["_path"])
	}
	if msg["Phone"] != "a" || msg["Body"] != "oi" {
		t.Fatalf("payload errado: %v", msg)
	}
	if msg["_token"] != "tok-123" {
		t.Fatalf("token do painel não foi usado no header token: %v", msg["_token"])
	}
}

func bytesContains(haystack, needle []byte) bool { return bytes.Contains(haystack, needle) }

func hmacHex(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}
