package auction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeDigiGO imita o gateway conforme a coleção oficial: auth no header
// "token", respostas envelopadas em {code,data,success}.
type fakeDigiGO struct {
	mu       sync.Mutex
	token    string
	loggedIn bool
	webhook  string
	enviadas []map[string]any
	chamadas []string
}

func (f *fakeDigiGO) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	envelope := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "data": data})
	}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("token") != f.token {
				http.Error(w, `{"success":false,"error":"token inválido"}`, http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			f.chamadas = append(f.chamadas, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("/session/status", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		envelope(w, map[string]any{"Connected": true, "LoggedIn": f.loggedIn, "jid": "5511999@s.whatsapp.net"})
	}))
	mux.HandleFunc("/session/connect", auth(func(w http.ResponseWriter, r *http.Request) {
		envelope(w, map[string]any{"details": "Connected!"})
	}))
	mux.HandleFunc("/session/qr", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		qr := "iVBORw0KGgoAAAANS-qr-falso"
		if f.loggedIn {
			qr = "" // já logado: sem QR, conforme a doc
		}
		envelope(w, map[string]any{"QRCode": qr})
	}))
	mux.HandleFunc("/webhook", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPost {
			var in struct {
				WebhookURL string   `json:"webhookurl"`
				Events     []string `json:"events"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			f.webhook = in.WebhookURL
			envelope(w, map[string]any{"webhook": in.WebhookURL, "events": in.Events})
			return
		}
		envelope(w, map[string]any{"webhook": f.webhook, "events": []string{"Message"}})
	}))
	mux.HandleFunc("/chat/send/text", auth(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.enviadas = append(f.enviadas, in)
		f.mu.Unlock()
		envelope(w, map[string]any{"Id": "ABC123"})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDigiGO_FluxoDeConexao(t *testing.T) {
	f := &fakeDigiGO{token: "tok-digigo"}
	srv := f.server(t)
	ctx := context.Background()
	c := NewDigiGO(srv.URL, "tok-digigo")

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Connected || st.LoggedIn {
		t.Fatalf("status inicial errado: %+v", st)
	}

	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	qr, err := c.QRCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if qr == "" {
		t.Fatal("deveria vir QR enquanto não está logado")
	}

	// Depois de parear, o QR some — é assim que a tela sabe que conectou.
	f.mu.Lock()
	f.loggedIn = true
	f.mu.Unlock()
	if qr, err = c.QRCode(ctx); err != nil || qr != "" {
		t.Fatalf("logado deveria devolver QR vazio, veio %q (%v)", qr, err)
	}

	const hook = "https://leiloes.example/api/webhooks/whatsapp?token=seg"
	if err := c.SetWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	got, events, err := c.Webhook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("webhook gravado errado: %q", got)
	}
	if len(events) == 0 {
		t.Fatal("sem eventos assinados")
	}
}

// O gateway espera {Phone, Body} — não o {to, text} genérico.
func TestDigiGO_EnvioUsaPhoneEBody(t *testing.T) {
	f := &fakeDigiGO{token: "tok"}
	srv := f.server(t)
	c := NewDigiGO(srv.URL, "tok")
	ctx := context.Background()

	if err := c.SendText(ctx, "5511988887777@s.whatsapp.net", "olá"); err != nil {
		t.Fatal(err)
	}
	// Grupo mantém o sufixo; conversa vira só o número.
	if err := c.SendText(ctx, "12036304-159@g.us", "aviso do grupo"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enviadas) != 2 {
		t.Fatalf("esperava 2 envios, houve %d", len(f.enviadas))
	}
	if f.enviadas[0]["Phone"] != "5511988887777" || f.enviadas[0]["Body"] != "olá" {
		t.Fatalf("payload errado: %v", f.enviadas[0])
	}
	if f.enviadas[1]["Phone"] != "12036304-159@g.us" {
		t.Fatalf("grupo deveria manter o sufixo: %v", f.enviadas[1])
	}
}

// Token errado tem de falhar, não passar em silêncio.
func TestDigiGO_TokenErradoFalha(t *testing.T) {
	f := &fakeDigiGO{token: "certo"}
	srv := f.server(t)
	if _, err := NewDigiGO(srv.URL, "errado").Status(context.Background()); err == nil {
		t.Fatal("token errado deveria falhar")
	}
}

// O evento que o DIGIGO entrega (formato whatsmeow) vira lance.
func TestDigiGO_EventoDoWhatsmeowViraMensagem(t *testing.T) {
	corpo := []byte(`{
	  "type":"Message",
	  "event":{
	    "Info":{"ID":"3EB0ABC","Chat":"12036304-159@g.us","Sender":"5511988887777@s.whatsapp.net",
	            "PushName":"Maria","IsFromMe":false,"IsGroup":true},
	    "Message":{"conversation":"250,00"}
	  }}`)
	m, err := ParseInbound(corpo)
	if err != nil {
		t.Fatal(err)
	}
	if m.MsgID != "3EB0ABC" || m.From != "5511988887777@s.whatsapp.net" ||
		m.Channel != "12036304-159@g.us" || m.Text != "250,00" {
		t.Fatalf("parse errado: %+v", m)
	}

	// Texto longo vem em extendedTextMessage.
	m2, err := ParseInbound([]byte(`{"type":"Message","event":{
	    "Info":{"ID":"X1","Chat":"g@g.us","Sender":"a@s.whatsapp.net"},
	    "Message":{"extendedTextMessage":{"Text":"+"}}}}`))
	if err != nil || m2.Text != "+" {
		t.Fatalf("extendedTextMessage não foi lido: %+v (%v)", m2, err)
	}

	// Mensagem que o próprio bot mandou não pode virar lance de ninguém.
	if _, err := ParseInbound([]byte(`{"type":"Message","event":{
	    "Info":{"ID":"X2","Chat":"g@g.us","Sender":"eu@s.whatsapp.net","IsFromMe":true},
	    "Message":{"conversation":"500"}}}`)); err == nil {
		t.Fatal("mensagem própria deveria ser ignorada")
	}
}
