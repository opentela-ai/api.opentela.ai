-- Scoped deploy keys for instance linking (Phase 1 of private linking).
--
-- A deploy key ("otd-...") authorizes exactly one operation: registering
-- (linking) an instance under the issuing account. Unlike the account JWT the
-- node previously needed, a deploy key cannot read or mutate anything else,
-- is usage-capped (max_uses), expires, and is revocable from the console.
-- Keys are stored as SHA-256 digests (key_hash) with a non-secret display
-- prefix (key_prefix), mirroring api_keys (0001/0002).
--
-- Instances linked through a deploy key are bound to the ACCOUNT privately:
-- ownership is proven by the node's libp2p key signing a server challenge,
-- not by a public mesh wallet observation, so the linking path never needs
-- the operator's wallet identity (see 0004: node_credential_challenges,
-- audience-scoped; the link flow uses audience
-- 'api.opentela.ai/internal/instances/link').

CREATE TABLE IF NOT EXISTS deploy_keys (
    id           BIGSERIAL PRIMARY KEY,
    user_id      TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    name         TEXT,
    max_uses     INT  NOT NULL DEFAULT 1,
    use_count    INT  NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    CONSTRAINT deploy_keys_max_uses_chk CHECK (max_uses >= 1)
);

CREATE INDEX IF NOT EXISTS idx_deploy_keys_user
    ON deploy_keys (user_id, created_at DESC);
