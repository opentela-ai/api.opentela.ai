-- Phase 2 (design §11.5): durable state machine for on-chain delegation
-- settlement. Each row is one batched transfer_checked (buyer ATA ->
-- destination ATA) signed by the settlement authority, replaying legs the
-- off-chain ledger already debited. The batch_ref is UNIQUE so a re-created
-- batch can never double-transfer, and the legs table anchors exactly-once
-- per ledger leg (UNIQUE ledger_leg_id) — the same (ref, leg) model as §6.
CREATE TABLE IF NOT EXISTS delegation_settlements (
    id                      BIGSERIAL PRIMARY KEY,
    batch_ref               TEXT NOT NULL UNIQUE,
    buyer_account           TEXT NOT NULL,
    delegate                TEXT NOT NULL,
    source_ata              TEXT NOT NULL,
    destination_wallet      TEXT NOT NULL,
    destination_ata         TEXT NOT NULL,
    amount_raw              BIGINT NOT NULL CHECK (amount_raw > 0),
    state                   TEXT NOT NULL DEFAULT 'pending',
    signed_wire             TEXT,
    tx_signature            TEXT,
    blockhash               TEXT,
    last_valid_block_height BIGINT,
    blockhash_expires_at    TIMESTAMPTZ,
    fail_reason             TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_delegation_settlements_state
    ON delegation_settlements (state);

-- Exactly-once per ledger leg: a leg can be carried by at most one
-- settlement batch, ever. The cursor only bounds the scan; this constraint
-- is what makes a cursor regression (or a concurrent worker) harmless.
CREATE TABLE IF NOT EXISTS delegation_settlement_legs (
    settlement_id BIGINT NOT NULL REFERENCES delegation_settlements(id) ON DELETE CASCADE,
    ledger_leg_id BIGINT NOT NULL UNIQUE,
    PRIMARY KEY (settlement_id, ledger_leg_id)
);

-- Per-delegate scan cursor over the earn legs (mirrors deposit_cursors).
CREATE TABLE IF NOT EXISTS delegation_settlement_cursors (
    delegate    TEXT PRIMARY KEY,
    last_leg_id BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
