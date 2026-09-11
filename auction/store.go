package auction

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoOrder: não existe pedido com esse charge_id. Provavelmente o
	// AttachCharge ainda não rodou (webhook chegou antes) — o chamador deve
	// devolver erro ao provedor para forçar a reentrega.
	ErrNoOrder = errors.New("auction: pedido de pagamento não encontrado")
	// ErrOrderAlreadyPaid: reentrega de um webhook já processado. Sucesso.
	ErrOrderAlreadyPaid = errors.New("auction: pedido já estava pago")
	// ErrOrderNotPending: pagamento chegou depois de o pedido ter expirado
	// e o produto ter voltado pro estoque. Exige reconciliação manual.
	ErrOrderNotPending = errors.New("auction: pagamento confirmado para pedido que não está mais pendente")
	ErrTokenInvalid    = errors.New("auction: link de cadastro inválido, expirado ou já usado")
	// ErrProductUnavailable: tentaram leiloar um produto que não está no
	// estoque disponível (já em leilão ou já vendido).
	ErrProductUnavailable = errors.New("auction: produto não está disponível para leilão")
	ErrLotNotFound        = errors.New("auction: lote não encontrado")
	ErrLotPaid            = errors.New("auction: lote já foi pago — o caso é de estorno, não de cancelamento")
	ErrLotNotCancellable  = errors.New("auction: lote já está encerrado")
)

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// upsertParticipant garante o registro do apostador pelo JID do WhatsApp e
// devolve o id interno. name pode vir vazio; se vier, atualiza o cadastro.
// Usado pelo caminho de lance, que não precisa saber se o endereço já foi
// preenchido.
func upsertParticipant(ctx context.Context, q querier, jid, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO participants (whatsapp_jid, name) VALUES ($1, NULLIF($2, ''))
		ON CONFLICT (whatsapp_jid) DO UPDATE
			SET name = COALESCE(NULLIF(EXCLUDED.name, ''), participants.name)
		RETURNING id`, jid, name).Scan(&id)
	return id, err
}

// CreateLot registra o lote no Postgres. Chame antes (ou logo após) de
// Engine.Create; os dois compartilham o mesmo id de lote.
func (s *Store) CreateLot(ctx context.Context, lotID string, productID int64, c Config, paymentWindow time.Duration, channelJID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Reserva o produto ANTES de criar o lote: se ele não estiver
	// disponível (já em leilão, vendido), nada acontece. Junto com o índice
	// parcial lots_one_open_per_product, isso impede dois leilões abertos do
	// mesmo item físico mesmo com dois cliques simultâneos no painel.
	res, err := tx.ExecContext(ctx, `
		UPDATE products SET status = 'in_auction' WHERE id = $1 AND status = 'available'`, productID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProductUnavailable
	}
	var maxBid any
	if c.MaxBid > 0 {
		maxBid = c.MaxBid
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO lots (id, product_id, start_price, min_increment, payment_ms, channel_jid, max_bid)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)`,
		lotID, productID, c.StartPrice, c.MinIncrement, paymentWindow.Milliseconds(),
		channelJID, maxBid); err != nil {
		return fmt.Errorf("criar lote: %w", err)
	}
	return tx.Commit()
}

// RecordBid persiste um lance. Idempotente por (lot_id, msg_id): uma
// reentrega do webhook do WhatsApp não gera linha duplicada NEM incrementa
// bid_count de novo. current_amount só sobe — um evento processado fora de
// ordem nunca faz o lote "voltar" para um valor menor.
func (s *Store) RecordBid(ctx context.Context, lotID, jid, name string, amount int64, msgID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pid, err := upsertParticipant(ctx, tx, jid, name)
	if err != nil {
		return fmt.Errorf("upsert participante: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO bids (lot_id, participant_id, amount, msg_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (lot_id, msg_id) DO NOTHING`, lotID, pid, amount, msgID)
	if err != nil {
		return fmt.Errorf("inserir lance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Reentrega da mesma mensagem: nada a contabilizar.
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE lots SET current_amount = GREATEST(current_amount, $2), bid_count = bid_count + 1
		WHERE id = $1`, lotID, amount); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelLot cancela um lote e devolve o produto ao estoque. Serve para o
// erro de digitação: abriu com o valor errado, o produto errado, o canal
// errado. Só vale enquanto ninguém pagou — depois disso o dinheiro já
// entrou e o caso é de estorno, não de cancelamento.
//
// Os lances ficam gravados: o histórico do que aconteceu não se apaga por
// causa de um cancelamento administrativo.
func (s *Store) CancelLot(ctx context.Context, lotID, motivo string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status::text FROM lots WHERE id = $1 FOR UPDATE`, lotID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLotNotFound
		}
		return err
	}
	switch status {
	case "paid":
		return ErrLotPaid
	case "cancelled", "unsold":
		return ErrLotNotCancellable
	}

	// Um pedido de pagamento em aberto morre junto: ninguém deve pagar
	// por um lote cancelado.
	if _, err := tx.ExecContext(ctx, `
		UPDATE payment_orders SET status = 'cancelled'
		WHERE lot_id = $1 AND status = 'pending'`, lotID); err != nil {
		return err
	}
	// Idem para um link de cadastro pendente.
	if _, err := tx.ExecContext(ctx, `
		UPDATE registration_tokens SET expired_at = now()
		WHERE lot_id = $1 AND used_at IS NULL AND expired_at IS NULL`, lotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE lots SET status = 'cancelled', closed_at = COALESCE(closed_at, now()),
			cancel_reason = NULLIF($2, '') WHERE id = $1`, lotID, motivo); err != nil {
		return err
	}
	if err := releaseProductTx(ctx, tx, lotID); err != nil {
		return err
	}
	return tx.Commit()
}

// AbortLot desfaz um lote que foi criado no Postgres mas não conseguiu
// subir no Redis: apaga o registro e devolve o produto ao estoque. Sem
// isso o produto ficava preso em 'in_auction' para sempre.
func (s *Store) AbortLot(ctx context.Context, lotID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := releaseProductTx(ctx, tx, lotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM lots WHERE id = $1`, lotID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetWinningAmount grava no Postgres o lance vencedor apurado pelo Redis.
// O Redis é a autoridade sobre quanto o vencedor ofereceu; o Postgres pode
// ter perdido algum evento de lance sob pressão de fila, então na hora de
// cobrar o valor é reconciliado a partir daqui.
func (s *Store) SetWinningAmount(ctx context.Context, lotID string, amount int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE lots SET current_amount = GREATEST(current_amount, $2) WHERE id = $1`, lotID, amount)
	return err
}

// CloseNoBids fecha um lote que não recebeu nenhum lance.
func (s *Store) CloseNoBids(ctx context.Context, lotID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE lots SET status = 'unsold', closed_at = now() WHERE id = $1`, lotID); err != nil {
		return err
	}
	if err := releaseProduct(ctx, tx, lotID); err != nil {
		return err
	}
	return tx.Commit()
}

// ParticipantInfo é o que o orquestrador precisa saber sobre o vencedor
// pra decidir se já pode cobrar (cadastro completo) ou se precisa mandar o
// link de cadastro primeiro.
type ParticipantInfo struct {
	ID         int64
	Registered bool
	Customer   Customer
	CEP        string
}

// GetOrCreateParticipant garante o participante pelo JID e devolve se o
// cadastro (endereço + documento) já está completo.
func (s *Store) GetOrCreateParticipant(ctx context.Context, jid, name string) (ParticipantInfo, error) {
	var p ParticipantInfo
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO participants (whatsapp_jid, name) VALUES ($1, NULLIF($2, ''))
		ON CONFLICT (whatsapp_jid) DO UPDATE
			SET name = COALESCE(NULLIF(EXCLUDED.name, ''), participants.name)
		RETURNING id, registered_at IS NOT NULL,
			COALESCE(full_name, ''), COALESCE(document, ''), COALESCE(email, ''),
			COALESCE(address_cep, '')`,
		jid, name).Scan(&p.ID, &p.Registered, &p.Customer.Name, &p.Customer.Document, &p.Customer.Email, &p.CEP)
	p.Customer.Phone = jid
	return p, err
}

// OpenRegistration marca o lote como aguardando cadastro e já gera o token
// de uso único numa única transação — assim ninguém observa o status
// mudado com o token ainda não existindo.
func (s *Store) OpenRegistration(ctx context.Context, lotID string, participantID int64, ttl time.Duration) (token string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `
		UPDATE lots SET status = 'awaiting_registration', claimant_id = $2,
			closed_at = COALESCE(closed_at, now()) WHERE id = $1`, lotID, participantID); err != nil {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err = rand.Read(buf); err != nil {
		return "", err
	}
	token = hex.EncodeToString(buf)
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO registration_tokens (token, participant_id, lot_id, expires_at)
		VALUES ($1, $2, $3, now() + ($4 || ' milliseconds')::interval)`,
		token, participantID, lotID, ttl.Milliseconds()); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

type RegistrationContext struct {
	Token         string
	ParticipantID int64
	LotID         string
	JID           string
	ProductName   string
	BidAmount     int64
}

// GetRegistrationContext valida o token (sem consumir) e devolve o
// contexto pra a página pública mostrar "você ganhou X, endereço de Y".
func (s *Store) GetRegistrationContext(ctx context.Context, token string) (RegistrationContext, error) {
	var c RegistrationContext
	c.Token = token
	err := s.db.QueryRowContext(ctx, `
		SELECT rt.participant_id, rt.lot_id, p.whatsapp_jid, pr.name, l.current_amount
		FROM registration_tokens rt
		JOIN participants p ON p.id = rt.participant_id
		JOIN lots l ON l.id = rt.lot_id
		JOIN products pr ON pr.id = l.product_id
		WHERE rt.token = $1 AND rt.used_at IS NULL AND rt.expired_at IS NULL AND rt.expires_at > now()`,
		token).Scan(&c.ParticipantID, &c.LotID, &c.JID, &c.ProductName, &c.BidAmount)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistrationContext{}, ErrTokenInvalid
	}
	return c, err
}

// CustomerForm é o que a pessoa preenche no formulário público.
type CustomerForm struct {
	Name         string
	Document     string
	Email        string
	CEP          string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string
}

// CompleteRegistration consome o token, grava o cadastro e devolve o
// contexto necessário pra calcular frete e cobrar em seguida.
func (s *Store) CompleteRegistration(ctx context.Context, token string, form CustomerForm) (RegistrationContext, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RegistrationContext{}, err
	}
	defer tx.Rollback()
	var c RegistrationContext
	c.Token = token
	err = tx.QueryRowContext(ctx, `
		SELECT rt.participant_id, rt.lot_id, p.whatsapp_jid, pr.name, l.current_amount
		FROM registration_tokens rt
		JOIN participants p ON p.id = rt.participant_id
		JOIN lots l ON l.id = rt.lot_id
		JOIN products pr ON pr.id = l.product_id
		WHERE rt.token = $1 AND rt.used_at IS NULL AND rt.expired_at IS NULL AND rt.expires_at > now()
		FOR UPDATE OF rt`,
		token).Scan(&c.ParticipantID, &c.LotID, &c.JID, &c.ProductName, &c.BidAmount)
	if errors.Is(err, sql.ErrNoRows) {
		return RegistrationContext{}, ErrTokenInvalid
	}
	if err != nil {
		return RegistrationContext{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE registration_tokens SET used_at = now() WHERE token = $1`, token); err != nil {
		return RegistrationContext{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE participants SET
			full_name = $2, document = $3, email = NULLIF($4, ''),
			address_cep = $5, address_street = $6, address_number = $7,
			address_complement = NULLIF($8, ''), address_neighborhood = $9,
			address_city = $10, address_state = $11, registered_at = now()
		WHERE id = $1`,
		c.ParticipantID, form.Name, form.Document, form.Email,
		form.CEP, form.Street, form.Number, form.Complement, form.Neighborhood,
		form.City, form.State); err != nil {
		return RegistrationContext{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegistrationContext{}, err
	}
	return c, nil
}

// CountPaidToday conta quantos outros lotes esse participante já pagou
// hoje — usado pra decidir o desconto de frete por múltiplos itens.
func (s *Store) CountPaidToday(ctx context.Context, participantID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM payment_orders
		WHERE participant_id = $1 AND status = 'paid' AND paid_at::date = now()::date`,
		participantID).Scan(&n)
	return n, err
}

// AssignClaimant abre o pedido de pagamento do vencedor já com frete e
// desconto calculados. bidAmount é o lance vencedor puro; o total cobrado
// (payment_orders.amount) é bidAmount + shippingCents - discountCents.
func (s *Store) AssignClaimant(ctx context.Context, lotID string, participantID int64, bidAmount, shippingCents, discountCents int64) (orderID int64, totalAmount int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	total := bidAmount + shippingCents - discountCents
	if _, err = tx.ExecContext(ctx, `
		UPDATE lots SET status = 'awaiting_payment', claimant_id = $2,
			shipping_cents = $3, discount_cents = $4,
			closed_at = COALESCE(closed_at, now()) WHERE id = $1`,
		lotID, participantID, shippingCents, discountCents); err != nil {
		return 0, 0, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO payment_orders (lot_id, participant_id, amount, expires_at)
		SELECT $1, $2, $3, now() + (l.payment_ms || ' milliseconds')::interval
		FROM lots l WHERE l.id = $1
		RETURNING id`, lotID, participantID, total).Scan(&orderID)
	if err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return orderID, total, nil
}

// AttachCharge grava a cobrança criada no provedor (HubPay, Asaas, ...)
// assim que ela é gerada e REINICIA o prazo de pagamento a partir de agora.
// O relógio do cliente só pode começar a correr quando ele de fato tem uma
// cobrança na mão — se o provedor demorou (ou caiu e só voltou no retry),
// o prazo não pode já nascer consumido.
func (s *Store) AttachCharge(ctx context.Context, orderID int64, provider string, charge ChargeResult) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE payment_orders po SET
			provider = $2, charge_id = $3,
			qr_code = NULLIF($4, ''), pay_url = NULLIF($5, ''),
			last_charge_error = NULL,
			expires_at = now() + (l.payment_ms || ' milliseconds')::interval
		FROM lots l
		WHERE po.id = $1 AND l.id = po.lot_id`,
		orderID, provider, charge.ChargeID, charge.PixCode, charge.PayURL)
	return err
}

// RecordChargeFailure guarda por que a cobrança não pôde ser gerada, para
// o worker de retry e para o painel.
func (s *Store) RecordChargeFailure(ctx context.Context, orderID int64, cause string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE payment_orders SET charge_attempts = charge_attempts + 1, last_charge_error = $2
		WHERE id = $1`, orderID, cause)
	return err
}

// PendingWithoutCharge lista pedidos pendentes cuja cobrança ainda não foi
// gerada (provedor fora do ar na hora). São eles que o worker de retry
// tenta de novo — antes deste conserto, viravam calote em silêncio.
func (s *Store) PendingWithoutCharge(ctx context.Context, limit, maxAttempts int) ([]PendingCharge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT po.id, po.lot_id, po.participant_id, po.amount, p.whatsapp_jid,
		       COALESCE(p.full_name, p.name, ''), COALESCE(p.document, ''),
		       COALESCE(p.email, ''), l.current_amount, l.shipping_cents, l.discount_cents
		FROM payment_orders po
		JOIN participants p ON p.id = po.participant_id
		JOIN lots l ON l.id = po.lot_id
		WHERE po.status = 'pending' AND po.charge_id IS NULL AND po.charge_attempts < $2
		ORDER BY po.created_at LIMIT $1`, limit, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingCharge
	for rows.Next() {
		var c PendingCharge
		if err := rows.Scan(&c.OrderID, &c.LotID, &c.ParticipantID, &c.Amount, &c.JID,
			&c.Customer.Name, &c.Customer.Document, &c.Customer.Email,
			&c.BidAmount, &c.ShippingCents, &c.DiscountCents); err != nil {
			return nil, err
		}
		c.Customer.Phone = c.JID
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingCharge é um pedido pendente esperando que a cobrança seja gerada.
type PendingCharge struct {
	OrderID       int64
	LotID         string
	ParticipantID int64
	Amount        int64
	JID           string
	Customer      Customer
	BidAmount     int64
	ShippingCents int64
	DiscountCents int64
}

// ExhaustedCharges lista os pedidos que já esgotaram as tentativas de
// cobrança. Separado de PendingWithoutCharge de propósito: misturar os
// dois faria o worker desistir de pedidos que ainda tinham retry.
func (s *Store) ExhaustedCharges(ctx context.Context, limit, maxAttempts int) ([]PendingCharge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT po.id, po.lot_id, po.participant_id, po.amount, p.whatsapp_jid
		FROM payment_orders po
		JOIN participants p ON p.id = po.participant_id
		WHERE po.status = 'pending' AND po.charge_id IS NULL AND po.charge_attempts >= $2
		ORDER BY po.created_at LIMIT $1`, limit, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingCharge
	for rows.Next() {
		var c PendingCharge
		if err := rows.Scan(&c.OrderID, &c.LotID, &c.ParticipantID, &c.Amount, &c.JID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimStaleLot marca um lote 'open' como 'closed' de forma atômica, para
// que só uma réplica (e só uma vez) conclua o fluxo dele na reconciliação.
func (s *Store) ClaimStaleLot(ctx context.Context, lotID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE lots SET status = 'closed' WHERE id = $1 AND status = 'open'`, lotID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseStaleLotClaim devolve o lote para 'open' se a conclusão falhou,
// para a próxima passada tentar de novo em vez de deixá-lo preso.
func (s *Store) ReleaseStaleLotClaim(ctx context.Context, lotID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE lots SET status = 'open' WHERE id = $1 AND status = 'closed'`, lotID)
	return err
}

// FailCharge desiste de gerar a cobrança depois de esgotadas as
// tentativas: o pedido vira 'failed' (NÃO é calote do cliente) e o produto
// volta pro estoque.
func (s *Store) FailCharge(ctx context.Context, orderID int64, lotID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE payment_orders SET status = 'failed'
		WHERE id = $1 AND status = 'pending' AND charge_id IS NULL`, orderID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, tx.Commit()
	}
	if err := releaseProduct(ctx, tx, lotID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lots SET status = 'unsold' WHERE id = $1`, lotID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

type PaidOrder struct {
	LotID         string
	ProductID     int64
	ParticipantID int64
}

// MarkPaid confirma o pagamento pelo charge_id do provedor.
//
// Os três "não deu" são distinguidos de propósito, porque exigem respostas
// HTTP diferentes no webhook:
//
//   - ErrNoOrder: nenhum pedido tem esse charge_id. Quase sempre é o
//     webhook correndo na frente do AttachCharge (PIX é rápido). O handler
//     devolve 5xx para o provedor REENVIAR — antes disso o pagamento
//     sumia em silêncio.
//   - ErrOrderAlreadyPaid: reentrega do mesmo webhook. Sucesso, 200.
//   - ErrOrderNotPending: pagou depois de expirar. 200 (não adianta
//     reenviar) mas com alarme para reconciliação manual.
func (s *Store) MarkPaid(ctx context.Context, chargeID string) (PaidOrder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PaidOrder{}, err
	}
	defer tx.Rollback()
	var lotID string
	var pid int64
	err = tx.QueryRowContext(ctx, `
		UPDATE payment_orders SET status = 'paid', paid_at = now()
		WHERE charge_id = $1 AND status = 'pending'
		RETURNING lot_id, participant_id`, chargeID).Scan(&lotID, &pid)
	if errors.Is(err, sql.ErrNoRows) {
		var current string
		if e := s.db.QueryRowContext(ctx,
			`SELECT status::text FROM payment_orders WHERE charge_id = $1`, chargeID).Scan(&current); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return PaidOrder{}, ErrNoOrder
			}
			return PaidOrder{}, e
		}
		if current == "paid" {
			return PaidOrder{}, ErrOrderAlreadyPaid
		}
		return PaidOrder{}, fmt.Errorf("%w (status %s)", ErrOrderNotPending, current)
	}
	if err != nil {
		return PaidOrder{}, err
	}
	var productID int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE lots SET status = 'paid' WHERE id = $1 RETURNING product_id`, lotID).Scan(&productID); err != nil {
		return PaidOrder{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE products SET status = 'sold', owner_id = $2 WHERE id = $1`, productID, pid); err != nil {
		return PaidOrder{}, err
	}
	if err := tx.Commit(); err != nil {
		return PaidOrder{}, err
	}
	return PaidOrder{LotID: lotID, ProductID: productID, ParticipantID: pid}, nil
}

type DueOrder struct {
	OrderID       int64
	LotID         string
	ParticipantID int64
	Amount        int64
	// HadCharge indica se o cliente chegou a receber uma cobrança. Quando
	// é false, o pedido venceu sem que o provedor tivesse gerado o boleto —
	// isso NÃO é calote e não entra em lot_defaults.
	HadCharge bool
}

// ExpireDuePayments marca os pedidos vencidos, registra o calote de quem
// realmente recebeu cobrança e não pagou, e devolve o produto pro estoque.
//
// Tudo acontece DENTRO de uma transação: é aí que o FOR UPDATE SKIP LOCKED
// realmente segura a linha. Na versão anterior o SELECT rodava em
// autocommit e o lock era liberado na mesma hora — a proteção entre
// réplicas vinha só do UPDATE condicional, não do SKIP LOCKED como o
// comentário prometia.
func (s *Store) ExpireDuePayments(ctx context.Context, limit int) ([]DueOrder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, lot_id, participant_id, amount, charge_id IS NOT NULL
		FROM payment_orders
		WHERE status = 'pending' AND expires_at < now()
		ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	var due []DueOrder
	for rows.Next() {
		var d DueOrder
		if err := rows.Scan(&d.OrderID, &d.LotID, &d.ParticipantID, &d.Amount, &d.HadCharge); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, d := range due {
		if _, err := tx.ExecContext(ctx, `
			UPDATE payment_orders SET status = 'expired' WHERE id = $1`, d.OrderID); err != nil {
			return nil, err
		}
		if d.HadCharge {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO lot_defaults (lot_id, participant_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`, d.LotID, d.ParticipantID); err != nil {
				return nil, err
			}
		}
		if err := releaseProductTx(ctx, tx, d.LotID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE lots SET status = 'unsold' WHERE id = $1`, d.LotID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return due, nil
}

type DueRegistration struct {
	Token         string
	LotID         string
	ParticipantID int64
}

// ExpireDueRegistrations faz o mesmo para quem ganhou e não preencheu o
// endereço no prazo — aqui o calote é legítimo: a pessoa foi avisada e
// tinha um link válido na mão.
func (s *Store) ExpireDueRegistrations(ctx context.Context, limit int) ([]DueRegistration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT token, lot_id, participant_id FROM registration_tokens
		WHERE used_at IS NULL AND expired_at IS NULL AND expires_at < now()
		ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	var due []DueRegistration
	for rows.Next() {
		var d DueRegistration
		if err := rows.Scan(&d.Token, &d.LotID, &d.ParticipantID); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, d := range due {
		if _, err := tx.ExecContext(ctx, `
			UPDATE registration_tokens SET expired_at = now() WHERE token = $1`, d.Token); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO lot_defaults (lot_id, participant_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, d.LotID, d.ParticipantID); err != nil {
			return nil, err
		}
		if err := releaseProductTx(ctx, tx, d.LotID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE lots SET status = 'unsold' WHERE id = $1`, d.LotID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return due, nil
}

// HasDefaulted diz se o participante já deu calote em algum lote — usado
// para barrar lances de caloteiro reincidente. A tabela lot_defaults era
// escrita e nunca lida; agora ela vale alguma coisa.
func (s *Store) HasDefaulted(ctx context.Context, jid string, minDefaults int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM lot_defaults d
		JOIN participants p ON p.id = d.participant_id
		WHERE p.whatsapp_jid = $1`, jid).Scan(&n)
	return n >= minDefaults, err
}

// StaleOpenLots lista lotes que o Postgres ainda acha abertos mas que já
// passaram do prazo máximo possível. É a rede de proteção para o caso de
// o evento de fechamento ter se perdido (fila cheia, processo morto entre
// o fechamento no Redis e a escrita no Postgres).
func (s *Store) StaleOpenLots(ctx context.Context, olderThan time.Duration, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM lots
		WHERE status = 'open' AND opened_at < now() - ($1 || ' milliseconds')::interval
		ORDER BY opened_at LIMIT $2`, olderThan.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LotChannel devolve o canal de WhatsApp do lote e o teto de lance.
func (s *Store) LotChannel(ctx context.Context, lotID string) (channelJID string, maxBid int64, err error) {
	var ch, mb sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT channel_jid, max_bid::text FROM lots WHERE id = $1`, lotID).Scan(&ch, &mb)
	if err != nil {
		return "", 0, err
	}
	if mb.Valid {
		maxBid = toInt(mb.String)
	}
	return ch.String, maxBid, nil
}

// ActiveLotForChannel traduz "chegou mensagem nesse grupo" em "esse é o
// lote que está sendo disputado agora". É o que faltava para o webhook do
// WhatsApp conseguir registrar um lance.
func (s *Store) ActiveLotForChannel(ctx context.Context, channelJID string) (lotID string, maxBid int64, err error) {
	var mb sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT id, max_bid::text FROM lots
		WHERE channel_jid = $1 AND status = 'open'
		ORDER BY opened_at DESC LIMIT 1`, channelJID).Scan(&lotID, &mb)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNoActiveLot
	}
	if err != nil {
		return "", 0, err
	}
	if mb.Valid {
		maxBid = toInt(mb.String)
	}
	return lotID, maxBid, nil
}

// PurgeExpiredAuth apaga códigos de login e sessões vencidos. Sem isso as
// tabelas só cresciam.
func (s *Store) PurgeExpiredAuth(ctx context.Context, olderThan time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM login_codes WHERE expires_at < now() - ($1 || ' milliseconds')::interval`,
		olderThan.Milliseconds()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions WHERE expires_at < now() - ($1 || ' milliseconds')::interval`,
		olderThan.Milliseconds())
	return err
}

// RevokeSession encerra a sessão da área do cliente (logout). Casa pelo
// hash, que é o que fica guardado.
func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE token = $1 AND revoked_at IS NULL`, hashCode(token))
	return err
}

func releaseProduct(ctx context.Context, tx *sql.Tx, lotID string) error {
	return releaseProductTx(ctx, tx, lotID)
}

func releaseProductTx(ctx context.Context, q querier, lotID string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE products SET status = 'available'
		WHERE id = (SELECT product_id FROM lots WHERE id = $1)`, lotID)
	return err
}
