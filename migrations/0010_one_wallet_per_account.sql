-- One OpenTela Cloud account operates a single linked wallet, and every peer
-- it claims is owned by that wallet (see LinkWallet's one-wallet-per-account
-- enforcement in internal/store/acl.go). This migration hardens that rule in
-- the schema with a UNIQUE constraint on user_wallets(account_id).
--
-- Self-guarding: if any account currently has more than one wallet, this
-- raises a clear exception naming them and applies nothing. The keyctl
-- preDeploy step then fails, which keeps Railway on the previous deployment
-- (no downtime) until the extra wallets are reconciled away.
DO $$
DECLARE
    n int;
BEGIN
    SELECT count(*) INTO n
      FROM (SELECT account_id FROM user_wallets GROUP BY account_id HAVING count(*) > 1) x;
    IF n > 0 THEN
        RAISE EXCEPTION
            'cannot add UNIQUE(account_id): % account(s) have more than one linked wallet; reconcile first',
            n
            USING HINT = 'Delete each extra wallet after releasing the instances it owns, then redeploy.';
    END IF;

    ALTER TABLE user_wallets
        ADD CONSTRAINT user_wallets_account_unique UNIQUE (account_id);

    -- The per-account "one primary" partial index is now redundant: with at
    -- most one wallet per account, that wallet is always primary. Drop the
    -- partial index; the is_primary column stays (always true) so queries and
    -- responses remain unchanged.
    DROP INDEX IF EXISTS idx_user_wallets_primary;
END $$;
