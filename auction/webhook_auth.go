package auction

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxWebhookBody limita o corpo que aceitamos ler de um webhook. Sem isso
// um POST gigante consome memória do processo à vontade.
const maxWebhookBody = 1 << 20 // 1 MiB

var (
	ErrWebhookUnconfigured = errors.New("auction: webhook sem segredo configurado")
	ErrWebhookSignature    = errors.New("auction: assinatura de webhook inválida")
)

// WebhookAuth guarda os segredos que provam que um webhook de pagamento
// veio mesmo do provedor.
//
// Isto existe porque, sem ele, QUALQUER PESSOA que conheça um charge_id
// marca o pedido como pago — e o próprio comprador conhece o charge_id
// dele, que vem na URL do checkout. Era produto saindo de graça.
//
// Política: falha FECHADA. Um provedor sem segredo configurado tem o
// webhook recusado com 503, em vez de aceitar qualquer corpo.
type WebhookAuth struct {
	// HubPaySecret: segredo compartilhado. Esperamos HMAC-SHA256 do corpo
	// cru, em hex, no header HubPaySigHeader.
	HubPaySecret string
	// HubPaySigHeader: header que carrega a assinatura. Padrão
	// "X-Hubpay-Signature". Ajuste conforme a documentação da HubPay.
	HubPaySigHeader string
	// AsaasToken: o valor que você definiu em authToken ao cadastrar o
	// webhook na Asaas. A Asaas devolve esse token num header a cada
	// entrega; conferimos contra a lista asaasTokenHeaders.
	AsaasToken string
	// AsaasTokenHeader: se você confirmou no painel da Asaas qual é o
	// header exato, fixe aqui. Vazio = tenta os nomes conhecidos.
	AsaasTokenHeader string
}

// asaasTokenHeaders são os nomes de header sob os quais a Asaas já
// entregou o authToken. Conferimos todos porque o nome exato não estava
// confirmado na documentação consultada — configure AsaasTokenHeader
// assim que confirmar o seu, para deixar a checagem estrita.
var asaasTokenHeaders = []string{
	"asaas-access-token",
	"Asaas-Access-Token",
	"access_token",
	"asaas-signature",
}

// readSignedBody lê o corpo (com limite) sem consumir a capacidade de
// verificá-lo: devolve os bytes crus, que é o que precisa ser assinado.
func readSignedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
}

// VerifyHubPay confere o HMAC-SHA256 do corpo cru.
func (a WebhookAuth) VerifyHubPay(r *http.Request, body []byte) error {
	if a.HubPaySecret == "" {
		return ErrWebhookUnconfigured
	}
	header := a.HubPaySigHeader
	if header == "" {
		header = "X-Hubpay-Signature"
	}
	got := strings.TrimSpace(r.Header.Get(header))
	// Aceita tanto "abc123" quanto "sha256=abc123".
	if i := strings.IndexByte(got, '='); i >= 0 && strings.HasPrefix(strings.ToLower(got), "sha256=") {
		got = got[i+1:]
	}
	if got == "" {
		return ErrWebhookSignature
	}
	mac := hmac.New(sha256.New, []byte(a.HubPaySecret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) != 1 {
		return ErrWebhookSignature
	}
	return nil
}

// VerifyAsaas confere o authToken que a Asaas reenvia a cada entrega.
func (a WebhookAuth) VerifyAsaas(r *http.Request) error {
	if a.AsaasToken == "" {
		return ErrWebhookUnconfigured
	}
	headers := asaasTokenHeaders
	if a.AsaasTokenHeader != "" {
		headers = []string{a.AsaasTokenHeader}
	}
	for _, h := range headers {
		got := strings.TrimSpace(r.Header.Get(h))
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.AsaasToken)) == 1 {
			return nil
		}
	}
	return ErrWebhookSignature
}

// resolveFromSettings monta o WebhookAuth a partir da configuração do
// painel (com fallback para as variáveis de ambiente antigas). É chamado
// a cada webhook, o que permite trocar o segredo na tela sem redeploy —
// o SettingsStore tem cache curto, então isso não vira consulta ao banco
// por requisição.
func resolveFromSettings(ctx context.Context, s *SettingsStore) WebhookAuth {
	if s == nil {
		return WebhookAuth{}
	}
	return WebhookAuth{
		HubPaySecret:     s.Get(ctx, SetHubPayWebhookSecret),
		HubPaySigHeader:  s.Get(ctx, SetHubPaySigHeader),
		AsaasToken:       s.Get(ctx, SetAsaasWebhookToken),
		AsaasTokenHeader: s.Get(ctx, SetAsaasTokenHeader),
	}
}
