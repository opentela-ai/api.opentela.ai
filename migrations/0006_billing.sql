-- Billing plane (Phase 0, devnet). Replaces the draft 0006_market.sql that
-- never shipped: peer asks now carry three rates (regular input, cached
-- input, output), account credit has a reserved projection and three
-- nullable caps, and every charge flows through durable, idempotent request
-- reservations and an immutable ledger instead of a lossy in-memory
-- accumulator. Caches may accelerate reads against these tables but every
-- spend is authorized and finalized by a row-locked transaction here.

-- peer_asks: the market. Each operator publishes their own price per
-- (peer_id, service, model); OpenTela only enforces expiry and validates
-- ranges. `revision` is bumped to a fresh value across a full replacement so
-- a snapshot taken at the gate can detect that the ask moved by settlement.
CREATE TABLE IF NOT EXISTS peer_asks (
    peer_id                   TEXT NOT NULL,
    service                   TEXT NOT NULL,
    model                     TEXT NOT NULL,
    input_per_million         BIGINT NOT NULL,
    cached_input_per_million  BIGINT NOT NULL,
    output_per_million        BIGINT NOT NULL,
    revision                  BIGINT NOT NULL DEFAULT 1,
    expires_at                TIMESTAMPTZ NOT NULL,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (peer_id, service, model),
    CONSTRAINT peer_asks_rates_nonneg_chk
        CHECK (input_per_million >= 0
           AND cached_input_per_million >= 0
           AND output_per_million >= 0),
    -- 1e12 base units per 1M tokens is ~1e6 OTELA/1M at 9 decimals; far past
    -- any sane ask, while keeping multiplication by token counts (<= ~1e7)
    -- comfortably inside signed int64 (max ~9.2e18) before division.
    CONSTRAINT peer_asks_rates_bound_chk
        CHECK (input_per_million    < 1000000000000
           AND cached_input_per_million < 1000000000000
           AND output_per_million   < 1000000000000)
);

CREATE INDEX IF NOT EXISTS idx_peer_asks_model
    ON peer_asks (model, input_per_million);

-- account_credits: one fungible, withdrawable API-credit balance per account.
-- `reserved_raw` is the sum of in-flight request reservations; it is a part of
-- (not in addition to) `credit_raw`, so the spendable balance is always
-- `credit_raw - reserved_raw` and the invariant `0 <= reserved_raw <=
-- credit_raw` holds. The three caps are the buyer's per-1M-token ceilings
-- (NULL = unlimited on that dimension).
CREATE TABLE IF NOT EXISTS account_credits (
    account_id                  TEXT PRIMARY KEY,
    credit_raw                  BIGINT NOT NULL DEFAULT 0,
    reserved_raw                BIGINT NOT NULL DEFAULT 0,
    max_input_per_million       BIGINT,
    max_cached_input_per_million BIGINT,
    max_output_per_million      BIGINT,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_credits_nonneg_chk
        CHECK (credit_raw >= 0 AND reserved_raw >= 0),
    CONSTRAINT account_credits_reserved_chk
        CHECK (reserved_raw <= credit_raw),
    CONSTRAINT account_credits_caps_nonneg_chk
        CHECK ((max_input_per_million IS NULL OR max_input_per_million >= 0)
           AND (max_cached_input_per_million IS NULL OR max_cached_input_per_million >= 0)
           AND (max_output_per_million IS NULL OR max_output_per_million >= 0))
);

-- billing_requests: the durable, idempotent reservation and settlement record.
-- `eligible_peers` is the immutable quote snapshot taken at the gate (each
-- entry carries the peer id, seller account, owner wallet, ask revision, and
-- the three rates); settlement resolves X-Computing-Node against this snapshot
-- and prices at it, never at a fresh read. `state` is the durable state
-- machine: reserved -> settled (2xx, complete usage) | released (non-2xx,
-- missing/unsupported usage, zero-price/unpriced peer, or stale recovery).
CREATE TABLE IF NOT EXISTS billing_requests (
    request_id                  TEXT PRIMARY KEY,
    buyer_account_id            TEXT NOT NULL,
    service                     TEXT NOT NULL,
    model                       TEXT NOT NULL,
    eligible_peers              JSONB NOT NULL,
    -- Effective caps applied at the gate (account defaults overridden by
    -- per-request headers). NULL on a dimension = unlimited.
    max_input_per_million       BIGINT,
    max_cached_input_per_million BIGINT,
    max_output_per_million      BIGINT,
    reserved_raw                BIGINT NOT NULL,
    state                       TEXT NOT NULL DEFAULT 'reserved',
    -- Populated at settlement from the served peer's snapshot entry.
    served_peer_id              TEXT,
    served_seller_account_id    TEXT,
    served_revision             BIGINT,
    served_input_per_million    BIGINT,
    served_cached_input_per_million BIGINT,
    served_output_per_million   BIGINT,
    input_tokens                INT,
    cached_input_tokens         INT,
    output_tokens               INT,
    cost_raw                    BIGINT,
    fee_raw                     BIGINT,
    seller_raw                  BIGINT,
    reserved_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at                  TIMESTAMPTZ,
    released_at                 TIMESTAMPTZ,
    release_reason              TEXT,
    CONSTRAINT billing_requests_state_chk
        CHECK (state IN ('reserved', 'settled', 'released')),
    CONSTRAINT billing_requests_reserved_nonneg_chk
        CHECK (reserved_raw >= 0)
);

CREATE INDEX IF NOT EXISTS idx_billing_requests_buyer
    ON billing_requests (buyer_account_id, reserved_at DESC, request_id);
CREATE INDEX IF NOT EXISTS idx_billing_requests_open
    ON billing_requests (state, reserved_at) WHERE state = 'reserved';

-- credit_ledger: the immutable, authoritative balance movements. Every
-- successful settlement, deposit credit, withdrawal, and manual adjustment
-- produces exactly one row per role, and `UNIQUE (ref, leg)` makes the whole
-- system idempotent: a retried settlement replays the same (request_id, leg)
-- and is rejected as a duplicate rather than double-charging. NULL refs are
-- distinct under Postgres uniqueness, so uncategorized adjustments never
-- collide. `account_credits` is the transactional projection; reconciliation
-- (internal/store/billing.go) verifies it against SUM(delta_raw) over this
-- table plus the open reservations.
CREATE TABLE IF NOT EXISTS credit_ledger (
    id                          BIGSERIAL PRIMARY KEY,
    account_id                  TEXT NOT NULL,
    delta_raw                   BIGINT NOT NULL,
    source                      TEXT NOT NULL,
    leg                         TEXT NOT NULL,
    counterparty                TEXT,
    ref                         TEXT,
    model                       TEXT,
    input_per_million           BIGINT,
    cached_input_per_million    BIGINT,
    output_per_million          BIGINT,
    input_tokens                INT,
    cached_input_tokens         INT,
    output_tokens               INT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT credit_ledger_source_chk
        CHECK (source IN ('usage', 'earn', 'fee', 'deposit', 'withdraw', 'adjust')),
    CONSTRAINT credit_ledger_unique UNIQUE (ref, leg)
);

CREATE INDEX IF NOT EXISTS idx_credit_ledger_account
    ON credit_ledger (account_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_credit_ledger_counterparty
    ON credit_ledger (counterparty, created_at DESC, id DESC);
-- The accounts view cursor: created_at (nulls last) + id is globally unique
-- and stable under the BIGSERIAL id.
CREATE INDEX IF NOT EXISTS idx_credit_ledger_cursor
    ON credit_ledger (created_at DESC, id DESC);

-- deposit_events: one row per (transaction_signature, instruction_index) of
-- an inbound SPL transfer into the treasury ATA. Every transfer instruction is
-- persisted BEFORE any credit is applied, so a crash mid-watch can never lose
-- or double-count a deposit. `assignment_state` tracks attribution: a transfer
-- from a wallet not yet linked is recorded as 'unassigned' and credited
-- automatically once that wallet is linked (ReconcileDepositsForWallet).
-- Account IDs are never accepted from untrusted memos.
CREATE TABLE IF NOT EXISTS deposit_events (
    transaction_signature      TEXT NOT NULL,
    instruction_index          INT NOT NULL,
    slot                       BIGINT NOT NULL,
    from_wallet                TEXT NOT NULL,
    amount_raw                 BIGINT NOT NULL,
    assigned_account_id        TEXT,
    assignment_state           TEXT NOT NULL DEFAULT 'unassigned',
    credited_at                TIMESTAMPTZ,
    seen_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transaction_signature, instruction_index),
    CONSTRAINT deposit_events_amount_chk CHECK (amount_raw > 0),
    CONSTRAINT deposit_events_state_chk
        CHECK (assignment_state IN ('unassigned', 'assigned', 'skipped'))
);

CREATE INDEX IF NOT EXISTS idx_deposit_events_wallet
    ON deposit_events (from_wallet, seen_at DESC, transaction_signature, instruction_index);
CREATE INDEX IF NOT EXISTS idx_deposit_events_unassigned
    ON deposit_events (from_wallet, seen_at) WHERE assignment_state = 'unassigned';

-- withdrawals: the durable state machine for off-chain earnings -> on-chain
-- OTELA. reserve (decrement account_credits, persist idempotency key) ->
-- signed (persist the signed wire transaction and its deterministic signature
-- before broadcast) -> broadcast -> finalized. On an ambiguous RPC result the
-- worker keeps the row reserved/signed and queries transaction status; credit
-- is restored only after the blockhash expiry proves the transaction cannot
-- land.
CREATE TABLE IF NOT EXISTS withdrawals (
    id                          BIGSERIAL PRIMARY KEY,
    account_id                  TEXT NOT NULL,
    idempotency_key             TEXT NOT NULL UNIQUE,
    destination_wallet          TEXT NOT NULL,
    amount_raw                  BIGINT NOT NULL,
    state                       TEXT NOT NULL DEFAULT 'reserved',
    signed_wire                 TEXT,
    signature                   TEXT,
    blockhash                   TEXT,
    blockhash_expires_at        TIMESTAMPTZ,
    error                       TEXT,
    reserved_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    signed_at                   TIMESTAMPTZ,
    broadcast_at                TIMESTAMPTZ,
    finalized_at                TIMESTAMPTZ,
    CONSTRAINT withdrawals_amount_chk CHECK (amount_raw > 0),
    CONSTRAINT withdrawals_state_chk
        CHECK (state IN ('reserved', 'signed', 'broadcast', 'finalized', 'failed', 'restored'))
);

CREATE INDEX IF NOT EXISTS idx_withdrawals_account
    ON withdrawals (account_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_withdrawals_open
    ON withdrawals (state, reserved_at) WHERE state IN ('reserved', 'signed', 'broadcast');
