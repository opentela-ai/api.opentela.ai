CREATE TABLE IF NOT EXISTS account_identities (
    account_id        TEXT PRIMARY KEY,
    email             TEXT NOT NULL DEFAULT '',
    email_domain      TEXT NOT NULL DEFAULT '',
    email_verified    BOOLEAN NOT NULL DEFAULT FALSE,
    last_verified_at  TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_wallets (
    id         BIGSERIAL PRIMARY KEY,
    account_id TEXT NOT NULL,
    wallet     TEXT NOT NULL UNIQUE,
    is_primary BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_wallets_account_wallet_unique UNIQUE (account_id, wallet)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_wallets_primary
    ON user_wallets (account_id) WHERE is_primary = TRUE;

CREATE INDEX IF NOT EXISTS idx_user_wallets_account
    ON user_wallets (account_id, created_at, id);

CREATE TABLE IF NOT EXISTS wallet_challenges (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    wallet     TEXT NOT NULL,
    nonce      TEXT NOT NULL,
    message    TEXT NOT NULL,
    issued_at  TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_wallet_challenges_lookup
    ON wallet_challenges (account_id, id);

CREATE TABLE IF NOT EXISTS instances (
    id                      BIGSERIAL PRIMARY KEY,
    account_id              TEXT NOT NULL,
    peer_id                 TEXT NOT NULL UNIQUE,
    label                   TEXT NOT NULL DEFAULT '',
    owner_wallet            TEXT NOT NULL,
    access_mode             TEXT NOT NULL DEFAULT 'restricted',
    policy_revision         BIGINT NOT NULL DEFAULT 1,
    ownership_status        TEXT NOT NULL DEFAULT 'active',
    observed_wallet         TEXT,
    ownership_observed_at   TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT instances_access_mode_chk
        CHECK (access_mode IN ('public', 'restricted')),
    CONSTRAINT instances_ownership_status_chk
        CHECK (ownership_status IN ('active', 'mismatch', 'unavailable')),
    CONSTRAINT instances_owner_wallet_fk
        FOREIGN KEY (account_id, owner_wallet)
        REFERENCES user_wallets(account_id, wallet)
        ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_instances_account
    ON instances (account_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_instances_owner_wallet
    ON instances (owner_wallet);

CREATE TABLE IF NOT EXISTS instance_acl_rules (
    id          BIGSERIAL PRIMARY KEY,
    instance_id BIGINT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    rule_kind   TEXT NOT NULL,
    rule_value  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT instance_acl_rules_kind_chk
        CHECK (rule_kind IN ('email_domain', 'wallet')),
    CONSTRAINT instance_acl_rules_unique
        UNIQUE (instance_id, rule_kind, rule_value)
);

CREATE INDEX IF NOT EXISTS idx_instance_acl_rules_instance
    ON instance_acl_rules (instance_id, rule_kind, rule_value);
