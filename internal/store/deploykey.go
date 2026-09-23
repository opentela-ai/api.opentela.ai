package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeployKeyInfo is a row of the deploy_keys table (migration 0015). The
// plaintext token is never stored: KeyHash is the SHA-256 hex digest and
// KeyPrefix is the non-secret display prefix ("otd-" + first 8 hex chars).
type DeployKeyInfo struct {
	ID         int64
	UserID     string
	KeyHash    string
	KeyPrefix  string
	Name       string
	MaxUses    int
	UseCount   int
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	Active     bool
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// UsesLeft reports how many more successful links the key permits; -1 means
// unlimited (a nil ExpiresAt makes the key expiry-free, but uses are always
// finite: MaxUses >= 1 is a table constraint).
func (k DeployKeyInfo) UsesLeft() int {
	return k.MaxUses - k.UseCount
}

// Expired reports whether the key's TTL has passed at now.
func (k DeployKeyInfo) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.UTC().After(k.ExpiresAt.UTC())
}

// Usable reports whether the key may complete another link right now.
func (k DeployKeyInfo) Usable(now time.Time) bool {
	return k.Active && k.RevokedAt == nil && !k.Expired(now) && k.UsesLeft() > 0
}

const deployKeyColumns = `id, user_id, key_hash, key_prefix, name, max_uses, use_count,
	expires_at, last_used_at, active, created_at, revoked_at`

func scanDeployKey(row pgx.Row) (DeployKeyInfo, error) {
	var k DeployKeyInfo
	err := row.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name, &k.MaxUses,
		&k.UseCount, &k.ExpiresAt, &k.LastUsedAt, &k.Active, &k.CreatedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployKeyInfo{}, ErrNotFound
	}
	if err != nil {
		return DeployKeyInfo{}, fmt.Errorf("store: scan deploy key: %w", err)
	}
	return k, nil
}

// InsertDeployKey stores a freshly minted deploy key.
func (p *Postgres) InsertDeployKey(ctx context.Context, userID, keyHash, keyPrefix, name string, maxUses int, expiresAt *time.Time) (DeployKeyInfo, error) {
	k, err := scanDeployKey(p.pool.QueryRow(ctx, `
		INSERT INTO deploy_keys (user_id, key_hash, key_prefix, name, max_uses, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
		RETURNING `+deployKeyColumns,
		userID, keyHash, keyPrefix, name, maxUses, expiresAt))
	if err != nil {
		return DeployKeyInfo{}, fmt.Errorf("store: insert deploy key: %w", err)
	}
	return k, nil
}

// ListDeployKeysByUser returns the account's deploy keys, newest first.
// Revoked keys are included so the console can show their history.
func (p *Postgres) ListDeployKeysByUser(ctx context.Context, userID string) ([]DeployKeyInfo, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+deployKeyColumns+` FROM deploy_keys WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list deploy keys: %w", err)
	}
	defer rows.Close()
	var out []DeployKeyInfo
	for rows.Next() {
		var k DeployKeyInfo
		if err := rows.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Name, &k.MaxUses,
			&k.UseCount, &k.ExpiresAt, &k.LastUsedAt, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: scan deploy key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deploy keys: %w", err)
	}
	return out, nil
}

// CountActiveDeployKeysByUser counts non-revoked keys for the per-user cap.
func (p *Postgres) CountActiveDeployKeysByUser(ctx context.Context, userID string) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM deploy_keys WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count deploy keys: %w", err)
	}
	return n, nil
}

// RevokeDeployKeyByIDForUser revokes one of the account's keys by id,
// returning the key hash (for cache purges, if any) and whether a row changed.
func (p *Postgres) RevokeDeployKeyByIDForUser(ctx context.Context, userID string, id int64) (string, bool, error) {
	var keyHash string
	tag, err := p.pool.Exec(ctx, `
		UPDATE deploy_keys SET revoked_at = now(), active = FALSE
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return "", false, fmt.Errorf("store: revoke deploy key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", false, nil
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT key_hash FROM deploy_keys WHERE id = $1`, id).Scan(&keyHash); err != nil {
		return "", false, fmt.Errorf("store: revoke deploy key hash: %w", err)
	}
	return keyHash, true, nil
}

// FindActiveDeployKey resolves a plaintext deploy key to its row, enforcing
// active + unrevoked + unexpired. Use consumption is separate
// (ConsumeDeployKeyUse) so challenge issuance stays read-only.
func (p *Postgres) FindActiveDeployKey(ctx context.Context, keyHash string, now time.Time) (DeployKeyInfo, error) {
	k, err := scanDeployKey(p.pool.QueryRow(ctx,
		`SELECT `+deployKeyColumns+` FROM deploy_keys WHERE key_hash = $1`, keyHash))
	if err != nil {
		return DeployKeyInfo{}, err
	}
	if !k.Active || k.RevokedAt != nil || k.Expired(now) {
		return DeployKeyInfo{}, ErrNotFound
	}
	return k, nil
}

// ConsumeDeployKeyUse atomically spends one use of the key, returning the
// owning account id. The conditional UPDATE makes over-concurrency safe: two
// simultaneous links racing for the last use cannot both win, and the loser
// observes ErrNotFound.
func (p *Postgres) ConsumeDeployKeyUse(ctx context.Context, keyHash string, now time.Time) (string, error) {
	var userID string
	err := p.pool.QueryRow(ctx, `
		UPDATE deploy_keys SET use_count = use_count + 1, last_used_at = $2
		WHERE key_hash = $1 AND active AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > $2)
		  AND use_count < max_uses
		RETURNING user_id`, keyHash, now.UTC()).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: consume deploy key use: %w", err)
	}
	return userID, nil
}

// CreateInstanceLinkChallenge stores a single-use libp2p signing challenge
// for the instance-link flow. It mirrors CreateNodeCredentialChallenge
// (same table, distinct ch.Audience) but scopes the pending-cap accounting
// to the link audience so the two flows cannot evict each other's
// challenges.
func (p *Postgres) CreateInstanceLinkChallenge(ctx context.Context, ch NodeCredentialChallenge) error {
	audience := ch.Audience
	if audience == "" {
		audience = "api.opentela.ai/internal/instances/link"
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin create link challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, ch.PeerID); err != nil {
		return fmt.Errorf("store: lock link challenges: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM node_credential_challenges
		WHERE peer_id = $1 AND audience = $2 AND (consumed_at IS NOT NULL OR expires_at <= $3)`,
		ch.PeerID, audience, ch.IssuedAt.UTC()); err != nil {
		return fmt.Errorf("store: prune link challenges: %w", err)
	}
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM node_credential_challenges
		WHERE peer_id = $1 AND audience = $2 AND consumed_at IS NULL AND expires_at > $3`,
		ch.PeerID, audience, ch.IssuedAt.UTC()).Scan(&pending); err != nil {
		return fmt.Errorf("store: count link challenges: %w", err)
	}
	if pending >= 5 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO node_credential_challenges
		    (id, peer_id, region_slug, node_role, audience, nonce_hash, challenge_message, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ch.ID, ch.PeerID, ch.RegionSlug, ch.NodeRole, audience, ch.NonceHash, ch.ChallengeMessage, ch.IssuedAt.UTC(), ch.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("store: create link challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit link challenge: %w", err)
	}
	return nil
}
