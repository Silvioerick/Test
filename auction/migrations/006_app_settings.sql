-- Configuração gerenciável pelo painel, sem redeploy.
--
-- Mesma ideia do payment_settings: o que o admin consegue digitar na tela
-- não deveria viver em variável de ambiente. Segredos ficam cifrados com
-- a mesma chave mestra (AES-256-GCM); o que não é segredo fica em claro
-- para dar para auditar direto no banco.
--
-- O que NÃO cabe aqui, e por quê:
--   DATABASE_URL / REDIS_ADDR -- são necessários para chegar até esta tabela.
--   ENCRYPTION_KEY            -- é a chave que decifra esta tabela; guardá-la
--                                aqui não protegeria nada.
--   ADMIN_TOKEN               -- é o que protege o painel; se morasse no
--                                painel, seria preciso autenticar para poder
--                                configurar a autenticação.
CREATE TABLE IF NOT EXISTS app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT,      -- valor em claro (configuração não-sensível)
    value_enc  BYTEA,     -- nonce + ciphertext (AES-256-GCM), para segredos
    secret     BOOLEAN NOT NULL DEFAULT false,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Exatamente um dos dois preenchido, conforme `secret`.
    CONSTRAINT app_settings_one_value CHECK (
        (secret AND value_enc IS NOT NULL AND value IS NULL) OR
        (NOT secret AND value IS NOT NULL AND value_enc IS NULL)
    )
);

COMMENT ON TABLE app_settings IS
    'Configuração editável no painel. Segredos cifrados com a ENCRYPTION_KEY.';
