-- Cadastro de cliente (endereço + documento), necessário pro frete e pra
-- cobrar via Asaas (que exige CPF/CNPJ do cliente). Coletado depois que a
-- pessoa ganha o lote — não faz sentido pedir isso de quem só participou.

ALTER TABLE participants
    ADD COLUMN full_name            TEXT,
    ADD COLUMN document             TEXT,          -- CPF/CNPJ
    ADD COLUMN email                TEXT,
    ADD COLUMN address_cep          TEXT,
    ADD COLUMN address_street       TEXT,
    ADD COLUMN address_number       TEXT,
    ADD COLUMN address_complement   TEXT,
    ADD COLUMN address_neighborhood TEXT,
    ADD COLUMN address_city         TEXT,
    ADD COLUMN address_state        CHAR(2),
    ADD COLUMN registered_at        TIMESTAMPTZ;   -- NULL = ainda não completou o cadastro

-- Token de uso único enviado por WhatsApp junto com "você ganhou!" pra abrir
-- o formulário de cadastro sem precisar de login.
CREATE TABLE registration_tokens (
    token          TEXT PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES participants(id),
    lot_id         TEXT NOT NULL REFERENCES lots(id),
    expires_at     TIMESTAMPTZ NOT NULL,
    used_at        TIMESTAMPTZ
);

-- Faixas de frete por prefixo de CEP. Simples de configurar no painel;
-- troque por uma chamada real aos Correios/Melhor Envio depois — a
-- interface ShippingCalculator (shipping.go) já isola essa decisão.
CREATE TABLE shipping_zones (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    cep_prefix   TEXT NOT NULL,   -- ex.: "0" cobre CEPs 0xxxxxxx (região SP capital)
    price_cents  BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX shipping_zones_prefix_idx ON shipping_zones (cep_prefix);

ALTER TYPE lot_status ADD VALUE IF NOT EXISTS 'awaiting_registration' BEFORE 'awaiting_payment';

ALTER TABLE lots
    ADD COLUMN shipping_cents BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN discount_cents BIGINT NOT NULL DEFAULT 0;
-- total cobrado do cliente = current_amount + shipping_cents - discount_cents;
-- calculado só quando o endereço chega (ver Orchestrator.completeRegistration).

-- payment_orders precisa acomodar mais de um provedor de cobrança.
ALTER TABLE payment_orders RENAME COLUMN hubpay_charge_id TO charge_id;
ALTER TABLE payment_orders ADD COLUMN provider TEXT NOT NULL DEFAULT 'hubpay';
ALTER TABLE payment_orders ADD COLUMN pay_url TEXT;  -- link de checkout (Asaas), quando houver

ALTER TABLE registration_tokens ADD COLUMN expired_at TIMESTAMPTZ;
