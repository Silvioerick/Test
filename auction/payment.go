package auction

import "context"

// Customer é o que um provedor de cobrança pode exigir do titular da
// cobrança. HubPay (PIX simples) pode ignorar Document/Email; a Asaas
// EXIGE Document (CPF/CNPJ) pra criar o cliente — por isso o cadastro de
// endereço acontece antes de qualquer cobrança ser gerada.
type Customer struct {
	Name     string
	Document string // CPF ou CNPJ, só dígitos
	Email    string
	Phone    string // formato local, ex.: "11999999999"
}

// ChargeResult é o que o cliente recebe pra pagar. Nem todo provedor
// preenche os dois campos: HubPay tende a só dar PixCode (QR copia-e-cola);
// um checkout hospedado (Asaas com billingType UNDEFINED) só dá PayURL.
// O Notifier manda o que existir.
type ChargeResult struct {
	ChargeID string // id da cobrança no provedor — usado para casar com o webhook
	PixCode  string // payload PIX copia-e-cola, quando o provedor gera na hora
	PayURL   string // link de checkout hospedado (PIX/boleto/cartão), quando houver
}

// PaymentProvider cria a cobrança pelo valor total (lance + frete -
// desconto). Implemente contra a API real do gateway escolhido.
type PaymentProvider interface {
	CreateCharge(ctx context.Context, reference string, amountCents int64, customer Customer) (ChargeResult, error)
}

// Notifier envia mensagens ao participante via WhatsApp. Implemente contra
// a API do DigiGO.
type Notifier interface {
	SendText(ctx context.Context, jid, text string) error
	// SendCharge manda a cobrança pro cliente: PIX (QR/copia-e-cola), link
	// de checkout, ou os dois — o que vier preenchido em ChargeResult.
	SendCharge(ctx context.Context, jid, caption string, charge ChargeResult) error
}
