-- Motivo do cancelamento, para o relatório explicar por que um lote
-- sumiu em vez de só mostrar 'cancelled'.
ALTER TABLE lots ADD COLUMN IF NOT EXISTS cancel_reason TEXT;
