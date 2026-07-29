ALTER TABLE instances
    ADD COLUMN IF NOT EXISTS policy_scope TEXT NOT NULL DEFAULT 'peer';

ALTER TABLE instances
    DROP CONSTRAINT IF EXISTS instances_policy_scope_chk;

ALTER TABLE instances
    ADD CONSTRAINT instances_policy_scope_chk
        CHECK (policy_scope IN ('peer', 'service'));

CREATE TABLE IF NOT EXISTS trusted_regions (
    id               BIGSERIAL PRIMARY KEY,
    slug             TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    owner_account_id TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active',
    region_revision  BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trusted_regions_slug_chk
        CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$' OR slug ~ '^[a-z0-9]$'),
    CONSTRAINT trusted_regions_status_chk
        CHECK (status IN ('active', 'disabled'))
);

CREATE INDEX IF NOT EXISTS idx_trusted_regions_owner
    ON trusted_regions (owner_account_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS trusted_region_memberships (
    instance_id             BIGINT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
    region_id               BIGINT NOT NULL REFERENCES trusted_regions(id) ON DELETE RESTRICT,
    node_role               TEXT NOT NULL,
    status                  TEXT NOT NULL,
    admitted_by_account_id  TEXT,
    admission_reason        TEXT,
    expires_at              TIMESTAMPTZ,
    ownership_verified_at   TIMESTAMPTZ,
    membership_revision     BIGINT NOT NULL DEFAULT 1,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trusted_region_memberships_role_chk
        CHECK (node_role IN ('worker', 'head', 'combined')),
    CONSTRAINT trusted_region_memberships_status_chk
        CHECK (status IN ('active', 'suspended', 'expired', 'revoked', 'ownership_mismatch', 'ownership_unavailable')),
    CONSTRAINT trusted_region_memberships_region_instance_unique
        UNIQUE (region_id, instance_id)
);

CREATE INDEX IF NOT EXISTS idx_trusted_region_memberships_region
    ON trusted_region_memberships (region_id, status, updated_at DESC, instance_id);

CREATE TABLE IF NOT EXISTS trusted_region_membership_events (
    id                  BIGSERIAL PRIMARY KEY,
    instance_id         BIGINT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    region_id           BIGINT REFERENCES trusted_regions(id) ON DELETE SET NULL,
    event_kind          TEXT NOT NULL,
    actor_account_id    TEXT,
    membership_revision BIGINT NOT NULL,
    payload             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trusted_region_membership_events_kind_chk
        CHECK (event_kind IN (
            'invited',
            'accepted',
            'suspended',
            'reactivated',
            'revoked',
            'released',
            'expired',
            'ownership_mismatch',
            'ownership_unavailable',
            'region_disabled',
            'region_enabled'
        ))
);

CREATE INDEX IF NOT EXISTS idx_trusted_region_membership_events_instance
    ON trusted_region_membership_events (instance_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS trusted_region_invitations (
    id                     BIGSERIAL PRIMARY KEY,
    region_id              BIGINT NOT NULL REFERENCES trusted_regions(id) ON DELETE CASCADE,
    instance_id            BIGINT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    created_by_account_id  TEXT NOT NULL,
    acceptance_token_hash  TEXT NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'pending',
    node_role              TEXT NOT NULL,
    expires_at             TIMESTAMPTZ NOT NULL,
    accepted_at            TIMESTAMPTZ,
    cancelled_at           TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trusted_region_invitations_status_chk
        CHECK (status IN ('pending', 'accepted', 'expired', 'cancelled')),
    CONSTRAINT trusted_region_invitations_role_chk
        CHECK (node_role IN ('worker', 'head', 'combined'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_trusted_region_invitations_pending
    ON trusted_region_invitations (region_id, instance_id)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS instance_services (
    id                      BIGSERIAL PRIMARY KEY,
    instance_id             BIGINT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    service_name            TEXT NOT NULL,
    exposure                TEXT NOT NULL,
    region_id               BIGINT REFERENCES trusted_regions(id) ON DELETE RESTRICT,
    access_mode             TEXT NOT NULL DEFAULT 'inherit',
    service_policy_revision BIGINT NOT NULL DEFAULT 1,
    observed_present        BOOLEAN NOT NULL DEFAULT FALSE,
    observed_last_seen_at   TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT instance_services_exposure_chk
        CHECK (exposure IN ('permissionless', 'trusted_region', 'disabled')),
    CONSTRAINT instance_services_access_mode_chk
        CHECK (access_mode IN ('inherit', 'public', 'restricted')),
    CONSTRAINT instance_services_name_chk
        CHECK (service_name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$'),
    CONSTRAINT instance_services_unique
        UNIQUE (instance_id, service_name),
    CONSTRAINT instance_services_region_exposure_chk
        CHECK (
            (exposure = 'trusted_region' AND region_id IS NOT NULL) OR
            (exposure IN ('permissionless', 'disabled') AND region_id IS NULL)
        ),
    CONSTRAINT instance_services_membership_fk
        FOREIGN KEY (region_id, instance_id)
        REFERENCES trusted_region_memberships(region_id, instance_id)
        ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_instance_services_instance
    ON instance_services (instance_id, service_name);

CREATE TABLE IF NOT EXISTS instance_service_acl_rules (
    id                  BIGSERIAL PRIMARY KEY,
    instance_service_id BIGINT NOT NULL REFERENCES instance_services(id) ON DELETE CASCADE,
    rule_kind           TEXT NOT NULL,
    rule_value          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT instance_service_acl_rules_kind_chk
        CHECK (rule_kind IN ('email_domain', 'wallet')),
    CONSTRAINT instance_service_acl_rules_unique
        UNIQUE (instance_service_id, rule_kind, rule_value)
);

CREATE INDEX IF NOT EXISTS idx_instance_service_acl_rules_service
    ON instance_service_acl_rules (instance_service_id, rule_kind, rule_value);

CREATE TABLE IF NOT EXISTS node_credential_challenges (
    id                 TEXT PRIMARY KEY,
    peer_id            TEXT NOT NULL,
    region_slug        TEXT NOT NULL,
    node_role          TEXT NOT NULL,
    nonce_hash         TEXT NOT NULL,
    challenge_message  TEXT NOT NULL,
    issued_at          TIMESTAMPTZ NOT NULL,
    expires_at         TIMESTAMPTZ NOT NULL,
    consumed_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT node_credential_challenges_role_chk
        CHECK (node_role IN ('worker', 'head', 'combined'))
);

CREATE INDEX IF NOT EXISTS idx_node_credential_challenges_peer
    ON node_credential_challenges (peer_id, expires_at DESC);
