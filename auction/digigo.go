package auction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DigiGO fala com a API do gateway de WhatsApp (DIGIGO v3.1.5, base
// whatsmeow/wuzapi).
//
// Contrato confirmado na coleção oficial da API:
//   - autenticação no header "token" (NÃO é Authorization: Bearer);
//   - POST /session/connect  {Subscribe, Immediate} conecta e, sem sessão
//     anterior, gera o QR;
//   - GET  /session/qr       devolve o QR (vazio quando já logado);
//   - GET  /session/status   conexão e login;
//   - POST /webhook          {webhookurl, events} registra o webhook;
//   - POST /chat/send/text   {Phone, Body} envia mensagem.
type DigiGO struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewDigiGO(baseURL, token string) *DigiGO {
	return &DigiGO{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// envelope: a API devolve {"code":200,"data":{...},"success":true} na
// maioria das rotas, mas nem sempre. Guardamos o data cru e, se não vier
// envelopado, usamos o corpo inteiro.
type digigoEnvelope struct {
	Code    int             `json:"code"`
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

func (d *DigiGO) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, d.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("token", d.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("digigo: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("digigo: %s %s devolveu HTTP %d: %s",
			method, path, resp.StatusCode, truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	var env digigoEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		if env.Success != nil && !*env.Success {
			return fmt.Errorf("digigo: %s %s recusado: %s", method, path,
				firstNonEmpty(env.Error, env.Message, truncate(string(raw), 200)))
		}
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(raw, out)
}

// DigiGOStatus é o estado da conexão com o WhatsApp.
type DigiGOStatus struct {
	Connected bool   `json:"Connected"`
	LoggedIn  bool   `json:"LoggedIn"`
	JID       string `json:"jid"`
	Name      string `json:"name"`
	// Webhook e Events vêm de GET /webhook, preenchidos à parte.
	Webhook string   `json:"webhook,omitempty"`
	Events  []string `json:"events,omitempty"`
}

func (d *DigiGO) Status(ctx context.Context) (DigiGOStatus, error) {
	var s DigiGOStatus
	err := d.do(ctx, http.MethodGet, "/session/status", nil, &s)
	return s, err
}

// Connect inicia a conexão. Sem sessão anterior, é isto que faz nascer o
// QR. Immediate=true para não travar a requisição por 10s esperando.
func (d *DigiGO) Connect(ctx context.Context) error {
	return d.do(ctx, http.MethodPost, "/session/connect", map[string]any{
		// Só o que o leilão usa: mensagem recebida e eventos de conexão.
		// Assinar "All" traria recibo de leitura e presença de todo mundo,
		// que aqui só viraria tráfego e log.
		"Subscribe": []string{"Message", "Connected", "Disconnected", "LoggedOut"},
		"Immediate": true,
	}, nil)
}

func (d *DigiGO) Disconnect(ctx context.Context) error {
	return d.do(ctx, http.MethodPost, "/session/disconnect", nil, nil)
}

func (d *DigiGO) Logout(ctx context.Context) error {
	return d.do(ctx, http.MethodPost, "/session/logout", nil, nil)
}

// QRCode devolve o QR para parear o aparelho. Vem vazio quando já está
// logado — nesse caso não há o que escanear.
func (d *DigiGO) QRCode(ctx context.Context) (string, error) {
	var r struct {
		QRCode string `json:"QRCode"`
		Qrcode string `json:"qrcode"`
	}
	if err := d.do(ctx, http.MethodGet, "/session/qr", nil, &r); err != nil {
		return "", err
	}
	return firstNonEmpty(r.QRCode, r.Qrcode), nil
}

// SetWebhook aponta o gateway para o nosso endpoint de entrada. É isto
// que evita ter que configurar na mão no painel do DigiGO.
func (d *DigiGO) SetWebhook(ctx context.Context, url string) error {
	return d.do(ctx, http.MethodPost, "/webhook", map[string]any{
		"webhookurl": url,
		"events":     []string{"Message"},
	}, nil)
}

func (d *DigiGO) Webhook(ctx context.Context) (string, []string, error) {
	var r struct {
		WebhookURL string   `json:"webhook"`
		Alt        string   `json:"webhookurl"`
		Events     []string `json:"events"`
		Subscribe  []string `json:"subscribe"`
	}
	if err := d.do(ctx, http.MethodGet, "/webhook", nil, &r); err != nil {
		return "", nil, err
	}
	ev := r.Events
	if len(ev) == 0 {
		ev = r.Subscribe
	}
	return firstNonEmpty(r.WebhookURL, r.Alt), ev, nil
}

// SendText manda a mensagem. O gateway espera o número puro em "Phone" e
// o texto em "Body" — não é o {to, text} genérico.
func (d *DigiGO) SendText(ctx context.Context, jid, text string) error {
	return d.do(ctx, http.MethodPost, "/chat/send/text", map[string]any{
		"Phone": digigoPhone(jid),
		"Body":  text,
	}, nil)
}

// digigoPhone converte o JID interno para o que o gateway espera.
// Conversa normal vira só o número; grupo (@g.us) e newsletter mantêm o
// sufixo, porque aí o destino não é um telefone.
func digigoPhone(jid string) string {
	if strings.Contains(jid, "@g.us") || strings.Contains(jid, "@newsletter") {
		return jid
	}
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		return jid[:i]
	}
	return jid
}
