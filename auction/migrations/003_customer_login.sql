-- Login da área do cliente: a pessoa informa o telefone, recebe um código
-- de 6 dígitos por WhatsApp, confirma e recebe uma sessão. Sem senha —
-- o WhatsApp já é a prova de identidade.

CREATE TABLE login_codes (
    id             BIGSERIAL PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES participants(id),
    code_hash      TEXT NOT NULL,       -- SHA-256 do código; nunca guardamos em texto puro
    attempts       INT NOT NULL DEFAULT 0,
    expires_at     TIMESTAMPTZ NOT NULL,
    used_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX login_codes_participant_idx ON login_codes (participant_id, created_at DESC);

CREATE TABLE sessions (
    token          TEXT PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES participants(id),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_participant_idx ON sessions (participant_id);
