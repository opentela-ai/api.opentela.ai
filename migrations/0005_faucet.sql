-- One faucet claim per account. The PRIMARY KEY on account_id makes claims
-- race-safe: the API inserts a pending row (tx_signature = '') BEFORE the
-- on-chain transfer, so a concurrent duplicate claim is rejected by the
-- unique constraint and never sends a second transaction. On success the
-- row is completed via UPDATE; on failure it is deleted so the account can
-- retry. A stale pending claim (server crash mid-flight) is taken over again
-- after 2 minutes — past the Solana blockhash validity window, so the
-- original transaction (if any) can no longer confirm.
CREATE TABLE IF NOT EXISTS faucet_claims (
    account_id   TEXT PRIMARY KEY,
    wallet       TEXT NOT NULL,
    amount_raw   BIGINT NOT NULL,
    tx_signature TEXT NOT NULL,
    claimed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_faucet_claims_wallet
    ON faucet_claims (wallet, claimed_at DESC);
