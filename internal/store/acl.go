package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound                = errors.New("store: not found")
	ErrConflict                = errors.New("store: conflict")
	ErrChallengeExpired        = errors.New("store: challenge expired")
	ErrChallengeConsumed       = errors.New("store: challenge consumed")
	ErrWalletInUse             = errors.New("store: wallet in use")
	ErrWalletOtherAccount      = errors.New("store: wallet linked to another account")
	ErrRegionMigrationConflict = errors.New("store: region migration blocked by existing bindings")
)

type IdentityInfo struct {
	AccountID      string
	Email          string
	EmailDomain    string
	EmailVerified  bool
	LastVerifiedAt time.Time
}

type WalletInfo struct {
	ID        int64
	AccountID string
	Wallet    string
	Primary   bool
	CreatedAt time.Time
}

type WalletChallenge struct {
	ID        string
	AccountID string
	Wallet    string
	Nonce     string
	Message   string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type ACLRule struct {
	Kind  string
	Value string
}

type InstanceInfo struct {
	ID                  int64
	AccountID           string
	PeerID              string
	Label               string
	OwnerWallet         string
	AccessMode          string
	PolicyScope         string
	PolicyRevision      int64
	OwnershipStatus     string
	ObservedWallet      *string
	OwnershipObservedAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Rules               []ACLRule
	Membership          *RegionMembership
	Services            []InstanceService
}

type RegionInfo struct {
	ID             int64
	Slug           string
	Name           string
	OwnerAccountID string
	Status         string
	RegionRevision int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type RegionMembership struct {
	InstanceID          int64
	PeerID              string
	Label               string
	RegionID            int64
	RegionSlug          string
	RegionStatus        string
	RegionRevision      int64
	NodeRole            string
	Status              string
	AdmittedByAccountID *string
	AdmissionReason     *string
	ExpiresAt           *time.Time
	OwnershipVerifiedAt *time.Time
	MembershipRevision  int64
	TrustedServiceCount int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type RegionInvitation struct {
	ID                  int64
	RegionID            int64
	RegionSlug          string
	InstanceID          int64
	PeerID              string
	Label               string
	CreatedByAccountID  string
	AcceptanceTokenHash string
	Status              string
	NodeRole            string
	ExpiresAt           time.Time
	AcceptedAt          *time.Time
	CancelledAt         *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type RegionMembershipEvent struct {
	ID                 int64
	InstanceID         int64
	RegionID           *int64
	EventKind          string
	ActorAccountID     *string
	MembershipRevision int64
	Payload            []byte
	CreatedAt          time.Time
}

type InstanceService struct {
	ID                    int64
	InstanceID            int64
	ServiceName           string
	Exposure              string
	RegionID              *int64
	RegionSlug            *string
	AccessMode            string
	ServicePolicyRevision int64
	ObservedPresent       bool
	ObservedLastSeenAt    *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Rules                 []ACLRule
}

type ServiceInventory struct {
	ObservedAt       time.Time
	SupportsPolicyV2 bool
	Services         []ObservedService
}

type ObservedService struct {
	Name  string
	Count int
}

type ReplaceServicePolicyInput struct {
	PolicyScope           string
	AcknowledgeScopeReset bool
	Inventory             ServiceInventory
	Services              []InstanceService
}

type NodeCredentialChallenge struct {
	ID               string
	PeerID           string
	RegionSlug       string
	NodeRole         string
	Audience         string
	NonceHash        string
	ChallengeMessage string
	IssuedAt         time.Time
	ExpiresAt        time.Time
	ConsumedAt       *time.Time
}

type ActiveKey struct {
	KeyID   int64
	UserID  *string
	KeyHash string
}

func (p *Postgres) RefreshIdentity(ctx context.Context, in IdentityInfo) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO account_identities
		    (account_id, email, email_domain, email_verified, last_verified_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (account_id) DO UPDATE SET
		    email = EXCLUDED.email,
		    email_domain = EXCLUDED.email_domain,
		    email_verified = EXCLUDED.email_verified,
		    last_verified_at = EXCLUDED.last_verified_at,
		    updated_at = now()`,
		in.AccountID, in.Email, in.EmailDomain, in.EmailVerified, in.LastVerifiedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: refresh identity: %w", err)
	}
	return nil
}

func (p *Postgres) GetIdentity(ctx context.Context, accountID string) (IdentityInfo, error) {
	var out IdentityInfo
	err := p.pool.QueryRow(ctx, `
		SELECT account_id, email, email_domain, email_verified, last_verified_at
		FROM account_identities WHERE account_id = $1`, accountID).
		Scan(&out.AccountID, &out.Email, &out.EmailDomain, &out.EmailVerified, &out.LastVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityInfo{}, ErrNotFound
	}
	if err != nil {
		return IdentityInfo{}, fmt.Errorf("store: get identity: %w", err)
	}
	return out, nil
}

func (p *Postgres) CreateWalletChallenge(ctx context.Context, ch WalletChallenge) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO wallet_challenges
		    (id, account_id, wallet, nonce, message, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ch.ID, ch.AccountID, ch.Wallet, ch.Nonce, ch.Message, ch.IssuedAt.UTC(), ch.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("store: create wallet challenge: %w", err)
	}
	return nil
}

func (p *Postgres) GetWalletChallenge(ctx context.Context, accountID, id string) (WalletChallenge, error) {
	var ch WalletChallenge
	err := p.pool.QueryRow(ctx, `
		SELECT id, account_id, wallet, nonce, message, issued_at, expires_at
		FROM wallet_challenges WHERE account_id = $1 AND id = $2`, accountID, id).
		Scan(&ch.ID, &ch.AccountID, &ch.Wallet, &ch.Nonce, &ch.Message, &ch.IssuedAt, &ch.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WalletChallenge{}, ErrNotFound
	}
	if err != nil {
		return WalletChallenge{}, fmt.Errorf("store: get wallet challenge: %w", err)
	}
	return ch, nil
}

func (p *Postgres) ConsumeWalletChallenge(ctx context.Context, accountID, id string, now time.Time) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin consume challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var expiresAt time.Time
	var usedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT expires_at, used_at
		FROM wallet_challenges
		WHERE account_id = $1 AND id = $2
		FOR UPDATE`, accountID, id).
		Scan(&expiresAt, &usedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock wallet challenge: %w", err)
	}
	if usedAt != nil {
		return ErrChallengeConsumed
	}
	if now.UTC().After(expiresAt.UTC()) {
		return ErrChallengeExpired
	}
	if _, err := tx.Exec(ctx, `
		UPDATE wallet_challenges SET used_at = $3
		WHERE account_id = $1 AND id = $2`, accountID, id, now.UTC()); err != nil {
		return fmt.Errorf("store: consume wallet challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit wallet challenge: %w", err)
	}
	return nil
}

func (p *Postgres) LinkWallet(ctx context.Context, accountID, wallet string) (WalletInfo, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return WalletInfo{}, fmt.Errorf("store: begin link wallet: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, accountID); err != nil {
		return WalletInfo{}, fmt.Errorf("store: lock wallet account: %w", err)
	}

	var primary bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM user_wallets WHERE account_id = $1 AND is_primary = TRUE)`, accountID).
		Scan(&primary)
	if err != nil {
		return WalletInfo{}, fmt.Errorf("store: wallet primary existence: %w", err)
	}
	shouldPrimary := !primary

	var out WalletInfo
	err = tx.QueryRow(ctx, `
		INSERT INTO user_wallets (account_id, wallet, is_primary)
		VALUES ($1, $2, $3)
		ON CONFLICT (wallet) DO NOTHING
		RETURNING id, created_at`, accountID, wallet, shouldPrimary).
		Scan(&out.ID, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var owner string
		if qErr := tx.QueryRow(ctx, `SELECT account_id FROM user_wallets WHERE wallet = $1`, wallet).Scan(&owner); qErr != nil {
			return WalletInfo{}, fmt.Errorf("store: resolve wallet conflict: %w", qErr)
		}
		if owner == accountID {
			return WalletInfo{}, ErrConflict
		}
		return WalletInfo{}, ErrWalletOtherAccount
	}
	if err != nil {
		return WalletInfo{}, fmt.Errorf("store: link wallet: %w", err)
	}
	out.AccountID = accountID
	out.Wallet = wallet
	out.Primary = shouldPrimary
	if err := tx.Commit(ctx); err != nil {
		return WalletInfo{}, fmt.Errorf("store: commit link wallet: %w", err)
	}
	return out, nil
}

// AccountForWallet resolves the owning account for a wallet pubkey. The
// user_wallets.wallet column is UNIQUE (enforced by LinkWallet's
// ON CONFLICT), so at most one account owns a given wallet. ok is false when
// the wallet is not linked; the deposit watcher leaves such transfers
// 'unassigned' for later reconciliation (ReconcileDepositsForWallet).
func (p *Postgres) AccountForWallet(ctx context.Context, wallet string) (string, bool, error) {
	if wallet == "" {
		return "", false, nil
	}
	var accountID string
	err := p.pool.QueryRow(ctx, `SELECT account_id FROM user_wallets WHERE wallet = $1`, wallet).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: account for wallet: %w", err)
	}
	return accountID, true, nil
}

// PrimaryWalletForAccount returns the account's primary linked wallet.
// ok is false when no primary wallet is linked.
func (p *Postgres) PrimaryWalletForAccount(ctx context.Context, accountID string) (string, bool, error) {
	if accountID == "" {
		return "", false, nil
	}
	var wallet string
	err := p.pool.QueryRow(ctx, `
		SELECT wallet
		FROM user_wallets
		WHERE account_id = $1 AND is_primary = TRUE
		LIMIT 1`, accountID).Scan(&wallet)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: primary wallet for account: %w", err)
	}
	return wallet, true, nil
}

func (p *Postgres) ListWalletsByUser(ctx context.Context, accountID string) ([]WalletInfo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, account_id, wallet, is_primary, created_at
		FROM user_wallets
		WHERE account_id = $1
		ORDER BY is_primary DESC, created_at ASC, id ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list wallets: %w", err)
	}
	defer rows.Close()

	var out []WalletInfo
	for rows.Next() {
		var w WalletInfo
		if err := rows.Scan(&w.ID, &w.AccountID, &w.Wallet, &w.Primary, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list wallets scan: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list wallets rows: %w", err)
	}
	return out, nil
}

func (p *Postgres) DeleteWalletByIDForUser(ctx context.Context, accountID string, id int64) (bool, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: begin delete wallet: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, accountID); err != nil {
		return false, fmt.Errorf("store: lock wallet account: %w", err)
	}

	var wallet string
	var wasPrimary bool
	err = tx.QueryRow(ctx, `
		SELECT wallet, is_primary FROM user_wallets
		WHERE id = $1 AND account_id = $2
		FOR UPDATE`, id, accountID).Scan(&wallet, &wasPrimary)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: lookup wallet delete: %w", err)
	}

	var inUse bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		    SELECT 1 FROM instances
		    WHERE account_id = $1 AND owner_wallet = $2
		)`, accountID, wallet).Scan(&inUse); err != nil {
		return false, fmt.Errorf("store: wallet delete in-use check: %w", err)
	}
	if inUse {
		return false, ErrWalletInUse
	}

	tag, err := tx.Exec(ctx, `DELETE FROM user_wallets WHERE id = $1 AND account_id = $2`, id, accountID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return false, ErrWalletInUse
		}
		return false, fmt.Errorf("store: delete wallet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if wasPrimary {
		if _, err := tx.Exec(ctx, `
			UPDATE user_wallets SET is_primary = TRUE
			WHERE id = (
				SELECT id FROM user_wallets
				WHERE account_id = $1
				ORDER BY created_at ASC, id ASC
				LIMIT 1
			)`, accountID); err != nil {
			return false, fmt.Errorf("store: promote primary wallet: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit delete wallet: %w", err)
	}
	return true, nil
}

func (p *Postgres) GetUserWalletSet(ctx context.Context, accountID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT wallet FROM user_wallets WHERE account_id = $1 ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list wallet set: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var wallet string
		if err := rows.Scan(&wallet); err != nil {
			return nil, fmt.Errorf("store: scan wallet set: %w", err)
		}
		out = append(out, wallet)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rows wallet set: %w", err)
	}
	return out, nil
}

func (p *Postgres) CreateInstance(ctx context.Context, in InstanceInfo) (InstanceInfo, error) {
	var out InstanceInfo
	err := p.pool.QueryRow(ctx, `
		INSERT INTO instances
		    (account_id, peer_id, label, owner_wallet, access_mode, policy_scope, ownership_status, observed_wallet, ownership_observed_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE(NULLIF($6, ''), 'peer'), $7, $8, $9)
		RETURNING id, policy_scope, policy_revision, created_at, updated_at`,
		in.AccountID, in.PeerID, in.Label, in.OwnerWallet, in.AccessMode, in.PolicyScope, in.OwnershipStatus, in.ObservedWallet, in.OwnershipObservedAt).
		Scan(&out.ID, &out.PolicyScope, &out.PolicyRevision, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return InstanceInfo{}, ErrConflict
		}
		return InstanceInfo{}, fmt.Errorf("store: create instance: %w", err)
	}
	out.AccountID = in.AccountID
	out.PeerID = in.PeerID
	out.Label = in.Label
	out.OwnerWallet = in.OwnerWallet
	out.AccessMode = in.AccessMode
	if out.PolicyScope == "" {
		out.PolicyScope = PolicyScopePeer
	}
	out.OwnershipStatus = in.OwnershipStatus
	out.ObservedWallet = in.ObservedWallet
	out.OwnershipObservedAt = in.OwnershipObservedAt
	return out, nil
}

func (p *Postgres) GetInstanceByIDForUser(ctx context.Context, accountID string, id int64) (InstanceInfo, error) {
	out, err := p.getInstance(ctx, `SELECT id, account_id, peer_id, label, owner_wallet, access_mode,
		policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at, created_at, updated_at
		FROM instances WHERE id = $1 AND account_id = $2`, id, accountID)
	if err != nil {
		return InstanceInfo{}, err
	}
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	rules, err := p.ListInstanceRules(ctx, out.ID)
	if err != nil {
		return InstanceInfo{}, err
	}
	out.Rules = rules
	return out, nil
}

func (p *Postgres) GetInstanceByID(ctx context.Context, id int64) (InstanceInfo, error) {
	out, err := p.getInstance(ctx, `SELECT id, account_id, peer_id, label, owner_wallet, access_mode,
		policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at, created_at, updated_at
		FROM instances WHERE id = $1`, id)
	if err != nil {
		return InstanceInfo{}, err
	}
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	rules, err := p.ListInstanceRules(ctx, out.ID)
	if err != nil {
		return InstanceInfo{}, err
	}
	out.Rules = rules
	return out, nil
}

func (p *Postgres) GetInstanceByPeerID(ctx context.Context, peerID string) (InstanceInfo, error) {
	out, err := p.getInstance(ctx, `SELECT id, account_id, peer_id, label, owner_wallet, access_mode,
		policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at, created_at, updated_at
		FROM instances WHERE peer_id = $1`, peerID)
	if err != nil {
		return InstanceInfo{}, err
	}
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	rules, err := p.ListInstanceRules(ctx, out.ID)
	if err != nil {
		return InstanceInfo{}, err
	}
	out.Rules = rules
	return out, nil
}

func (p *Postgres) ListInstancesByUser(ctx context.Context, accountID string) ([]InstanceInfo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, account_id, peer_id, label, owner_wallet, access_mode,
		       policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at,
		       created_at, updated_at
		FROM instances
		WHERE account_id = $1
		ORDER BY created_at DESC, id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list instances: %w", err)
	}
	defer rows.Close()

	var out []InstanceInfo
	var ids []int64
	for rows.Next() {
		var item InstanceInfo
		if err := rows.Scan(&item.ID, &item.AccountID, &item.PeerID, &item.Label, &item.OwnerWallet, &item.AccessMode,
			&item.PolicyScope, &item.PolicyRevision, &item.OwnershipStatus, &item.ObservedWallet, &item.OwnershipObservedAt,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan instances: %w", err)
		}
		out = append(out, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list instances rows: %w", err)
	}
	if len(ids) == 0 {
		return out, nil
	}
	if err := p.enrichInstances(ctx, out); err != nil {
		return nil, err
	}
	rulesByID, err := p.listRulesByInstanceIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Rules = rulesByID[out[i].ID]
	}
	return out, nil
}

func (p *Postgres) UpdateInstanceMetadata(ctx context.Context, accountID string, id int64, label, mode, ownershipStatus string, observedWallet *string, observedAt *time.Time) (InstanceInfo, error) {
	var out InstanceInfo
	err := p.pool.QueryRow(ctx, `
		UPDATE instances
		SET label = $3,
		    access_mode = $4,
		    ownership_status = $5,
		    observed_wallet = $6,
		    ownership_observed_at = $7,
		    updated_at = now()
		WHERE id = $1 AND account_id = $2
		RETURNING id, account_id, peer_id, label, owner_wallet, access_mode,
		          policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at,
		          created_at, updated_at`,
		id, accountID, label, mode, ownershipStatus, observedWallet, observedAt).
		Scan(&out.ID, &out.AccountID, &out.PeerID, &out.Label, &out.OwnerWallet, &out.AccessMode,
			&out.PolicyScope, &out.PolicyRevision, &out.OwnershipStatus, &out.ObservedWallet, &out.OwnershipObservedAt,
			&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceInfo{}, ErrNotFound
	}
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: update instance metadata: %w", err)
	}
	out.Rules, err = p.ListInstanceRules(ctx, out.ID)
	if err != nil {
		return InstanceInfo{}, err
	}
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	return out, nil
}

func (p *Postgres) ReplaceInstanceACL(ctx context.Context, accountID string, id int64, mode, ownershipStatus string, observedWallet *string, observedAt *time.Time, rules []ACLRule) (InstanceInfo, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: begin replace acl: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out InstanceInfo
	err = tx.QueryRow(ctx, `
		UPDATE instances
		SET access_mode = $3,
		    ownership_status = $4,
		    observed_wallet = $5,
		    ownership_observed_at = $6,
		    policy_revision = policy_revision + 1,
		    updated_at = now()
		WHERE id = $1 AND account_id = $2
		RETURNING id, account_id, peer_id, label, owner_wallet, access_mode,
		          policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at,
		          created_at, updated_at`,
		id, accountID, mode, ownershipStatus, observedWallet, observedAt).
		Scan(&out.ID, &out.AccountID, &out.PeerID, &out.Label, &out.OwnerWallet, &out.AccessMode,
			&out.PolicyScope, &out.PolicyRevision, &out.OwnershipStatus, &out.ObservedWallet, &out.OwnershipObservedAt,
			&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceInfo{}, ErrNotFound
	}
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: update instance acl: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM instance_acl_rules WHERE instance_id = $1`, id); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: delete instance rules: %w", err)
	}
	if len(rules) > 0 {
		batch := &pgx.Batch{}
		for _, rule := range rules {
			batch.Queue(`INSERT INTO instance_acl_rules (instance_id, rule_kind, rule_value) VALUES ($1, $2, $3)`,
				id, rule.Kind, rule.Value)
		}
		br := tx.SendBatch(ctx, batch)
		for range rules {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return InstanceInfo{}, fmt.Errorf("store: insert instance rules: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return InstanceInfo{}, fmt.Errorf("store: close instance rule batch: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: commit replace acl: %w", err)
	}
	out.Rules = slices.Clone(rules)
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	return out, nil
}

func (p *Postgres) ReclaimInstance(ctx context.Context, id int64, accountID, ownerWallet string, observedWallet *string, observedAt *time.Time) (InstanceInfo, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: begin reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM instance_acl_rules WHERE instance_id = $1`, id); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: reclaim delete rules: %w", err)
	}

	var out InstanceInfo
	err = tx.QueryRow(ctx, `
		UPDATE instances
		SET account_id = $2,
		    owner_wallet = $3,
		    access_mode = 'restricted',
		    policy_scope = 'peer',
		    ownership_status = 'active',
		    observed_wallet = $4,
		    ownership_observed_at = $5,
		    policy_revision = policy_revision + 1,
		    updated_at = now()
		WHERE id = $1
		RETURNING id, account_id, peer_id, label, owner_wallet, access_mode,
		          policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at,
		          created_at, updated_at`,
		id, accountID, ownerWallet, observedWallet, observedAt).
		Scan(&out.ID, &out.AccountID, &out.PeerID, &out.Label, &out.OwnerWallet, &out.AccessMode,
			&out.PolicyScope, &out.PolicyRevision, &out.OwnershipStatus, &out.ObservedWallet, &out.OwnershipObservedAt,
			&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceInfo{}, ErrNotFound
	}
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: reclaim instance: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InstanceInfo{}, fmt.Errorf("store: commit reclaim: %w", err)
	}
	if err := p.enrichInstance(ctx, &out); err != nil {
		return InstanceInfo{}, err
	}
	return out, nil
}

func (p *Postgres) DeleteInstanceByIDForUser(ctx context.Context, accountID string, id int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM instances WHERE id = $1 AND account_id = $2`, id, accountID)
	if err != nil {
		return false, fmt.Errorf("store: delete instance: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) ListInstanceRules(ctx context.Context, instanceID int64) ([]ACLRule, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT rule_kind, rule_value
		FROM instance_acl_rules
		WHERE instance_id = $1
		ORDER BY rule_kind, rule_value`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("store: list instance rules: %w", err)
	}
	defer rows.Close()
	var out []ACLRule
	for rows.Next() {
		var rule ACLRule
		if err := rows.Scan(&rule.Kind, &rule.Value); err != nil {
			return nil, fmt.Errorf("store: list instance rules scan: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list instance rules rows: %w", err)
	}
	return out, nil
}

func (p *Postgres) LookupActiveKey(ctx context.Context, keyHash string) (ActiveKey, error) {
	var out ActiveKey
	err := p.pool.QueryRow(ctx, `
		SELECT id, user_id, key_hash
		FROM api_keys
		WHERE key_hash = $1 AND active = TRUE`, keyHash).
		Scan(&out.KeyID, &out.UserID, &out.KeyHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActiveKey{}, ErrNotFound
	}
	if err != nil {
		return ActiveKey{}, fmt.Errorf("store: lookup active key: %w", err)
	}
	return out, nil
}

func (p *Postgres) ListManagedInstancesByPeerIDs(ctx context.Context, peerIDs []string) ([]InstanceInfo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, account_id, peer_id, label, owner_wallet, access_mode,
		       policy_scope, policy_revision, ownership_status, observed_wallet, ownership_observed_at,
		       created_at, updated_at
		FROM instances WHERE peer_id = ANY($1)`, peerIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list managed instances: %w", err)
	}
	defer rows.Close()
	var out []InstanceInfo
	var ids []int64
	for rows.Next() {
		var item InstanceInfo
		if err := rows.Scan(&item.ID, &item.AccountID, &item.PeerID, &item.Label, &item.OwnerWallet, &item.AccessMode,
			&item.PolicyScope, &item.PolicyRevision, &item.OwnershipStatus, &item.ObservedWallet, &item.OwnershipObservedAt,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan managed instance: %w", err)
		}
		out = append(out, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: managed instances rows: %w", err)
	}
	if err := p.enrichInstances(ctx, out); err != nil {
		return nil, err
	}
	rulesByID, err := p.listRulesByInstanceIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Rules = rulesByID[out[i].ID]
	}
	return out, nil
}

func (p *Postgres) getInstance(ctx context.Context, query string, args ...any) (InstanceInfo, error) {
	var out InstanceInfo
	err := p.pool.QueryRow(ctx, query, args...).
		Scan(&out.ID, &out.AccountID, &out.PeerID, &out.Label, &out.OwnerWallet, &out.AccessMode,
			&out.PolicyScope, &out.PolicyRevision, &out.OwnershipStatus, &out.ObservedWallet, &out.OwnershipObservedAt,
			&out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceInfo{}, ErrNotFound
	}
	if err != nil {
		return InstanceInfo{}, fmt.Errorf("store: get instance: %w", err)
	}
	return out, nil
}

func (p *Postgres) listRulesByInstanceIDs(ctx context.Context, ids []int64) (map[int64][]ACLRule, error) {
	if len(ids) == 0 {
		return map[int64][]ACLRule{}, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT instance_id, rule_kind, rule_value
		FROM instance_acl_rules
		WHERE instance_id = ANY($1)
		ORDER BY instance_id, rule_kind, rule_value`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list rule map: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]ACLRule, len(ids))
	for rows.Next() {
		var id int64
		var rule ACLRule
		if err := rows.Scan(&id, &rule.Kind, &rule.Value); err != nil {
			return nil, fmt.Errorf("store: scan rule map: %w", err)
		}
		out[id] = append(out[id], rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rule map rows: %w", err)
	}
	return out, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
