-- Unlimited-use deploy keys: max_uses = 0 now means "no link budget" — the
-- key can link any number of nodes until revoked (or expired, if a TTL was
-- set). The console renders 0 as an infinite budget (n/∞ uses) and the link
-- path skips the budget check when max_uses = 0. Additive and idempotent so
-- it can run alongside earlier migrations on any branch.

ALTER TABLE deploy_keys DROP CONSTRAINT IF EXISTS deploy_keys_max_uses_chk;

ALTER TABLE deploy_keys
    ADD CONSTRAINT deploy_keys_max_uses_chk
    CHECK (max_uses >= 0);
