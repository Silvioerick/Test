package auction

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
)

var ErrNoShippingZone = errors.New("auction: nenhuma zona de frete cobre esse CEP")

// ShippingCalculator decide o frete de um CEP. A implementação padrão usa
// uma tabela de zonas por prefixo configurável no painel; troque por uma
// chamada real aos Correios/Melhor Envio implementando a mesma interface —
// o resto do fluxo (desconto, cobrança) não muda.
type ShippingCalculator interface {
	Quote(ctx context.Context, cep string) (priceCents int64, err error)
}

var cepDigits = regexp.MustCompile(`\D`)

// ZoneShipping escolhe, entre as zonas cadastradas, a de prefixo mais
// específico (mais dígitos) que bate com o CEP — assim uma zona "01" mais
// específica vence uma zona "0" mais genérica para o mesmo CEP.
type ZoneShipping struct{ db *sql.DB }

func NewZoneShipping(db *sql.DB) *ZoneShipping { return &ZoneShipping{db: db} }

func (z *ZoneShipping) Quote(ctx context.Context, cep string) (int64, error) {
	digits := cepDigits.ReplaceAllString(cep, "")
	if len(digits) < 5 {
		return 0, errors.New("auction: CEP inválido")
	}
	var price int64
	err := z.db.QueryRowContext(ctx, `
		SELECT price_cents FROM shipping_zones
		WHERE $1 LIKE cep_prefix || '%'
		ORDER BY length(cep_prefix) DESC LIMIT 1`, digits).Scan(&price)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoShippingZone
	}
	return price, err
}

// ShippingDiscountRule decide quanto abater do frete calculado, dado
// quantos OUTROS lotes esse participante já pagou hoje. Mantido como
// função pura (sem depender do banco) pra ficar fácil de testar e trocar.
//
// Assumo que "mais de 1 lance" no pedido original significa "ganhar mais
// de um lote no mesmo dia" (frete combinado), não "dar vários lances no
// mesmo leilão" — dar lances a mais não muda quantos itens vão na caixa.
// Se a intenção era outra, é só trocar esta função.
type ShippingDiscountRule struct {
	// FreeFromNthItem: a partir de qual item do dia (2 = a partir do
	// segundo) o frete fica de graça. 0 desativa o "grátis" e usa só
	// ExtraItemPercentOff.
	FreeFromNthItem int
	// ExtraItemPercentOff: desconto percentual (0-100) aplicado quando não
	// está no modo "grátis" — ex.: 50 = metade do frete a partir do 2º item.
	ExtraItemPercentOff int
}

func DefaultShippingDiscountRule() ShippingDiscountRule {
	return ShippingDiscountRule{FreeFromNthItem: 2}
}

// Apply devolve o desconto em centavos pro frete `shippingCents`, dado que
// este é o item de número `itemNumber` (1 = primeiro) que a pessoa ganha
// no dia.
func (r ShippingDiscountRule) Apply(shippingCents int64, itemNumber int) int64 {
	if itemNumber < 2 {
		return 0
	}
	if r.FreeFromNthItem > 0 && itemNumber >= r.FreeFromNthItem {
		return shippingCents
	}
	if r.ExtraItemPercentOff > 0 {
		return shippingCents * int64(r.ExtraItemPercentOff) / 100
	}
	return 0
}
