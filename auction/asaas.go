package auction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// customerRef é a chave estável da pessoa na Asaas: o número do WhatsApp
// sem o sufixo do JID.
func customerRef(jid string) string {
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		return "wa-" + jid[:i]
	}
	return "wa-" + jid
}

// centsToReais converte centavos para reais sem passar por aritmética de
// ponto flutuante: o valor é montado como texto e relido, então 1234567
// vira exatamente 12345.67 e nunca 12345.669999999999.
func centsToReais(cents int64) float64 {
	sign := int64(1)
	if cents < 0 {
		sign, cents = -1, -cents
	}
	v, _ := strconv.ParseFloat(fmt.Sprintf("%d.%02d", cents/100, cents%100), 64)
	return float64(sign) * v
}

// AsaasProvider cobra via Asaas com billingType "UNDEFINED": a Asaas gera
// uma página de checkout hospedada (invoiceUrl) onde o cliente escolhe PIX,
// boleto ou cartão — nenhum dado de cartão passa pelo seu servidor.
//
// Detalhes confirmados na documentação oficial da Asaas (setembro/2026):
//   - Autenticação: header "access_token" (não "Authorization: Bearer").
//   - POST /v3/customers exige name e cpfCnpj.
//   - POST /v3/payments exige customer, billingType, value, dueDate (a data
//     é só granularidade de dia — não impede o cliente de pagar na hora;
//     quem decide o prazo curto do leilão é o expires_at do seu Postgres,
//     não o dueDate da Asaas).
//   - Confirmação chega via webhook nos eventos PAYMENT_CONFIRMED (cartão)
//     e PAYMENT_RECEIVED (PIX/boleto liquidado) — configure webhook.events
//     com os dois. Cadastre o webhook manualmente no painel da Asaas ou via
//     POST /v3/webhooks; o payload chega com o token que você definir em
//     authToken — confirme no painel da Asaas o header exato em que ele
//     volta antes de validar assinatura em produção.
//
// BaseURL: use "https://api-sandbox.asaas.com" pra testar; troque pelo
// host de produção informado no seu painel Asaas antes de ir ao ar (não
// hardcodei porque não confirmei esse valor na documentação consultada).
type AsaasProvider struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func NewAsaasProvider(baseURL, apiKey string) *AsaasProvider {
	return &AsaasProvider{BaseURL: baseURL, APIKey: apiKey, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

func (a *AsaasProvider) do(ctx context.Context, method, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("access_token", a.APIKey)
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("asaas: request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("asaas: %s %s -> HTTP %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("asaas: decodificar resposta de %s: %w", path, err)
		}
	}
	return nil
}

type asaasCustomer struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	CpfCnpj           string `json:"cpfCnpj,omitempty"`
	Email             string `json:"email,omitempty"`
	MobilePhone       string `json:"mobilePhone,omitempty"`
	ExternalReference string `json:"externalReference,omitempty"`
}

type asaasCustomerList struct {
	Data []asaasCustomer `json:"data"`
}

// findOrCreateCustomer procura pelo externalReference (o JID do WhatsApp,
// que é estável) antes de criar, pra não duplicar cliente na Asaas a cada
// compra da mesma pessoa.
func (a *AsaasProvider) findOrCreateCustomer(ctx context.Context, externalRef string, c Customer) (string, error) {
	var list asaasCustomerList
	q := url.Values{"externalReference": {externalRef}}
	if err := a.do(ctx, http.MethodGet,
		"/v3/customers?"+q.Encode(), nil, &list); err == nil && len(list.Data) > 0 {
		return list.Data[0].ID, nil
	}
	var created asaasCustomer
	err := a.do(ctx, http.MethodPost, "/v3/customers", asaasCustomer{
		Name: c.Name, CpfCnpj: c.Document, Email: c.Email,
		MobilePhone: localPhone(c.Phone), ExternalReference: externalRef,
	}, &created)
	if err != nil {
		return "", fmt.Errorf("criar cliente na Asaas: %w", err)
	}
	return created.ID, nil
}

type asaasPaymentRequest struct {
	Customer          string  `json:"customer"`
	BillingType       string  `json:"billingType"`
	Value             float64 `json:"value"`
	DueDate           string  `json:"dueDate"`
	Description       string  `json:"description,omitempty"`
	ExternalReference string  `json:"externalReference,omitempty"`
}

type asaasPaymentResponse struct {
	ID         string `json:"id"`
	InvoiceURL string `json:"invoiceUrl"`
	Status     string `json:"status"`
}

func (a *AsaasProvider) CreateCharge(ctx context.Context, reference string, amountCents int64, customer Customer) (ChargeResult, error) {
	if customer.Document == "" {
		return ChargeResult{}, fmt.Errorf("asaas: CPF/CNPJ do cliente é obrigatório")
	}
	// A referência estável da pessoa é o JID do WhatsApp, não a do pedido.
	// Passar reference ("lot-X-order-N") criava um cliente novo na Asaas a
	// cada compra, apesar de o comentário abaixo dizer o contrário.
	custID, err := a.findOrCreateCustomer(ctx, customerRef(customer.Phone), customer)
	if err != nil {
		return ChargeResult{}, err
	}
	var pay asaasPaymentResponse
	err = a.do(ctx, http.MethodPost, "/v3/payments", asaasPaymentRequest{
		Customer:    custID,
		BillingType: "UNDEFINED", // cliente escolhe PIX/boleto/cartão na página da Asaas
		Value:       centsToReais(amountCents),
		// Amanhã, não hoje: dueDate tem granularidade de dia e o fuso do
		// container pode fazer "hoje" nascer vencido. Quem manda no prazo
		// curto do leilão é o expires_at do nosso Postgres, não isto.
		DueDate:           time.Now().AddDate(0, 0, 1).Format("2006-01-02"),
		Description:       "Leilão " + reference,
		ExternalReference: reference,
	}, &pay)
	if err != nil {
		return ChargeResult{}, fmt.Errorf("criar cobrança na Asaas: %w", err)
	}
	return ChargeResult{ChargeID: pay.ID, PayURL: pay.InvoiceURL}, nil
}

// AsaasWebhookEvent é o formato que a Asaas envia no corpo do webhook.
// Configure o webhook (painel ou POST /v3/webhooks) para pelo menos os
// eventos PAYMENT_CONFIRMED e PAYMENT_RECEIVED.
type AsaasWebhookEvent struct {
	Event   string `json:"event"`
	Payment struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"payment"`
}

// IsPaidEvent reconhece os eventos que significam "dinheiro confirmado".
func (e AsaasWebhookEvent) IsPaidEvent() bool {
	return e.Event == "PAYMENT_CONFIRMED" || e.Event == "PAYMENT_RECEIVED"
}

// localPhone tira o sufixo "@s.whatsapp.net" e o código do país, que a
// Asaas não espera em mobilePhone.
func localPhone(jid string) string {
	n := jid
	if i := strings.IndexByte(n, '@'); i >= 0 {
		n = n[:i]
	}
	if len(n) > 11 && strings.HasPrefix(n, "55") {
		n = n[2:]
	}
	return n
}
