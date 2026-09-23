-- Model sharing via consumer API keys (issue #16): extends both ACL rule
-- kind CHECKs with 'api_key', whose value is the caller's non-secret display
-- prefix (sk- + first 8 hex chars). Owners can allow-list specific consumer
-- keys on restricted instances and services. Additive and idempotent so it
-- can run alongside earlier migrations on any branch.

ALTER TABLE instance_acl_rules DROP CONSTRAINT IF EXISTS instance_acl_rules_kind_chk;

ALTER TABLE instance_acl_rules
    ADD CONSTRAINT instance_acl_rules_kind_chk
    CHECK (rule_kind IN ('email_domain', 'wallet', 'api_key'));

ALTER TABLE instance_service_acl_rules DROP CONSTRAINT IF EXISTS instance_service_acl_rules_kind_chk;

ALTER TABLE instance_service_acl_rules
    ADD CONSTRAINT instance_service_acl_rules_kind_chk
    CHECK (rule_kind IN ('email_domain', 'wallet', 'api_key'));
