-- Persist the chain-derived expiry proof for withdrawals. Additive so it can
-- roll out safely before any code starts writing the new field.
ALTER TABLE withdrawals
    ADD COLUMN IF NOT EXISTS last_valid_block_height BIGINT;
