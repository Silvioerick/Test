package auction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNoActivePaymentProvider = errors.New("auction: nenhum provedor de pagamento ativo configurado no painel")

// PaymentSettingsStore guarda a configuração de cada gateway (chave
// cifrada, URL base) e qual deles está ativo agora. Tudo isso é
// gerenciável pelo painel web, sem precisar de redeploy pra trocar chave
// ou de provedor.
type PaymentSettingsStore struct {
	db  *sql.DB
	enc *Encryptor
}

func NewPaymentSettingsStore(db *sql.DB, enc *Encryptor) *PaymentSettingsStore {
	return &PaymentSettingsStore{db: db, enc: enc}
}

// Upsert cadastra ou atualiza a chave de um provedor. Não mexe em quem
// está ativo — use Activate pra isso.
func (s *PaymentSettingsStore) Upsert(ctx context.Context, provider, apiKey, baseURL string) error {
	enc, err := s.enc.Encrypt(apiKey)
	if err != nil {
		return fmt.Errorf("cifrar chave: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO payment_settings (provider, api_key_enc, base_url)
		VALUES ($1, $2, NULLIF($3, ''))
		ON CONFLICT (provider) DO UPDATE
			SET api_key_enc = $2, base_url = NULLIF($3, ''), updated_at = now()`,
		provider, enc, baseURL)
	return err
}

// Activate torna `provider` o único ativo. Falha se ele ainda não tiver
// sido cadastrado via Upsert.
func (s *PaymentSettingsStore) Activate(ctx context.Context, provider string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE payment_settings SET active = false WHERE active`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE payment_settings SET active = true WHERE provider = $1`, provider)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("auction: provedor %q não está cadastrado — cadastre a chave antes de ativar", provider)
	}
	return tx.Commit()
}

type PaymentSettingSummary struct {
	Provider  string
	BaseURL   string
	Active    bool
	MaskedKey string // só os últimos 4 caracteres aparecem
	UpdatedAt time.Time
}

func (s *PaymentSettingsStore) List(ctx context.Context) ([]PaymentSettingSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, COALESCE(base_url, ''), active, api_key_enc, updated_at
		FROM payment_settings ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaymentSettingSummary
	for rows.Next() {
		var p PaymentSettingSummary
		var encKey []byte
		if err := rows.Scan(&p.Provider, &p.BaseURL, &p.Active, &encKey, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if key, err := s.enc.Decrypt(encKey); err == nil {
			p.MaskedKey = maskKey(key)
		} else {
			p.MaskedKey = "(erro ao decifrar — a ENCRYPTION_KEY mudou?)"
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return "••••"
	}
	return "••••••••" + key[len(key)-4:]
}

// GetActive devolve o provedor ativo com a chave já decifrada, pronto pra
// uso — é o que DBPaymentProvider chama a cada cobrança.
func (s *PaymentSettingsStore) GetActive(ctx context.Context) (provider, apiKey, baseURL string, err error) {
	var encKey []byte
	err = s.db.QueryRowContext(ctx, `
		SELECT provider, api_key_enc, COALESCE(base_url, '') FROM payment_settings WHERE active LIMIT 1`,
	).Scan(&provider, &encKey, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", ErrNoActivePaymentProvider
	}
	if err != nil {
		return "", "", "", err
	}
	apiKey, err = s.enc.Decrypt(encKey)
	return provider, apiKey, baseURL, err
}

// DBPaymentProvider implementa PaymentProvider lendo a configuração do
// Postgres a cada cobrança — trocar de provedor ou de chave no painel vale
// pra próxima cobrança na hora, sem reiniciar o processo.
type DBPaymentProvider struct{ settings *PaymentSettingsStore }

func NewDBPaymentProvider(settings *PaymentSettingsStore) *DBPaymentProvider {
	return &DBPaymentProvider{settings: settings}
}

func (d *DBPaymentProvider) CreateCharge(ctx context.Context, reference string, amountCents int64, customer Customer) (ChargeResult, error) {
	provider, apiKey, baseURL, err := d.settings.GetActive(ctx)
	if err != nil {
		return ChargeResult{}, err
	}
	switch provider {
	case "asaas":
		return NewAsaasProvider(baseURL, apiKey).CreateCharge(ctx, reference, amountCents, customer)
	case "hubpay":
		// Ainda sem implementação real — falta a documentação da API de
		// vocês. Implemente um HubPayProvider com o mesmo formato do
		// AsaasProvider (asaas.go) e adicione o case aqui.
		return ChargeResult{}, fmt.Errorf("auction: provedor hubpay ainda não tem implementação real")
	default:
		return ChargeResult{}, fmt.Errorf("auction: provedor de pagamento desconhecido: %q", provider)
	}
}
