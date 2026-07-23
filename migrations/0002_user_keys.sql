-- Adds ownership + a non-secret display prefix to api_keys.
-- Additive and idempotent so it can run alongside 0001 on any branch.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS user_id    TEXT;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_prefix TEXT;

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id
    ON api_keys (user_id) WHERE user_id IS NOT NULL;
