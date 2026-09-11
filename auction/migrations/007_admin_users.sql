-- Login de verdade no painel.
--
-- Antes existia só um ADMIN_TOKEN: um segredo único, igual para todo
-- mundo, colado num campo da tela. Sem conta por pessoa, sem saber quem
-- fez o quê, sem revogar o acesso de alguém que saiu, e sem trocar a
-- senha sem redeploy — tudo isso num painel exposto na internet com CPF e
-- endereço de cliente dentro.
CREATE TABLE IF NOT EXISTS admin_users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    -- PBKDF2-HMAC-SHA256, formato: pbkdf2_sha256$<iter>$<salt_b64>$<hash_b64>
    password_hash TEXT NOT NULL,
    -- Obriga a trocar a senha no primeiro acesso (usuário criado pelo
    -- bootstrap, com senha que passou por variável de ambiente).
    must_change   BOOLEAN NOT NULL DEFAULT false,
    disabled_at   TIMESTAMPTZ,
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    -- SHA-256 do token; o token em claro só existe no cookie do navegador.
    token_hash TEXT PRIMARY KEY,
    admin_id   BIGINT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_sessions_admin_idx ON admin_sessions (admin_id);
CREATE INDEX IF NOT EXISTS admin_sessions_expiry_idx ON admin_sessions (expires_at);

-- Tentativas de login, para travar força bruta por usuário.
CREATE TABLE IF NOT EXISTS admin_login_attempts (
    username   TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    ok         BOOLEAN NOT NULL
);
CREATE INDEX IF NOT EXISTS admin_login_attempts_idx ON admin_login_attempts (username, at DESC);
