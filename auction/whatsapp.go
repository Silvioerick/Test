package auction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPNotifier envia mensagens de WhatsApp por uma API HTTP (DigiGO ou
// qualquer outra). Substitui o stub que existia no main.go: antes, nenhuma
// mensagem saía de verdade — nem o link de cadastro, nem a cobrança, nem a
// contagem regressiva.
//
// O contrato abaixo é o mais comum entre gateways de WhatsApp
// (POST JSON {to, text}); confirme os nomes dos campos na documentação do
// DigiGO e ajuste TextField/ToField se necessário. Nada mais no sistema
// depende desses nomes — o resto fala com a interface Notifier.
type HTTPNotifier struct {
	// Endpoint recebe o POST de envio de texto, ex.:
	// "https://api.digigo.dev/v1/messages".
	Endpoint string
	// Token vai no header Authorization como "Bearer <token>".
	Token string
	// ToField / TextField: nomes dos campos no corpo JSON. Padrão
	// "to" e "text".
	ToField, TextField string
	HTTPClient         *http.Client
	// Logf opcional para registrar falhas de envio.
	Logf func(string, ...any)
}

func NewHTTPNotifier(endpoint, token string) *HTTPNotifier {
	return &HTTPNotifier{
		Endpoint:   endpoint,
		Token:      token,
		ToField:    "to",
		TextField:  "text",
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		Logf:       func(string, ...any) {},
	}
}

// DBNotifier resolve a configuração do gateway no Postgres a cada envio,
// do mesmo jeito que o DBPaymentProvider faz com a chave de pagamento:
// trocar a URL ou o token do DigiGO no painel vale na próxima mensagem,
// sem redeploy e sem mexer em variável de ambiente.
//
// Enquanto ninguém tiver salvo nada no painel, o SettingsStore ainda
// herda de WHATSAPP_API_URL / WHATSAPP_API_TOKEN, então um deploy que já
// existia continua funcionando.
type DBNotifier struct {
	settings *SettingsStore
	client   *http.Client
	logf     func(string, ...any)
}

func NewDBNotifier(settings *SettingsStore) *DBNotifier {
	return &DBNotifier{
		settings: settings,
		client:   &http.Client{Timeout: 15 * time.Second},
		logf:     func(string, ...any) {},
	}
}

// WithLogger registra os envios quando o gateway ainda não foi
// configurado, para homologação não ficar silenciosa.
func (n *DBNotifier) WithLogger(logf func(string, ...any)) *DBNotifier {
	if logf != nil {
		n.logf = logf
	}
	return n
}

// Client devolve o cliente do gateway com a configuração salva no painel,
// ou erro se ainda não foi configurado.
func (n *DBNotifier) Client(ctx context.Context) (*DigiGO, error) {
	url := n.settings.Get(ctx, SetWhatsAppAPIURL)
	if url == "" {
		return nil, errNoNotifierConfigured
	}
	c := NewDigiGO(url, n.settings.Get(ctx, SetWhatsAppAPIToken))
	c.HTTP = n.client
	return c, nil
}

func (n *DBNotifier) SendText(ctx context.Context, jid, text string) error {
	c, err := n.Client(ctx)
	if err != nil {
		n.logf("[whatsapp nao enviado ->%s] %s", jid, text)
		return err
	}
	return c.SendText(ctx, jid, text)
}

func (n *DBNotifier) SendCharge(ctx context.Context, jid, caption string, charge ChargeResult) error {
	c, err := n.Client(ctx)
	if err != nil {
		n.logf("[whatsapp nao enviado ->%s] %s | pix=%s url=%s", jid, caption, charge.PixCode, charge.PayURL)
		return err
	}
	if err := c.SendText(ctx, jid, caption); err != nil {
		return err
	}
	// O código PIX vai numa mensagem só dele, para dar para copiar num
	// toque sem arrastar a legenda junto.
	if charge.PixCode != "" {
		if err := c.SendText(ctx, jid, charge.PixCode); err != nil {
			return err
		}
	}
	if charge.PayURL != "" {
		if err := c.SendText(ctx, jid, "Pagar: "+charge.PayURL); err != nil {
			return err
		}
	}
	if charge.PixCode == "" && charge.PayURL == "" {
		return fmt.Errorf("whatsapp: cobrança %s sem PIX nem link para enviar", charge.ChargeID)
	}
	return nil
}

func (n *HTTPNotifier) post(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := n.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: enviar mensagem: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp: envio devolveu HTTP %d: %s", resp.StatusCode, string(data))
	}
	return nil
}

func (n *HTTPNotifier) SendText(ctx context.Context, jid, text string) error {
	to, txt := n.ToField, n.TextField
	if to == "" {
		to = "to"
	}
	if txt == "" {
		txt = "text"
	}
	return n.post(ctx, map[string]any{to: jid, txt: text})
}

// SendCharge manda a legenda e, logo em seguida, o que o cliente precisa
// para pagar: o código PIX copia-e-cola numa mensagem só dele (para dar
// para copiar no toque) e/ou o link do checkout.
func (n *HTTPNotifier) SendCharge(ctx context.Context, jid, caption string, charge ChargeResult) error {
	if err := n.SendText(ctx, jid, caption); err != nil {
		return err
	}
	if charge.PixCode != "" {
		if err := n.SendText(ctx, jid, charge.PixCode); err != nil {
			return err
		}
	}
	if charge.PayURL != "" {
		if err := n.SendText(ctx, jid, "Pagar: "+charge.PayURL); err != nil {
			return err
		}
	}
	if charge.PixCode == "" && charge.PayURL == "" {
		return fmt.Errorf("whatsapp: cobrança %s sem PIX nem link para enviar", charge.ChargeID)
	}
	return nil
}

// --- entrada: mensagens do WhatsApp viram lances ------------------------

// InboundMessage é a mensagem recebida do gateway, já normalizada. O
// parser aceita os nomes de campo mais comuns para não amarrar o sistema
// ao formato exato do DigiGO.
type InboundMessage struct {
	MsgID   string // id da mensagem no WhatsApp — é o que deduplica reentregas
	From    string // JID de quem mandou
	Channel string // grupo (ou número do bot) em que a mensagem caiu
	Text    string
}

// whatsmeowEvent é o formato que o DIGIGO (base whatsmeow) entrega:
// {"type":"Message","event":{"Info":{...},"Message":{...}}}.
type whatsmeowEvent struct {
	Type  string `json:"type"`
	Event struct {
		Info struct {
			ID       string `json:"ID"`
			Chat     string `json:"Chat"`
			Sender   string `json:"Sender"`
			PushName string `json:"PushName"`
			IsFromMe bool   `json:"IsFromMe"`
			IsGroup  bool   `json:"IsGroup"`
			// Alguns builds entregam o remetente já em SenderAlt/LID.
			SenderAlt string `json:"SenderAlt"`
		} `json:"Info"`
		Message struct {
			Conversation    string                   `json:"conversation"`
			ExtendedText    struct{ Text string }    `json:"extendedTextMessage"`
			ImageCaption    struct{ Caption string } `json:"imageMessage"`
			DocumentCaption struct{ Caption string } `json:"documentMessage"`
		} `json:"Message"`
	} `json:"event"`
}

// parseWhatsmeow devolve ok=false quando o corpo não é desse formato.
func parseWhatsmeow(data []byte) (InboundMessage, bool) {
	var e whatsmeowEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return InboundMessage{}, false
	}
	i := e.Event.Info
	if i.ID == "" && i.Sender == "" {
		return InboundMessage{}, false
	}
	// Mensagem que o próprio bot mandou não é lance de ninguém.
	if i.IsFromMe {
		return InboundMessage{}, false
	}
	texto := firstNonEmpty(e.Event.Message.Conversation, e.Event.Message.ExtendedText.Text,
		e.Event.Message.ImageCaption.Caption, e.Event.Message.DocumentCaption.Caption)
	m := InboundMessage{
		MsgID:   i.ID,
		From:    firstNonEmpty(i.Sender, i.SenderAlt),
		Channel: firstNonEmpty(i.Chat, i.Sender),
		Text:    texto,
	}
	if m.MsgID == "" || m.From == "" || m.Text == "" {
		return InboundMessage{}, false
	}
	return m, true
}

// inboundPayload aceita as grafias usuais dos gateways de WhatsApp.
type inboundPayload struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	Key       struct {
		ID          string `json:"id"`
		RemoteJID   string `json:"remoteJid"`
		Participant string `json:"participant"`
	} `json:"key"`
	From        string `json:"from"`
	Sender      string `json:"sender"`
	Participant string `json:"participant"`
	To          string `json:"to"`
	Chat        string `json:"chat"`
	ChatID      string `json:"chat_id"`
	GroupJID    string `json:"group_jid"`
	Text        string `json:"text"`
	Body        string `json:"body"`
	Message     struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
	} `json:"message"`
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ParseInbound normaliza o corpo do webhook do gateway. Em grupo, o autor
// vem em "participant"/"key.participant" e o canal em "key.remoteJid"; em
// conversa privada os dois coincidem.
func ParseInbound(data []byte) (InboundMessage, error) {
	// Formato do DIGIGO/whatsmeow primeiro, que é o do gateway em uso.
	if m, ok := parseWhatsmeow(data); ok {
		return m, nil
	}
	var p inboundPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return InboundMessage{}, err
	}
	m := InboundMessage{
		MsgID:   firstNonEmpty(p.ID, p.MessageID, p.Key.ID),
		From:    firstNonEmpty(p.Key.Participant, p.Participant, p.Sender, p.From),
		Channel: firstNonEmpty(p.GroupJID, p.Key.RemoteJID, p.ChatID, p.Chat, p.To, p.From),
		Text:    firstNonEmpty(p.Text, p.Body, p.Message.Conversation, p.Message.Text),
	}
	if m.From == "" || m.Text == "" {
		return InboundMessage{}, fmt.Errorf("auction: mensagem sem remetente ou sem texto")
	}
	if m.Channel == "" {
		m.Channel = m.From
	}
	if m.MsgID == "" {
		return InboundMessage{}, fmt.Errorf("auction: mensagem sem id — sem ele não dá para deduplicar reentregas")
	}
	return m, nil
}

// BidOutcome é o resultado de processar uma mensagem recebida: o que
// responder e se virou lance.
type BidOutcome struct {
	Handled bool // false = mensagem não era um lance, ignorada
	Reply   string
	Result  BidResult
}

// HandleInbound é a ponta que faltava: traduz uma mensagem de WhatsApp em
// um lance no lote que está aberto naquele canal, e devolve o que
// responder. Antes disso, PlaceBid e ParseBid não eram chamados por
// nenhum código de produção — o sistema não tinha por onde receber lance.
//
// maxDefaults > 0 barra quem já deu calote essa quantidade de vezes.
func (o *Orchestrator) HandleInbound(ctx context.Context, engine *Engine, m InboundMessage, maxDefaults int) (BidOutcome, error) {
	cents, plus, ok := ParseBid(m.Text)
	if !ok {
		return BidOutcome{}, nil // conversa normal do grupo, não é lance
	}
	lotID, _, err := o.store.ActiveLotForChannel(ctx, m.Channel)
	if err == ErrNoActiveLot {
		return BidOutcome{Handled: true, Reply: "Não tem leilão aberto agora."}, nil
	}
	if err != nil {
		return BidOutcome{}, err
	}
	if maxDefaults > 0 {
		blocked, err := o.store.HasDefaulted(ctx, m.From, maxDefaults)
		if err != nil {
			o.logf("auction: checar calotes de %s: %v", m.From, err)
		} else if blocked {
			return BidOutcome{Handled: true,
				Reply: "Seus lances estão bloqueados por calote em leilão anterior. Fale com a gente para regularizar."}, nil
		}
	}
	res, err := engine.PlaceBid(ctx, lotID, m.From, cents, plus, m.MsgID)
	if err != nil {
		return BidOutcome{}, err
	}
	return BidOutcome{Handled: true, Reply: replyFor(res), Result: res}, nil
}

func replyFor(r BidResult) string {
	switch r.Status {
	case Accepted:
		s := fmt.Sprintf("Lance de %s registrado! Você está na frente. Faltam %s.",
			formatCents(r.Amount), formatRemaining(r.Remaining))
		if r.Extended {
			s += " (tempo estendido)"
		}
		return s
	case TooLow:
		return fmt.Sprintf("Lance baixo demais. O mínimo agora é %s — mande esse valor ou \"+\".", formatCents(r.Amount))
	case TooHigh:
		return fmt.Sprintf("Lance acima do teto deste lote (%s).", formatCents(r.Amount))
	case AlreadyLeading:
		return fmt.Sprintf("Você já está na frente com %s.", formatCents(r.Amount))
	case Closed:
		return "Esse leilão já encerrou."
	case NotFound:
		return "Não tem leilão aberto agora."
	case Duplicate:
		return "" // reentrega: não responde de novo
	}
	return ""
}
