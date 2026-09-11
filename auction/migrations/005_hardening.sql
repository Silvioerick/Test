-- Correções da revisão de segurança/consistência.
--
-- ATENÇÃO: rode este arquivo FORA de uma transação envolvente se o seu
-- runner envolver cada arquivo em BEGIN/COMMIT — o mesmo cuidado da 002,
-- por causa do ALTER TYPE ... ADD VALUE.

-- 1. Um produto não pode estar em dois leilões abertos ao mesmo tempo.
--    O índice parcial é a garantia no banco; a API também passa a checar.
CREATE UNIQUE INDEX IF NOT EXISTS lots_one_open_per_product
    ON lots (product_id)
    WHERE status IN ('open', 'awaiting_registration', 'awaiting_payment');

-- 2. Canal de WhatsApp (grupo ou número do bot) em que o lote está sendo
--    disputado. É o que permite traduzir "mensagem chegou nesse grupo"
--    para "esse é o lote ativo" no webhook de entrada.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS channel_jid TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS lots_one_open_per_channel
    ON lots (channel_jid)
    WHERE status = 'open' AND channel_jid IS NOT NULL;

-- 3. Marca pedidos cuja cobrança nunca chegou a ser gerada (provedor fora
--    do ar). Sem isso o cliente era marcado como caloteiro sem nunca ter
--    recebido um boleto. Também conta as tentativas de retry.
ALTER TABLE payment_orders ADD COLUMN IF NOT EXISTS charge_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE payment_orders ADD COLUMN IF NOT EXISTS last_charge_error TEXT;

-- 4. 'failed' = a cobrança não pôde ser gerada; não é calote do cliente.
ALTER TYPE order_status ADD VALUE IF NOT EXISTS 'failed';

-- 5. Índices que faltavam para as varreduras periódicas e para o extrato.
CREATE INDEX IF NOT EXISTS registration_tokens_due_idx
    ON registration_tokens (expires_at) WHERE used_at IS NULL AND expired_at IS NULL;
CREATE INDEX IF NOT EXISTS payment_orders_lot_idx ON payment_orders (lot_id);
CREATE INDEX IF NOT EXISTS payment_orders_participant_idx ON payment_orders (participant_id);
CREATE INDEX IF NOT EXISTS bids_participant_idx ON bids (participant_id);
CREATE INDEX IF NOT EXISTS lots_open_idx ON lots (status, opened_at) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expires_at);

-- 6. Limite de lance por lote: impede que uma mensagem trolada ("5000000000")
--    ganhe o lote e o trave até virar calote. NULL = sem teto.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS max_bid BIGINT;

-- 7. sessions.token passa a guardar o SHA-256 do token, não o token em si
--    (mesmo tratamento já dado a login_codes.code_hash). Sessões antigas
--    deixam de valer — os clientes só precisam entrar de novo.
DELETE FROM sessions;
COMMENT ON COLUMN sessions.token IS 'SHA-256 hex do token de sessão; o token em claro só existe no navegador';
