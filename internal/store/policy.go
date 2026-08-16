package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	PolicyScopePeer    = "peer"
	PolicyScopeService = "service"

	ExposurePermissionless = "permissionless"
	ExposureTrustedRegion  = "trusted_region"
	ExposureDisabled       = "disabled"

	AccessModeInherit    = "inherit"
	AccessModePublic     = "public"
	AccessModeRestricted = "restricted"
)

func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (p *Postgres) GetRegionByIDForOwner(ctx context.Context, ownerAccountID string, regionID int64) (RegionInfo, error) {
	var out RegionInfo
	err := p.pool.QueryRow(ctx, `
		SELECT id, slug, name, owner_account_id, status, region_revision, created_at, updated_at
		FROM trusted_regions
		WHERE id = $1 AND owner_account_id = $2`, regionID, ownerAccountID).
		Scan(&out.ID, &out.Slug, &out.Name, &out.OwnerAccountID, &out.Status, &out.RegionRevision, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionInfo{}, ErrNotFound
	}
	if err != nil {
		return RegionInfo{}, fmt.Errorf("store: get region by owner: %w", err)
	}
	return out, nil
}

func (p *Postgres) GetRegionByID(ctx context.Context, regionID int64) (RegionInfo, error) {
	var out RegionInfo
	err := p.pool.QueryRow(ctx, `
		SELECT id, slug, name, owner_account_id, status, region_revision, created_at, updated_at
		FROM trusted_regions
		WHERE id = $1`, regionID).
		Scan(&out.ID, &out.Slug, &out.Name, &out.OwnerAccountID, &out.Status, &out.RegionRevision, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionInfo{}, ErrNotFound
	}
	if err != nil {
		return RegionInfo{}, fmt.Errorf("store: get region: %w", err)
	}
	return out, nil
}

func (p *Postgres) ListRegionsByOwner(ctx context.Context, ownerAccountID string) ([]RegionInfo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, slug, name, owner_account_id, status, region_revision, created_at, updated_at
		FROM trusted_regions
		WHERE owner_account_id = $1
		ORDER BY created_at DESC, id DESC`, ownerAccountID)
	if err != nil {
		return nil, fmt.Errorf("store: list regions: %w", err)
	}
	defer rows.Close()
	var out []RegionInfo
	for rows.Next() {
		var item RegionInfo
		if err := rows.Scan(&item.ID, &item.Slug, &item.Name, &item.OwnerAccountID, &item.Status, &item.RegionRevision, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan region: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: region rows: %w", err)
	}
	return out, nil
}

func (p *Postgres) CreateRegion(ctx context.Context, in RegionInfo) (RegionInfo, error) {
	var out RegionInfo
	err := p.pool.QueryRow(ctx, `
		INSERT INTO trusted_regions (slug, name, owner_account_id, status)
		VALUES ($1, $2, $3, COALESCE(NULLIF($4, ''), 'active'))
		RETURNING id, slug, name, owner_account_id, status, region_revision, created_at, updated_at`,
		in.Slug, in.Name, in.OwnerAccountID, in.Status).
		Scan(&out.ID, &out.Slug, &out.Name, &out.OwnerAccountID, &out.Status, &out.RegionRevision, &out.CreatedAt, &out.UpdatedAt)
	if isUniqueViolation(err) {
		return RegionInfo{}, ErrConflict
	}
	if err != nil {
		return RegionInfo{}, fmt.Errorf("store: create region: %w", err)
	}
	return out, nil
}

func (p *Postgres) UpdateRegion(ctx context.Context, ownerAccountID string, regionID int64, name, status string) (RegionInfo, error) {
	var out RegionInfo
	err := p.pool.QueryRow(ctx, `
		UPDATE trusted_regions
		SET name = $3,
		    status = $4,
		    region_revision = CASE WHEN status <> $4 THEN region_revision + 1 ELSE region_revision END,
		    updated_at = now()
		WHERE id = $1 AND owner_account_id = $2
		RETURNING id, slug, name, owner_account_id, status, region_revision, created_at, updated_at`,
		regionID, ownerAccountID, name, status).
		Scan(&out.ID, &out.Slug, &out.Name, &out.OwnerAccountID, &out.Status, &out.RegionRevision, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionInfo{}, ErrNotFound
	}
	if err != nil {
		return RegionInfo{}, fmt.Errorf("store: update region: %w", err)
	}
	return out, nil
}

func (p *Postgres) DeleteRegion(ctx context.Context, ownerAccountID string, regionID int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM trusted_regions WHERE id = $1 AND owner_account_id = $2`, regionID, ownerAccountID)
	if isForeignKeyViolation(err) {
		return false, ErrConflict
	}
	if err != nil {
		return false, fmt.Errorf("store: delete region: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) ListRegionMembershipsByOwner(ctx context.Context, ownerAccountID string, regionID int64) ([]RegionMembership, []RegionInvitation, error) {
	if _, err := p.GetRegionByIDForOwner(ctx, ownerAccountID, regionID); err != nil {
		return nil, nil, err
	}
	members, err := p.listMemberships(ctx, `trm.region_id = $1`, regionID)
	if err != nil {
		return nil, nil, err
	}
	invitations, err := p.listInvitations(ctx, `tri.region_id = $1`, regionID)
	if err != nil {
		return nil, nil, err
	}
	return members, invitations, nil
}

func (p *Postgres) CreateRegionInvitation(ctx context.Context, ownerAccountID string, regionID, instanceID int64, nodeRole, token string, expiresAt time.Time) (RegionInvitation, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RegionInvitation{}, fmt.Errorf("store: begin create invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var region RegionInfo
	if err := tx.QueryRow(ctx, `
		SELECT id, slug, name, owner_account_id, status, region_revision, created_at, updated_at
		FROM trusted_regions WHERE id = $1 AND owner_account_id = $2`,
		regionID, ownerAccountID).
		Scan(&region.ID, &region.Slug, &region.Name, &region.OwnerAccountID, &region.Status, &region.RegionRevision, &region.CreatedAt, &region.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return RegionInvitation{}, ErrNotFound
	} else if err != nil {
		return RegionInvitation{}, fmt.Errorf("store: lock region for invitation: %w", err)
	}

	var instOwner, peerID, label string
	if err := tx.QueryRow(ctx, `SELECT account_id, peer_id, label FROM instances WHERE id = $1`, instanceID).Scan(&instOwner, &peerID, &label); errors.Is(err, pgx.ErrNoRows) {
		return RegionInvitation{}, ErrNotFound
	} else if err != nil {
		return RegionInvitation{}, fmt.Errorf("store: load invitation instance: %w", err)
	}

	var out RegionInvitation
	err = tx.QueryRow(ctx, `
		INSERT INTO trusted_region_invitations
		    (region_id, instance_id, created_by_account_id, acceptance_token_hash, status, node_role, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6)
		RETURNING id, region_id, instance_id, created_by_account_id, acceptance_token_hash, status, node_role,
		          expires_at, accepted_at, cancelled_at, created_at, updated_at`,
		regionID, instanceID, ownerAccountID, hashSecret(token), nodeRole, expiresAt.UTC()).
		Scan(&out.ID, &out.RegionID, &out.InstanceID, &out.CreatedByAccountID, &out.AcceptanceTokenHash, &out.Status, &out.NodeRole,
			&out.ExpiresAt, &out.AcceptedAt, &out.CancelledAt, &out.CreatedAt, &out.UpdatedAt)
	if isUniqueViolation(err) {
		return RegionInvitation{}, ErrConflict
	}
	if err != nil {
		return RegionInvitation{}, fmt.Errorf("store: insert invitation: %w", err)
	}
	out.RegionSlug = region.Slug
	out.PeerID = peerID
	out.Label = label

	payload, _ := json.Marshal(map[string]any{"invitation_id": out.ID, "status": out.Status})
	if _, err := tx.Exec(ctx, `
		INSERT INTO trusted_region_membership_events
		    (instance_id, region_id, event_kind, actor_account_id, membership_revision, payload)
		VALUES ($1, $2, 'invited', $3, 0, $4)`,
		instanceID, regionID, ownerAccountID, payload); err != nil {
		return RegionInvitation{}, fmt.Errorf("store: record invitation event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegionInvitation{}, fmt.Errorf("store: commit invitation: %w", err)
	}
	return out, nil
}

func (p *Postgres) AcceptRegionInvitation(ctx context.Context, actorAccountID string, regionID, instanceID int64, token string, ownershipVerifiedAt time.Time) (RegionMembership, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RegionMembership{}, fmt.Errorf("store: begin accept invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var instOwner string
	if err := tx.QueryRow(ctx, `SELECT account_id FROM instances WHERE id = $1`, instanceID).Scan(&instOwner); errors.Is(err, pgx.ErrNoRows) {
		return RegionMembership{}, ErrNotFound
	} else if err != nil {
		return RegionMembership{}, fmt.Errorf("store: lock instance for invitation acceptance: %w", err)
	}
	if instOwner != actorAccountID {
		return RegionMembership{}, ErrNotFound
	}

	var invitation RegionInvitation
	err = tx.QueryRow(ctx, `
		SELECT tri.id, tri.region_id, tr.slug, tri.instance_id, tri.created_by_account_id, tri.acceptance_token_hash,
		       tri.status, tri.node_role, tri.expires_at, tri.accepted_at, tri.cancelled_at, tri.created_at, tri.updated_at
		FROM trusted_region_invitations tri
		JOIN trusted_regions tr ON tr.id = tri.region_id
		WHERE tri.region_id = $1 AND tri.instance_id = $2 AND tri.status = 'pending'
		FOR UPDATE`,
		regionID, instanceID).
		Scan(&invitation.ID, &invitation.RegionID, &invitation.RegionSlug, &invitation.InstanceID, &invitation.CreatedByAccountID, &invitation.AcceptanceTokenHash,
			&invitation.Status, &invitation.NodeRole, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.CancelledAt, &invitation.CreatedAt, &invitation.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionMembership{}, ErrNotFound
	}
	if err != nil {
		return RegionMembership{}, fmt.Errorf("store: load pending invitation: %w", err)
	}
	if hashSecret(token) != invitation.AcceptanceTokenHash {
		return RegionMembership{}, ErrNotFound
	}
	now := ownershipVerifiedAt.UTC()
	if now.After(invitation.ExpiresAt.UTC()) {
		if _, err := tx.Exec(ctx, `
			UPDATE trusted_region_invitations SET status = 'expired', updated_at = now() WHERE id = $1`, invitation.ID); err != nil {
			return RegionMembership{}, fmt.Errorf("store: expire invitation: %w", err)
		}
		return RegionMembership{}, ErrChallengeExpired
	}

	// Migration guard. The membership table keys on instance_id alone, so an
	// invitation accepted while the instance is already an active member of a
	// *different* region would silently move it (ON CONFLICT DO UPDATE). That is
	// only safe once every trusted service binding tied to the old region has
	// been removed; otherwise the FK on instance_services(region_id, instance_id)
	// would dangle and the upsert surfaces as an opaque 503. Reject it here with
	// a meaningful, 409-mappable error instead.
	var existingRegionID int64
	switch err := tx.QueryRow(ctx, `
			SELECT region_id FROM trusted_region_memberships WHERE instance_id = $1`,
		instanceID).Scan(&existingRegionID); {
	case errors.Is(err, pgx.ErrNoRows):
		// no existing membership — fresh admission, nothing to guard against
	case err != nil:
		return RegionMembership{}, fmt.Errorf("store: load existing membership: %w", err)
	default:
		if existingRegionID != regionID {
			var bindings int
			if err := tx.QueryRow(ctx, `
					SELECT count(*) FROM instance_services
					WHERE instance_id = $1 AND region_id = $2 AND exposure = 'trusted_region'`,
				instanceID, existingRegionID).Scan(&bindings); err != nil {
				return RegionMembership{}, fmt.Errorf("store: count existing bindings: %w", err)
			}
			if bindings > 0 {
				return RegionMembership{}, ErrRegionMigrationConflict
			}
		}
	}

	var membership RegionMembership
	err = tx.QueryRow(ctx, `
		INSERT INTO trusted_region_memberships
		    (instance_id, region_id, node_role, status, admitted_by_account_id, ownership_verified_at)
		VALUES ($1, $2, $3, 'active', $4, $5)
		ON CONFLICT (instance_id) DO UPDATE SET
		    region_id = EXCLUDED.region_id,
		    node_role = EXCLUDED.node_role,
		    status = EXCLUDED.status,
		    admitted_by_account_id = EXCLUDED.admitted_by_account_id,
		    ownership_verified_at = EXCLUDED.ownership_verified_at,
		    membership_revision = trusted_region_memberships.membership_revision + 1,
		    updated_at = now()
		RETURNING instance_id, region_id, node_role, status, admitted_by_account_id, admission_reason,
		          expires_at, ownership_verified_at, membership_revision, created_at, updated_at`,
		instanceID, regionID, invitation.NodeRole, invitation.CreatedByAccountID, now).
		Scan(&membership.InstanceID, &membership.RegionID, &membership.NodeRole, &membership.Status, &membership.AdmittedByAccountID, &membership.AdmissionReason,
			&membership.ExpiresAt, &membership.OwnershipVerifiedAt, &membership.MembershipRevision, &membership.CreatedAt, &membership.UpdatedAt)
	if err != nil {
		return RegionMembership{}, fmt.Errorf("store: upsert membership: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE trusted_region_invitations
		SET status = 'accepted', accepted_at = now(), updated_at = now()
		WHERE id = $1`, invitation.ID); err != nil {
		return RegionMembership{}, fmt.Errorf("store: accept invitation row: %w", err)
	}
	var regionStatus string
	if err := tx.QueryRow(ctx, `SELECT slug, status FROM trusted_regions WHERE id = $1`, regionID).Scan(&membership.RegionSlug, &regionStatus); err != nil {
		return RegionMembership{}, fmt.Errorf("store: load accepted region: %w", err)
	}
	membership.RegionStatus = regionStatus
	if _, err := tx.Exec(ctx, `
		UPDATE instances
		SET policy_revision = policy_revision + 1,
		    updated_at = now()
		WHERE id = $1`, instanceID); err != nil {
		return RegionMembership{}, fmt.Errorf("store: bump instance revision after accept: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"node_role": membership.NodeRole, "status": membership.Status})
	if _, err := tx.Exec(ctx, `
		INSERT INTO trusted_region_membership_events
		    (instance_id, region_id, event_kind, actor_account_id, membership_revision, payload)
		VALUES ($1, $2, 'accepted', $3, $4, $5)`,
		instanceID, regionID, actorAccountID, membership.MembershipRevision, payload); err != nil {
		return RegionMembership{}, fmt.Errorf("store: record acceptance event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegionMembership{}, fmt.Errorf("store: commit accept invitation: %w", err)
	}
	return membership, nil
}

func (p *Postgres) UpdateMembershipState(ctx context.Context, ownerAccountID string, regionID, instanceID int64, nodeRole, status string, expiresAt *time.Time, ownershipVerifiedAt *time.Time) (RegionMembership, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RegionMembership{}, fmt.Errorf("store: begin update membership: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var region RegionInfo
	if err := tx.QueryRow(ctx, `
		SELECT id, slug, name, owner_account_id, status, region_revision, created_at, updated_at
		FROM trusted_regions WHERE id = $1 AND owner_account_id = $2`,
		regionID, ownerAccountID).
		Scan(&region.ID, &region.Slug, &region.Name, &region.OwnerAccountID, &region.Status, &region.RegionRevision, &region.CreatedAt, &region.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return RegionMembership{}, ErrNotFound
	} else if err != nil {
		return RegionMembership{}, fmt.Errorf("store: lock region for membership update: %w", err)
	}

	var out RegionMembership
	err = tx.QueryRow(ctx, `
		UPDATE trusted_region_memberships
		SET node_role = $4,
		    status = $5,
		    expires_at = $6,
		    ownership_verified_at = COALESCE($7, ownership_verified_at),
		    membership_revision = membership_revision + 1,
		    updated_at = now()
		WHERE region_id = $1 AND instance_id = $2
		RETURNING instance_id, region_id, node_role, status, admitted_by_account_id, admission_reason,
		          expires_at, ownership_verified_at, membership_revision, created_at, updated_at`,
		regionID, instanceID, ownerAccountID, nodeRole, status, expiresAt, ownershipVerifiedAt).
		Scan(&out.InstanceID, &out.RegionID, &out.NodeRole, &out.Status, &out.AdmittedByAccountID, &out.AdmissionReason,
			&out.ExpiresAt, &out.OwnershipVerifiedAt, &out.MembershipRevision, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegionMembership{}, ErrNotFound
	}
	if err != nil {
		return RegionMembership{}, fmt.Errorf("store: update membership: %w", err)
	}
	out.RegionSlug = region.Slug
	out.RegionStatus = region.Status
	if _, err := tx.Exec(ctx, `
		UPDATE instances SET policy_revision = policy_revision + 1, updated_at = now() WHERE id = $1`, instanceID); err != nil {
		return RegionMembership{}, fmt.Errorf("store: bump instance after membership update: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"node_role": out.NodeRole, "status": out.Status})
	if _, err := tx.Exec(ctx, `
		INSERT INTO trusted_region_membership_events
		    (instance_id, region_id, event_kind, actor_account_id, membership_revision, payload)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		instanceID, regionID, membershipEventKind(status), ownerAccountID, out.MembershipRevision, payload); err != nil {
		return RegionMembership{}, fmt.Errorf("store: record membership update event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegionMembership{}, fmt.Errorf("store: commit membership update: %w", err)
	}
	return out, nil
}

func membershipEventKind(status string) string {
	switch status {
	case "suspended":
		return "suspended"
	case "revoked":
		return "revoked"
	default:
		return "reactivated"
	}
}

func (p *Postgres) ReleaseMembership(ctx context.Context, actorAccountID string, instanceID int64) (bool, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: begin release membership: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var regionID int64
	if err := tx.QueryRow(ctx, `
		SELECT region_id
		FROM trusted_region_memberships trm
		JOIN instances i ON i.id = trm.instance_id
		WHERE trm.instance_id = $1 AND i.account_id = $2`, instanceID, actorAccountID).
		Scan(&regionID); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("store: lock membership for release: %w", err)
	}

	var activeTrusted int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM instance_services
		WHERE instance_id = $1 AND exposure = 'trusted_region'`, instanceID).
		Scan(&activeTrusted); err != nil {
		return false, fmt.Errorf("store: count trusted bindings: %w", err)
	}
	if activeTrusted > 0 {
		return false, ErrConflict
	}

	var membershipRevision int64
	if err := tx.QueryRow(ctx, `SELECT membership_revision FROM trusted_region_memberships WHERE instance_id = $1`, instanceID).Scan(&membershipRevision); err != nil {
		return false, fmt.Errorf("store: membership revision before release: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM trusted_region_memberships WHERE instance_id = $1`, instanceID)
	if err != nil {
		return false, fmt.Errorf("store: delete membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE instances SET policy_revision = policy_revision + 1, updated_at = now() WHERE id = $1`, instanceID); err != nil {
		return false, fmt.Errorf("store: bump instance on release: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"released": true})
	if _, err := tx.Exec(ctx, `
		INSERT INTO trusted_region_membership_events
		    (instance_id, region_id, event_kind, actor_account_id, membership_revision, payload)
		VALUES ($1, $2, 'released', $3, $4, $5)`,
		instanceID, regionID, actorAccountID, membershipRevision, payload); err != nil {
		return false, fmt.Errorf("store: record release event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit release membership: %w", err)
	}
	return true, nil
}

func (p *Postgres) GetInstanceServicesForUser(ctx context.Context, accountID string, instanceID int64) (InstanceInfo, error) {
	inst, err := p.GetInstanceByIDForUser(ctx, accountID, instanceID)
	if err != nil {
		return InstanceInfo{}, err
	}
	return inst, nil
}

func (p *Postgres) ReplaceInstanceServicePolicy(ctx context.Context, accountID string, instanceID int64, in ReplaceServicePolicyInput) (InstanceInfo, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: begin replace service policy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentScope string
	err = tx.QueryRow(ctx, `SELECT policy_scope FROM instances WHERE id = $1 AND account_id = $2 FOR UPDATE`, instanceID, accountID).Scan(&currentScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceInfo{}, ErrNotFound
	}
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: lock instance service policy: %w", err)
	}

	if in.PolicyScope == PolicyScopeService {
		if !in.Inventory.SupportsPolicyV2 {
			return InstanceInfo{}, ErrConflict
		}
		for _, observed := range in.Inventory.Services {
			if observed.Count > 1 {
				return InstanceInfo{}, ErrConflict
			}
		}
	}
	if currentScope == PolicyScopeService && in.PolicyScope == PolicyScopePeer {
		if !in.AcknowledgeScopeReset {
			return InstanceInfo{}, ErrConflict
		}
		var trustedCount int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM instance_services WHERE instance_id = $1 AND exposure = 'trusted_region'`, instanceID).Scan(&trustedCount); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: count trusted service bindings: %w", err)
		}
		if trustedCount > 0 {
			return InstanceInfo{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM instance_service_acl_rules WHERE instance_service_id IN (SELECT id FROM instance_services WHERE instance_id = $1)`, instanceID); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: delete service acl rules on peer reset: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM instance_services WHERE instance_id = $1`, instanceID); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: delete service policies on peer reset: %w", err)
		}
	}

	if in.PolicyScope == PolicyScopeService {
		if _, err := tx.Exec(ctx, `DELETE FROM instance_service_acl_rules WHERE instance_service_id IN (SELECT id FROM instance_services WHERE instance_id = $1)`, instanceID); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: delete service acl rules: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM instance_services WHERE instance_id = $1`, instanceID); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: delete service policies: %w", err)
		}
		for _, svc := range in.Services {
			var regionID any
			if svc.RegionID != nil {
				regionID = *svc.RegionID
			}
			var serviceID int64
			var revision int64
			var observedAt *time.Time
			observedPresent := false
			for _, observed := range in.Inventory.Services {
				if observed.Name == svc.ServiceName {
					observedPresent = observed.Count == 1
					ts := in.Inventory.ObservedAt.UTC()
					observedAt = &ts
					break
				}
			}
			err := tx.QueryRow(ctx, `
				INSERT INTO instance_services
				    (instance_id, service_name, exposure, region_id, access_mode, observed_present, observed_last_seen_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				RETURNING id, service_policy_revision`,
				instanceID, svc.ServiceName, svc.Exposure, regionID, svc.AccessMode, observedPresent, observedAt).
				Scan(&serviceID, &revision)
			if err != nil {
				if isUniqueViolation(err) || isForeignKeyViolation(err) {
					return InstanceInfo{}, ErrConflict
				}
				return InstanceInfo{}, fmt.Errorf("store: insert service policy: %w", err)
			}
			for _, rule := range svc.Rules {
				if _, err := tx.Exec(ctx, `
					INSERT INTO instance_service_acl_rules (instance_service_id, rule_kind, rule_value)
					VALUES ($1, $2, $3)`, serviceID, rule.Kind, rule.Value); err != nil {
					return InstanceInfo{}, fmt.Errorf("store: insert service acl rule: %w", err)
				}
			}
			svc.ID = serviceID
			svc.ServicePolicyRevision = revision
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE instances
		SET policy_scope = $3,
		    policy_revision = policy_revision + 1,
		    updated_at = now()
		WHERE id = $1 AND account_id = $2`, instanceID, accountID, in.PolicyScope); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: update instance policy scope: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: commit service policy replacement: %w", err)
	}
	return p.GetInstanceServicesForUser(ctx, accountID, instanceID)
}

func (p *Postgres) ReplaceInstanceServiceACL(ctx context.Context, accountID string, instanceID, serviceID int64, accessMode string, rules []ACLRule) (InstanceService, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return InstanceService{}, fmt.Errorf("store: begin replace service acl: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out InstanceService
	var regionID *int64
	err = tx.QueryRow(ctx, `
		UPDATE instance_services svc
		SET access_mode = $4,
		    service_policy_revision = service_policy_revision + 1,
		    updated_at = now()
		FROM instances i
		WHERE svc.id = $1 AND svc.instance_id = $2 AND i.id = svc.instance_id AND i.account_id = $3
		RETURNING svc.id, svc.instance_id, svc.service_name, svc.exposure, svc.region_id, svc.access_mode,
		          svc.service_policy_revision, svc.observed_present, svc.observed_last_seen_at, svc.created_at, svc.updated_at`,
		serviceID, instanceID, accountID, accessMode).
		Scan(&out.ID, &out.InstanceID, &out.ServiceName, &out.Exposure, &regionID, &out.AccessMode,
			&out.ServicePolicyRevision, &out.ObservedPresent, &out.ObservedLastSeenAt, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceService{}, ErrNotFound
	}
	if err != nil {
		return InstanceService{}, fmt.Errorf("store: update service acl: %w", err)
	}
	out.RegionID = regionID
	if _, err := tx.Exec(ctx, `DELETE FROM instance_service_acl_rules WHERE instance_service_id = $1`, serviceID); err != nil {
		return InstanceService{}, fmt.Errorf("store: clear service acl rules: %w", err)
	}
	for _, rule := range rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO instance_service_acl_rules (instance_service_id, rule_kind, rule_value)
			VALUES ($1, $2, $3)`, serviceID, rule.Kind, rule.Value); err != nil {
			return InstanceService{}, fmt.Errorf("store: insert service acl rule: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return InstanceService{}, fmt.Errorf("store: commit service acl replacement: %w", err)
	}
	out.Rules = append([]ACLRule(nil), rules...)
	return out, nil
}

func (p *Postgres) CreateNodeCredentialChallenge(ctx context.Context, ch NodeCredentialChallenge) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin create node credential challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, ch.PeerID); err != nil {
		return fmt.Errorf("store: lock node credential challenges: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM node_credential_challenges
		WHERE peer_id = $1 AND (consumed_at IS NOT NULL OR expires_at <= $2)`, ch.PeerID, ch.IssuedAt.UTC()); err != nil {
		return fmt.Errorf("store: prune node credential challenges: %w", err)
	}
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM node_credential_challenges
		WHERE peer_id = $1 AND consumed_at IS NULL AND expires_at > $2`, ch.PeerID, ch.IssuedAt.UTC()).Scan(&pending); err != nil {
		return fmt.Errorf("store: count node credential challenges: %w", err)
	}
	if pending >= 5 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO node_credential_challenges
		    (id, peer_id, region_slug, node_role, audience, nonce_hash, challenge_message, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ch.ID, ch.PeerID, ch.RegionSlug, ch.NodeRole, audFor(ch.Audience), ch.NonceHash, ch.ChallengeMessage, ch.IssuedAt.UTC(), ch.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("store: create node credential challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit node credential challenge: %w", err)
	}
	return nil
}

// audFor returns the audience to persist, defaulting to the ACL audience
// for rows written by callers that have not been updated.
func audFor(a string) string {
	if a == "" {
		return "api.opentela.ai/internal/acl"
	}
	return a
}

func (p *Postgres) ConsumeNodeCredentialChallenge(ctx context.Context, id, peerID, nonce, audience string, now time.Time) (NodeCredentialChallenge, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return NodeCredentialChallenge{}, fmt.Errorf("store: begin consume node challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out NodeCredentialChallenge
	err = tx.QueryRow(ctx, `
		SELECT id, peer_id, region_slug, node_role, audience, nonce_hash, challenge_message, issued_at, expires_at, consumed_at
		FROM node_credential_challenges
		WHERE id = $1 AND peer_id = $2 AND audience = $3
		FOR UPDATE`, id, peerID, audFor(audience)).
		Scan(&out.ID, &out.PeerID, &out.RegionSlug, &out.NodeRole, &out.Audience, &out.NonceHash, &out.ChallengeMessage, &out.IssuedAt, &out.ExpiresAt, &out.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeCredentialChallenge{}, ErrNotFound
	}
	if err != nil {
		return NodeCredentialChallenge{}, fmt.Errorf("store: lock node challenge: %w", err)
	}
	if out.ConsumedAt != nil {
		return NodeCredentialChallenge{}, ErrChallengeConsumed
	}
	if hashSecret(nonce) != out.NonceHash {
		return NodeCredentialChallenge{}, ErrNotFound
	}
	if now.UTC().After(out.ExpiresAt.UTC()) {
		return NodeCredentialChallenge{}, ErrChallengeExpired
	}
	if _, err := tx.Exec(ctx, `
		UPDATE node_credential_challenges SET consumed_at = $2 WHERE id = $1`, id, now.UTC()); err != nil {
		return NodeCredentialChallenge{}, fmt.Errorf("store: consume node challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return NodeCredentialChallenge{}, fmt.Errorf("store: commit node challenge: %w", err)
	}
	return out, nil
}

func (p *Postgres) CancelInvitationForOwner(ctx context.Context, ownerAccountID string, regionID, instanceID int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE trusted_region_invitations tri
		SET status = 'cancelled', cancelled_at = now(), updated_at = now()
		FROM trusted_regions tr
		WHERE tri.region_id = $1 AND tri.instance_id = $2 AND tri.status = 'pending'
		  AND tr.id = tri.region_id AND tr.owner_account_id = $3`, regionID, instanceID, ownerAccountID)
	if err != nil {
		return false, fmt.Errorf("store: cancel invitation: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) enrichInstances(ctx context.Context, instances []InstanceInfo) error {
	if len(instances) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(instances))
	index := make(map[int64]int, len(instances))
	for i := range instances {
		ids = append(ids, instances[i].ID)
		index[instances[i].ID] = i
	}

	memberships, err := p.listMemberships(ctx, `trm.instance_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	for _, membership := range memberships {
		if i, ok := index[membership.InstanceID]; ok {
			m := membership
			instances[i].Membership = &m
		}
	}

	services, err := p.listInstanceServices(ctx, ids)
	if err != nil {
		return err
	}
	for instanceID, list := range services {
		if i, ok := index[instanceID]; ok {
			instances[i].Services = list
		}
	}
	return nil
}

func (p *Postgres) enrichInstance(ctx context.Context, inst *InstanceInfo) error {
	if inst == nil {
		return nil
	}
	items := []InstanceInfo{*inst}
	if err := p.enrichInstances(ctx, items); err != nil {
		return err
	}
	*inst = items[0]
	return nil
}

func (p *Postgres) listMemberships(ctx context.Context, where string, arg any) ([]RegionMembership, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT trm.instance_id, i.peer_id, i.label, trm.region_id, tr.slug, tr.status, tr.region_revision, trm.node_role, trm.status,
		       trm.admitted_by_account_id, trm.admission_reason, trm.expires_at, trm.ownership_verified_at,
		       trm.membership_revision,
		       (SELECT count(*) FROM instance_services svc WHERE svc.instance_id = trm.instance_id AND svc.exposure = 'trusted_region'),
		       trm.created_at, trm.updated_at
		FROM trusted_region_memberships trm
		JOIN trusted_regions tr ON tr.id = trm.region_id
		JOIN instances i ON i.id = trm.instance_id
		WHERE `+where+`
		ORDER BY trm.created_at DESC, trm.instance_id DESC`, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list memberships: %w", err)
	}
	defer rows.Close()
	var out []RegionMembership
	for rows.Next() {
		var item RegionMembership
		if err := rows.Scan(&item.InstanceID, &item.PeerID, &item.Label, &item.RegionID, &item.RegionSlug, &item.RegionStatus, &item.RegionRevision, &item.NodeRole, &item.Status,
			&item.AdmittedByAccountID, &item.AdmissionReason, &item.ExpiresAt, &item.OwnershipVerifiedAt,
			&item.MembershipRevision, &item.TrustedServiceCount, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan membership: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: membership rows: %w", err)
	}
	return out, nil
}

func (p *Postgres) listInvitations(ctx context.Context, where string, arg any) ([]RegionInvitation, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT tri.id, tri.region_id, tr.slug, tri.instance_id, i.peer_id, i.label, tri.created_by_account_id, tri.acceptance_token_hash,
		       tri.status, tri.node_role, tri.expires_at, tri.accepted_at, tri.cancelled_at, tri.created_at, tri.updated_at
		FROM trusted_region_invitations tri
		JOIN trusted_regions tr ON tr.id = tri.region_id
		JOIN instances i ON i.id = tri.instance_id
		WHERE `+where+`
		ORDER BY tri.created_at DESC, tri.id DESC`, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list invitations: %w", err)
	}
	defer rows.Close()
	var out []RegionInvitation
	for rows.Next() {
		var item RegionInvitation
		if err := rows.Scan(&item.ID, &item.RegionID, &item.RegionSlug, &item.InstanceID, &item.PeerID, &item.Label, &item.CreatedByAccountID, &item.AcceptanceTokenHash,
			&item.Status, &item.NodeRole, &item.ExpiresAt, &item.AcceptedAt, &item.CancelledAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan invitation: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: invitation rows: %w", err)
	}
	return out, nil
}

func (p *Postgres) listInstanceServices(ctx context.Context, instanceIDs []int64) (map[int64][]InstanceService, error) {
	if len(instanceIDs) == 0 {
		return map[int64][]InstanceService{}, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT svc.id, svc.instance_id, svc.service_name, svc.exposure, svc.region_id, tr.slug, svc.access_mode,
		       svc.service_policy_revision, svc.observed_present, svc.observed_last_seen_at, svc.created_at, svc.updated_at
		FROM instance_services svc
		LEFT JOIN trusted_regions tr ON tr.id = svc.region_id
		WHERE svc.instance_id = ANY($1)
		ORDER BY svc.instance_id, svc.service_name`, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list instance services: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]InstanceService, len(instanceIDs))
	serviceIDs := make([]int64, 0)
	for rows.Next() {
		var item InstanceService
		var regionID *int64
		var regionSlug *string
		if err := rows.Scan(&item.ID, &item.InstanceID, &item.ServiceName, &item.Exposure, &regionID, &regionSlug, &item.AccessMode,
			&item.ServicePolicyRevision, &item.ObservedPresent, &item.ObservedLastSeenAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan instance service: %w", err)
		}
		item.RegionID = regionID
		item.RegionSlug = regionSlug
		out[item.InstanceID] = append(out[item.InstanceID], item)
		serviceIDs = append(serviceIDs, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: instance service rows: %w", err)
	}
	rulesByService, err := p.listServiceRulesByServiceIDs(ctx, serviceIDs)
	if err != nil {
		return nil, err
	}
	for instanceID := range out {
		list := out[instanceID]
		for i := range list {
			list[i].Rules = rulesByService[list[i].ID]
		}
		out[instanceID] = list
	}
	return out, nil
}

func (p *Postgres) listServiceRulesByServiceIDs(ctx context.Context, serviceIDs []int64) (map[int64][]ACLRule, error) {
	if len(serviceIDs) == 0 {
		return map[int64][]ACLRule{}, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT instance_service_id, rule_kind, rule_value
		FROM instance_service_acl_rules
		WHERE instance_service_id = ANY($1)
		ORDER BY instance_service_id, rule_kind, rule_value`, serviceIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list service acl rules: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]ACLRule, len(serviceIDs))
	for rows.Next() {
		var serviceID int64
		var rule ACLRule
		if err := rows.Scan(&serviceID, &rule.Kind, &rule.Value); err != nil {
			return nil, fmt.Errorf("store: scan service acl rule: %w", err)
		}
		out[serviceID] = append(out[serviceID], rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: service acl rule rows: %w", err)
	}
	return out, nil
}
