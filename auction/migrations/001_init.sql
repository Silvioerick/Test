-- Postgres guarda o registro durável (quem deu lance, pagamentos, dono do
-- produto). O Redis (auction.go) continua sendo a autoridade sobre a
-- corrida de lances em tempo real — é rápido demais e concorrido demais
-- para bater no Postgres a cada lance sem risco de travar a resposta.
-- Este arquivo é consumido por store.go via migração simples (roda uma vez).

CREATE TYPE product_status AS ENUM ('available', 'in_auction', 'sold', 'returned');
CREATE TYPE lot_status     AS ENUM ('open', 'closed', 'awaiting_payment', 'paid', 'unsold', 'cancelled');
CREATE TYPE order_status   AS ENUM ('pending', 'paid', 'expired', 'cancelled');

CREATE TABLE participants (
    id           BIGSERIAL PRIMARY KEY,
    whatsapp_jid TEXT NOT NULL UNIQUE,
    name         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE products (
    id         BIGSERIAL PRIMARY KEY,
    sku        TEXT UNIQUE,
    name       TEXT NOT NULL,
    status     product_status NOT NULL DEFAULT 'available',
    owner_id   BIGINT REFERENCES participants(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- id do lote é o MESMO id usado no motor Redis (auction.Engine), pra nunca
-- precisar traduzir entre os dois lados.
CREATE TABLE lots (
    id              TEXT PRIMARY KEY,
    product_id      BIGINT NOT NULL REFERENCES products(id),
    start_price     BIGINT NOT NULL,
    min_increment   BIGINT NOT NULL,
    status          lot_status NOT NULL DEFAULT 'open',
    current_amount  BIGINT NOT NULL DEFAULT 0,
    bid_count       INT NOT NULL DEFAULT 0,
    -- quem está devendo pagar agora (muda se o 1º colocado levar calote)
    claimant_id     BIGINT REFERENCES participants(id),
    payment_ms      BIGINT NOT NULL DEFAULT 900000,  -- prazo pra pagar, em ms; 900000 = 15 min
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ
);

CREATE TABLE bids (
    id             BIGSERIAL PRIMARY KEY,
    lot_id         TEXT NOT NULL REFERENCES lots(id),
    participant_id BIGINT NOT NULL REFERENCES participants(id),
    amount         BIGINT NOT NULL,
    msg_id         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (lot_id, msg_id)              -- reentrega do webhook não duplica
);
CREATE INDEX bids_lot_amount_idx ON bids (lot_id, amount DESC);

-- participantes que já levaram calote nesse lote: nunca voltam a ser
-- escolhidos como próximo da fila.
CREATE TABLE lot_defaults (
    lot_id         TEXT NOT NULL REFERENCES lots(id),
    participant_id BIGINT NOT NULL REFERENCES participants(id),
    defaulted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (lot_id, participant_id)
);

CREATE TABLE payment_orders (
    id               BIGSERIAL PRIMARY KEY,
    lot_id           TEXT NOT NULL REFERENCES lots(id),
    participant_id   BIGINT NOT NULL REFERENCES participants(id),
    hubpay_charge_id TEXT UNIQUE,        -- id da cobrança no HubPay
    amount           BIGINT NOT NULL,
    qr_code          TEXT,               -- payload copia-e-cola do PIX
    status           order_status NOT NULL DEFAULT 'pending',
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at          TIMESTAMPTZ
);
CREATE INDEX payment_orders_pending_idx ON payment_orders (status, expires_at)
    WHERE status = 'pending';
