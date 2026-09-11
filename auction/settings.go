package auction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Chaves conhecidas de app_settings. O painel edita estas; qualquer outra
// também funciona, mas estas são as que o código consulta.
const (
	SetWhatsAppAPIURL        = "whatsapp.api_url"
	SetWhatsAppAPIToken      = "whatsapp.api_token"      // segredo
	SetWhatsAppWebhookSecret = "whatsapp.webhook_secret" // segredo
	SetWhatsAppToField       = "whatsapp.to_field"
	SetWhatsAppTextField     = "whatsapp.text_field"
	SetHubPayWebhookSecret   = "hubpay.webhook_secret" // segredo
	SetHubPaySigHeader       = "hubpay.signature_header"
	SetAsaasWebhookToken     = "asaas.webhook_token" // segredo
	SetAsaasTokenHeader      = "asaas.token_header"
	SetRegisterURL           = "register_url"
)

// settingSpec descreve cada chave para o painel e para o fallback de env.
type settingSpec struct {
	Key    string
	Label  string
	Secret bool
	EnvVar string // de onde herdar enquanto ninguém tiver salvo no painel
	Help   string
}

// KnownSettings é o que o painel renderiza, na ordem em que aparece.
var KnownSettings = []settingSpec{
	{SetWhatsAppAPIURL, "DigiGO — URL de envio", false, "WHATSAPP_API_URL",
		"Endpoint que recebe o POST de envio de mensagem. Vazio = nada é entregue de verdade."},
	{SetWhatsAppAPIToken, "DigiGO — token", true, "WHATSAPP_API_TOKEN",
		"Vai no header Authorization: Bearer."},
	{SetWhatsAppWebhookSecret, "DigiGO — segredo do webhook de entrada", true, "WHATSAPP_WEBHOOK_SECRET",
		"Conferido no header X-Webhook-Token. Quem posta nesse endpoint dá lance no lugar de terceiros."},
	{SetWhatsAppToField, "DigiGO — nome do campo do destinatário", false, "",
		`Padrão "to". Só mexa se a API do DigiGO usar outro nome.`},
	{SetWhatsAppTextField, "DigiGO — nome do campo do texto", false, "",
		`Padrão "text".`},
	{SetHubPayWebhookSecret, "HubPay — segredo do webhook", true, "HUBPAY_WEBHOOK_SECRET",
		"HMAC-SHA256 do corpo cru. Sem ele o webhook responde 503 e não aceita nada."},
	{SetHubPaySigHeader, "HubPay — header da assinatura", false, "HUBPAY_SIGNATURE_HEADER",
		"Padrão X-Hubpay-Signature."},
	{SetAsaasWebhookToken, "Asaas — authToken do webhook", true, "ASAAS_WEBHOOK_TOKEN",
		"O mesmo valor configurado em authToken no painel da Asaas."},
	{SetAsaasTokenHeader, "Asaas — header do token", false, "ASAAS_TOKEN_HEADER",
		"Vazio = tenta os nomes conhecidos. Fixe assim que confirmar o seu."},
	{SetRegisterURL, "Link de cadastro — prefixo", false, "REGISTER_URL",
		"O token de uso único é concatenado no fim."},
}

func specFor(key string) (settingSpec, bool) {
	for _, s := range KnownSettings {
		if s.Key == key {
			return s, true
		}
	}
	return settingSpec{}, false
}

// SettingsStore serve a configuração editável no painel.
//
// Ordem de resolução de cada chave:
//  1. valor salvo no painel (Postgres) — ganha sempre;
//  2. variável de ambiente correspondente — fallback, para um deploy que
//     já existia continuar funcionando sem ninguém abrir o painel;
//  3. vazio.
//
// As leituras passam por um cache curto porque o webhook consulta isto a
// cada requisição. Uma alteração no painel vale na própria réplica na
// hora (a escrita invalida o cache) e nas outras em até cacheTTL.
type SettingsStore struct {
	db  *sql.DB
	enc *Encryptor

	mu       sync.RWMutex
	cache    map[string]string
	cachedAt time.Time
	ttl      time.Duration
}

const settingsCacheTTL = 5 * time.Second

func NewSettingsStore(db *sql.DB, enc *Encryptor) *SettingsStore {
	return &SettingsStore{db: db, enc: enc, ttl: settingsCacheTTL}
}

// Get resolve uma chave: painel, senão env, senão vazio.
func (s *SettingsStore) Get(ctx context.Context, key string) string {
	if v, ok := s.fromDB(ctx, key); ok && v != "" {
		return v
	}
	if spec, ok := specFor(key); ok && spec.EnvVar != "" {
		return envWithFile(spec.EnvVar)
	}
	return ""
}

// GetDefault é Get com um padrão para quando nada estiver configurado.
func (s *SettingsStore) GetDefault(ctx context.Context, key, fallback string) string {
	if v := s.Get(ctx, key); v != "" {
		return v
	}
	return fallback
}

func (s *SettingsStore) fromDB(ctx context.Context, key string) (string, bool) {
	s.mu.RLock()
	fresh := s.cache != nil && time.Since(s.cachedAt) < s.ttl
	if fresh {
		v, ok := s.cache[key]
		s.mu.RUnlock()
		return v, ok
	}
	s.mu.RUnlock()

	loaded, err := s.loadAll(ctx)
	if err != nil {
		// Não derruba o fluxo por causa de uma leitura de configuração:
		// devolve o que houver em cache (mesmo velho) e deixa o env decidir.
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.cache != nil {
			v, ok := s.cache[key]
			return v, ok
		}
		return "", false
	}
	s.mu.Lock()
	s.cache, s.cachedAt = loaded, time.Now()
	s.mu.Unlock()
	v, ok := loaded[key]
	return v, ok
}

func (s *SettingsStore) loadAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, value_enc, secret FROM app_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key string
		var value sql.NullString
		var enc []byte
		var secret bool
		if err := rows.Scan(&key, &value, &enc, &secret); err != nil {
			return nil, err
		}
		if !secret {
			out[key] = value.String
			continue
		}
		plain, err := s.enc.Decrypt(enc)
		if err != nil {
			// Chave mestra trocada: melhor tratar como não configurado do
			// que devolver lixo para um verificador de assinatura.
			continue
		}
		out[key] = plain
	}
	return out, rows.Err()
}

// Set grava (ou apaga, com value vazio) uma chave e invalida o cache.
func (s *SettingsStore) Set(ctx context.Context, key, value string) error {
	spec, known := specFor(key)
	if !known {
		return fmt.Errorf("auction: chave de configuração desconhecida: %q", key)
	}
	defer s.invalidate()
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key = $1`, key)
		return err
	}
	if !spec.Secret {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO app_settings (key, value, secret) VALUES ($1, $2, false)
			ON CONFLICT (key) DO UPDATE
				SET value = $2, value_enc = NULL, secret = false, updated_at = now()`,
			key, value)
		return err
	}
	blob, err := s.enc.Encrypt(value)
	if err != nil {
		return fmt.Errorf("cifrar %s: %w", key, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO app_settings (key, value_enc, secret) VALUES ($1, $2, true)
		ON CONFLICT (key) DO UPDATE
			SET value_enc = $2, value = NULL, secret = true, updated_at = now()`,
		key, blob)
	return err
}

func (s *SettingsStore) invalidate() {
	s.mu.Lock()
	s.cache, s.cachedAt = nil, time.Time{}
	s.mu.Unlock()
}

// SettingView é uma linha da tela de configuração. Segredo nunca sai em
// claro: o painel mostra só os últimos caracteres.
type SettingView struct {
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Help      string    `json:"help"`
	Secret    bool      `json:"secret"`
	Value     string    `json:"value"`            // vazio quando Secret
	Masked    string    `json:"masked,omitempty"` // preenchido quando Secret
	Source    string    `json:"source"`           // "painel", "env" ou "vazio"
	EnvVar    string    `json:"env_var,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// List devolve todas as chaves conhecidas com o valor efetivo e de onde
// ele veio — para o admin ver o que está herdado de env e o que já foi
// gravado no painel.
func (s *SettingsStore) List(ctx context.Context) ([]SettingView, error) {
	stored := map[string]time.Time{}
	rows, err := s.db.QueryContext(ctx, `SELECT key, updated_at FROM app_settings`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var t time.Time
		if err := rows.Scan(&k, &t); err != nil {
			rows.Close()
			return nil, err
		}
		stored[k] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SettingView, 0, len(KnownSettings))
	for _, spec := range KnownSettings {
		v := SettingView{
			Key: spec.Key, Label: spec.Label, Help: spec.Help,
			Secret: spec.Secret, EnvVar: spec.EnvVar,
		}
		dbVal, inDB := s.fromDB(ctx, spec.Key)
		effective := dbVal
		switch {
		case inDB && dbVal != "":
			v.Source = "painel"
			v.UpdatedAt = stored[spec.Key]
		case spec.EnvVar != "" && envWithFile(spec.EnvVar) != "":
			v.Source = "env"
			effective = envWithFile(spec.EnvVar)
		default:
			v.Source = "vazio"
		}
		if spec.Secret {
			if effective != "" {
				v.Masked = maskKey(effective)
			}
		} else {
			v.Value = effective
		}
		out = append(out, v)
	}
	return out, nil // já na ordem de KnownSettings
}

// envWithFile lê VAR ou, se vazia, o arquivo apontado por VAR_FILE — o
// mesmo contrato do cmd/server, para funcionar com secret do Swarm,
// Kubernetes ou Vault.
func envWithFile(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	path := os.Getenv(name + "_FILE")
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\r\n ")
}

var errNoNotifierConfigured = errors.New("auction: gateway de WhatsApp não configurado — preencha no painel, em Configurações")
