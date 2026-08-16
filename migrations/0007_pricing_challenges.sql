-- Pricing-scoped node credential challenges (Step 3).
--
-- Sellers publish asks at POST /internal/pricing with a credential whose
-- audience is api.opentela.ai/internal/pricing, distinct from the ACL
-- audience so a credential issued for one cannot authorize the other. A
-- permissionless provider has no trusted-region membership, so the pricing
-- challenge carries no region/role: the node_role CHECK (which forbids the
-- empty role used for pricing challenges) is dropped, and a new audience
-- column isolates the two flows. ACL challenges keep their non-empty region
-- and role; the ACL handlers set audience = 'api.opentela.ai/internal/acl'.

ALTER TABLE node_credential_challenges DROP CONSTRAINT IF EXISTS node_credential_challenges_role_chk;

ALTER TABLE node_credential_challenges
    ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'api.opentela.ai/internal/acl';

CREATE INDEX IF NOT EXISTS idx_node_credential_challenges_audience
    ON node_credential_challenges (audience, peer_id, expires_at DESC);
