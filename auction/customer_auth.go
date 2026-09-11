package auction

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrTooManyCodes   = errors.New("auction: muitos pedidos de código, tente de novo em alguns minutos")
	ErrCodeInvalid    = errors.New("auction: código inválido ou expirado")
	ErrSessionInvalid = errors.New("auction: sessão inválida ou expirada")
)

var nonDigits = regexp.MustCompile(`\D`)

// NormalizePhone aceita o telefone em qualquer formato comum
// ("(11) 99999-9999", "+55 11 99999-9999", "11999999999") e devolve o JID
// do WhatsApp no mesmo formato usado em todo o resto do sistema.
// Assumo números brasileiros: se não vier com o "55" na frente e tiver
// jeito de DDD+número (10 ou 11 dígitos), prefixo "55" automaticamente.
func NormalizePhone(raw string) (jid string, err error) {
	digits := nonDigits.ReplaceAllString(raw, "")
	switch len(digits) {
	case 10, 11:
		digits = "55" + digits
	case 12, 13:
		// já deve vir com código do país
	default:
		return "", fmt.Errorf("telefone inválido: %q", raw)
	}
	return digits + "@s.whatsapp.net", nil
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// randomDigits sorteia n dígitos uniformemente. A versão anterior fazia
// b%10 sobre um byte, o que deixava os dígitos 0-5 mais prováveis que 6-9
// (256 não é múltiplo de 10); aqui os bytes fora da faixa são descartados.
func randomDigits(n int) (string, error) {
	const digits = "0123456789"
	const limit = 250 // maior múltiplo de 10 que cabe em um byte
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= limit {
				continue // descarta para não enviesar
			}
			out = append(out, digits[b%10])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// CreateLoginCode gera um código de 6 dígitos pro participante e devolve o
// código em claro (pra ser mandado pelo WhatsApp — nunca fica salvo em
// claro no banco). Recusa com ErrTooManyCodes se já pediu demais recente.
func (s *Store) CreateLoginCode(ctx context.Context, jid string, window time.Duration, maxPerWindow int, ttl time.Duration) (code string, err error) {
	var participantID int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO participants (whatsapp_jid) VALUES ($1)
		ON CONFLICT (whatsapp_jid) DO UPDATE SET whatsapp_jid = EXCLUDED.whatsapp_jid
		RETURNING id`, jid).Scan(&participantID)
	if err != nil {
		return "", err
	}
	var recent int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM login_codes
		WHERE participant_id = $1 AND created_at > now() - ($2 || ' milliseconds')::interval`,
		participantID, window.Milliseconds()).Scan(&recent); err != nil {
		return "", err
	}
	if recent >= maxPerWindow {
		return "", ErrTooManyCodes
	}
	code, err = randomDigits(6)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO login_codes (participant_id, code_hash, expires_at)
		VALUES ($1, $2, now() + ($3 || ' milliseconds')::interval)`,
		participantID, hashCode(code), ttl.Milliseconds())
	return code, err
}

// VerifyLoginCode confere o código mais recente ainda válido pro
// participante. Cada tentativa errada conta; depois de maxAttempts o
// código é invalidado e a pessoa precisa pedir um novo.
func (s *Store) VerifyLoginCode(ctx context.Context, jid, code string, maxAttempts int) (participantID int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var codeID int64
	var hash string
	var attempts int
	err = tx.QueryRowContext(ctx, `
		SELECT lc.id, lc.code_hash, lc.attempts, p.id
		FROM login_codes lc
		JOIN participants p ON p.id = lc.participant_id
		WHERE p.whatsapp_jid = $1 AND lc.used_at IS NULL AND lc.expires_at > now()
		ORDER BY lc.created_at DESC LIMIT 1 FOR UPDATE OF lc`,
		jid).Scan(&codeID, &hash, &attempts, &participantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrCodeInvalid
	}
	if err != nil {
		return 0, err
	}
	if attempts >= maxAttempts {
		return 0, ErrCodeInvalid
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashCode(code))) != 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE login_codes SET attempts = attempts + 1 WHERE id = $1`, codeID); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, ErrCodeInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE login_codes SET used_at = now() WHERE id = $1`, codeID); err != nil {
		return 0, err
	}
	return participantID, tx.Commit()
}

// CreateSession abre uma sessão pra área do cliente depois do login por
// código. O banco guarda só o SHA-256 do token, pelo mesmo motivo do
// código de login: um dump do Postgres não pode virar um passe para a
// conta de ninguém.
func (s *Store) CreateSession(ctx context.Context, participantID int64, ttl time.Duration) (token string, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", err
	}
	token = hex.EncodeToString(buf)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (token, participant_id, expires_at)
		VALUES ($1, $2, now() + ($3 || ' milliseconds')::interval)`,
		hashCode(token), participantID, ttl.Milliseconds())
	return token, err
}

// GetSession valida o token da sessão e devolve o participante dono dela.
func (s *Store) GetSession(ctx context.Context, token string) (participantID int64, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT participant_id FROM sessions
		WHERE token = $1 AND revoked_at IS NULL AND expires_at > now()`,
		hashCode(token)).Scan(&participantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSessionInvalid
	}
	return participantID, err
}

// CustomerProfile é o que a área do cliente mostra sobre a própria pessoa.
type CustomerProfile struct {
	JID        string
	Name       string
	Document   string
	Email      string
	CEP        string
	Street     string
	Number     string
	City       string
	State      string
	Registered bool
}

func (s *Store) GetCustomerProfile(ctx context.Context, participantID int64) (CustomerProfile, error) {
	var p CustomerProfile
	err := s.db.QueryRowContext(ctx, `
		SELECT whatsapp_jid, COALESCE(full_name, name, ''), COALESCE(document, ''), COALESCE(email, ''),
		       COALESCE(address_cep, ''), COALESCE(address_street, ''), COALESCE(address_number, ''),
		       COALESCE(address_city, ''), COALESCE(address_state, ''), registered_at IS NOT NULL
		FROM participants WHERE id = $1`, participantID).Scan(
		&p.JID, &p.Name, &p.Document, &p.Email, &p.CEP, &p.Street, &p.Number, &p.City, &p.State, &p.Registered)
	return p, err
}

// CustomerLotHistory é uma linha do "extrato" da pessoa: cada lote em que
// ela deu lance, se ganhou, e a situação do pagamento — tudo que ela tem
// direito de ver sobre si mesma.
type CustomerLotHistory struct {
	LotID         string
	ProductName   string
	LotStatus     string
	YourBidCents  int64
	Won           bool
	ShippingCents int64
	DiscountCents int64
	ChargeAmount  sql.NullInt64
	PaymentStatus sql.NullString
	PaidAt        sql.NullTime
	OpenedAt      time.Time
	ClosedAt      sql.NullTime
}

// GetCustomerHistory lista todo lote em que o participante deu lance, com
// o próprio lance máximo, se ganhou e a situação de pagamento — sempre
// filtrado pelo participantID vindo da sessão, nunca de um id arbitrário
// na URL, pra ninguém ver o extrato de outra pessoa.
func (s *Store) GetCustomerHistory(ctx context.Context, participantID int64) ([]CustomerLotHistory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, pr.name, l.status, bmax.amount, (l.claimant_id = $1),
		       l.shipping_cents, l.discount_cents,
		       po.amount, po.status, po.paid_at, l.opened_at, l.closed_at
		FROM (
			SELECT lot_id, MAX(amount) AS amount FROM bids WHERE participant_id = $1 GROUP BY lot_id
		) bmax
		JOIN lots l ON l.id = bmax.lot_id
		JOIN products pr ON pr.id = l.product_id
		LEFT JOIN payment_orders po ON po.lot_id = l.id AND po.participant_id = $1
		ORDER BY l.opened_at DESC`, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomerLotHistory
	for rows.Next() {
		var h CustomerLotHistory
		if err := rows.Scan(&h.LotID, &h.ProductName, &h.LotStatus, &h.YourBidCents, &h.Won,
			&h.ShippingCents, &h.DiscountCents, &h.ChargeAmount, &h.PaymentStatus, &h.PaidAt,
			&h.OpenedAt, &h.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
