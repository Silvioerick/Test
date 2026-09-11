-- Configuração de provedor de pagamento gerenciável pelo painel, sem
-- precisar redeploy pra trocar chave ou de provedor. A chave da API fica
-- criptografada (AES-256-GCM) com uma chave mestra que essa, sim, mora
-- fora do banco (env ENCRYPTION_KEY) — é o único segredo que continua
-- fora do Postgres, porque criptografar com uma chave que mora do lado da
-- coisa criptografada não protege nada.

CREATE TABLE payment_settings (
    provider    TEXT PRIMARY KEY,        -- 'asaas', 'hubpay', etc.
    api_key_enc BYTEA NOT NULL,          -- nonce + ciphertext (AES-256-GCM)
    base_url    TEXT,
    active      BOOLEAN NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Garante no banco que só um provedor pode estar ativo por vez (índice
-- único parcial: só indexa as linhas com active=true).
CREATE UNIQUE INDEX payment_settings_single_active
    ON payment_settings ((active)) WHERE active;
