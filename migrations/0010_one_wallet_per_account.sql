-- One OpenTela Cloud account operates a single linked wallet, and every peer
-- it claims is owned by that wallet (see LinkWallet's one-wallet-per-account
-- enforcement in internal/store/acl.go). This hardens the rule in the schema
-- with a UNIQUE constraint on user_wallets(account_id).
--
-- Backward-compatible per the database-migration skill: this is a constraint
-- addition only (no drops/renames). The previous app version keeps working
-- against the new schema -- its normal first-wallet / claim / delete paths are
-- unchanged; the only divergence is that a second-wallet link (already
-- forbidden by the new code, and never used pre-launch) raises a unique
-- violation instead of succeeding. The now-redundant partial index
-- idx_user_wallets_primary is left in place so a rollback keeps that index
-- available without re-running any migration.
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
END $$;
