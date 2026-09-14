-- Phase 2 (design §11): non-custodial settlement — delegated SPL spending.
--
-- Increment 1 is off-chain only: the allowance registry mirrors each buyer's
-- SPL `approve` to the settlement authority (design §11.2), the reserve gate
-- bounds spend by min(credit available, Σ active allowances) (§11.3), and a
-- grant is mirrored into the credit projection through exactly-once ledger
-- legs (the ledger stays the source of truth, §11.4.1).

-- New ledger source for delegation grants/revocations (backing events, not
-- usage). The named constraint is rebuilt to admit it.
ALTER TABLE credit_ledger DROP CONSTRAINT credit_ledger_source_chk;
ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_source_chk
    CHECK (source IN ('usage', 'earn', 'fee', 'deposit', 'withdraw', 'adjust', 'delegation'));

-- Allowance registry: one row per (account, delegate), mirroring the on-chain
-- SPL delegated amount of the buyer's OTELA ATA to the settlement authority.
-- `delegate` is the base58 pubkey of the settlement authority named by the
-- buyer's approve. allowance_raw is the absolute delegated amount in raw
-- µUSDC; revoked_at is set when the allowance drops to zero and cleared when
-- a later grant revives it.
CREATE TABLE IF NOT EXISTS account_allowances (
    account_id    TEXT        NOT NULL,
    delegate      TEXT        NOT NULL,
    allowance_raw BIGINT      NOT NULL,
    approved_at   TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    CONSTRAINT account_allowances_pk PRIMARY KEY (account_id, delegate),
    CONSTRAINT account_allowances_nonneg_chk CHECK (allowance_raw >= 0),
    CONSTRAINT account_allowances_account_fk
        FOREIGN KEY (account_id) REFERENCES account_credits (account_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_account_allowances_delegate
    ON account_allowances (delegate) WHERE revoked_at IS NULL;
