package auction

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// --- Conexão do WhatsApp pelo painel -----------------------------------
//
// Com a URL e o token do DigiGO salvos em Configurações, o resto acontece
// aqui: conectar, ler o QR, conferir o status e registrar o webhook de
// volta no gateway. Ninguém precisa abrir o painel do DigiGO para colar
// URL nenhuma.

func (s *APIServer) digigo(r *http.Request) (*DigiGO, error) {
	if s.notifier == nil {
		return nil, errNoNotifierConfigured
	}
	return s.notifier.Client(r.Context())
}

// waStatus junta o estado da sessão com o webhook registrado e o que ele
// DEVERIA ser, para a tela mostrar se está tudo apontado certo.
func (s *APIServer) waStatus(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"hint":       "preencha a URL e o token do DigiGO em Configurações",
		})
		return
	}
	st, err := c.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true, "reachable": false, "error": err.Error(),
		})
		return
	}
	hook, events, hookErr := c.Webhook(r.Context())
	esperado := s.expectedWebhookURL(r)
	out := map[string]any{
		"configured":       true,
		"reachable":        true,
		"connected":        st.Connected,
		"logged_in":        st.LoggedIn,
		"jid":              st.JID,
		"webhook":          hook,
		"webhook_events":   events,
		"webhook_expected": esperado,
		"webhook_ok":       hook != "" && sameWebhook(hook, esperado),
	}
	if hookErr != nil {
		out["webhook_error"] = hookErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// expectedWebhookURL monta o endereço que o gateway deve chamar, já com o
// segredo na query — é assim que ele volta autenticado.
func (s *APIServer) expectedWebhookURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := firstNonEmpty(r.Header.Get("X-Forwarded-Host"), r.Host)
	u := scheme + "://" + host + "/api/webhooks/whatsapp"
	if sec := s.whatsappSecret(r); sec != "" {
		u += "?token=" + sec
	}
	return u
}

// sameWebhook compara ignorando o segredo, que pode ter sido rotacionado.
func sameWebhook(a, b string) bool {
	corta := func(s string) string {
		if i := strings.Index(s, "?"); i >= 0 {
			return s[:i]
		}
		return s
	}
	return corta(a) == corta(b)
}

func (s *APIServer) waConnect(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "preencha a URL e o token do DigiGO em Configurações antes de conectar")
		return
	}
	if err := c.Connect(r.Context()); err != nil {
		s.logf("auction api: conectar whatsapp: %v", err)
		writeErr(w, http.StatusBadGateway, "o gateway recusou a conexão: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *APIServer) waQR(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "gateway não configurado")
		return
	}
	qr, err := c.QRCode(r.Context())
	if err != nil {
		s.logf("auction api: ler QR: %v", err)
		writeErr(w, http.StatusBadGateway, "não consegui ler o QR: "+err.Error())
		return
	}
	// QR vazio é resposta legítima: quer dizer que já está logado.
	writeJSON(w, http.StatusOK, map[string]string{"qr": qr})
}

func (s *APIServer) waDisconnect(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "gateway não configurado")
		return
	}
	var in struct {
		Logout bool `json:"logout"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	// Desconectar derruba a sessão; sair (logout) desfaz o pareamento e
	// exige escanear o QR de novo.
	if in.Logout {
		err = c.Logout(r.Context())
	} else {
		err = c.Disconnect(r.Context())
	}
	if err != nil {
		s.logf("auction api: desconectar whatsapp: %v", err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// waRegisterWebhook aponta o gateway para cá. Recusa se o segredo do
// webhook ainda não foi definido: sem ele o endpoint responde 503 e o
// gateway ficaria mandando mensagem para o vazio.
func (s *APIServer) waRegisterWebhook(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "gateway não configurado")
		return
	}
	if s.whatsappSecret(r) == "" {
		writeErr(w, http.StatusBadRequest,
			"defina antes o segredo do webhook de entrada, em Configurações — sem ele o endereço não aceita nada")
		return
	}
	url := s.expectedWebhookURL(r)
	if strings.HasPrefix(url, "http://") {
		s.logf("auction api: ATENÇÃO registrando webhook em HTTP puro (%s) — o segredo viaja em claro", url)
	}
	if err := c.SetWebhook(r.Context(), url); err != nil {
		s.logf("auction api: registrar webhook: %v", err)
		writeErr(w, http.StatusBadGateway, "o gateway recusou: "+err.Error())
		return
	}
	s.logf("auction api: webhook registrado no gateway: %s", strings.Split(url, "?")[0])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": url})
}

// waDiagnose mostra a resposta LITERAL do gateway para as rotas que o
// painel usa. É o que transforma "não funcionou" em "o gateway respondeu
// isto" — sem precisar de acesso ao servidor.
func (s *APIServer) waDiagnose(w http.ResponseWriter, r *http.Request) {
	c, err := s.digigo(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "preencha a URL e o token do DigiGO em Configurações")
		return
	}
	out := []map[string]any{}
	for _, path := range []string{"/session/status", "/webhook", "/health"} {
		linha := map[string]any{"rota": path, "url": c.BaseURL + path}
		status, body, err := c.RawGet(r.Context(), path)
		if err != nil {
			linha["erro_de_rede"] = err.Error()
		} else {
			linha["http"] = status
			linha["resposta"] = truncate(body, 800)
		}
		out = append(out, linha)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url": c.BaseURL,
		"token_termina_em": func() string {
			if len(c.Token) > 6 {
				return "…" + c.Token[len(c.Token)-6:]
			}
			return "(muito curto)"
		}(),
		"chamadas": out,
	})
}

var _ = errors.Is // mantém o import quando o arquivo evolui
