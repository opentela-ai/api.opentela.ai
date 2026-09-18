package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a pgxpool-backed KeyStore plus administrative operations used by keyctl.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a connection pool to dsn and verifies connectivity.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate executes an arbitrary DDL string (used to apply migration files).
func (p *Postgres) Migrate(ctx context.Context, ddl string) error {
	_, err := p.pool.Exec(ctx, ddl)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Validate reports whether an active key exists for the given hash and, when
// it does, the owning account id (api_keys.user_id). Legacy keys — created
// before user ownership — return an empty account id alongside a positive
// result, which lets the request path reject them with 402 billing_account_required
// in enforcement mode. Revoked or unknown keys return ("", false, nil).
func (p *Postgres) Validate(ctx context.Context, keyHash string) (string, bool, error) {
	var active bool
	var accountID *string
	err := p.pool.QueryRow(ctx,
		`SELECT active, user_id FROM api_keys WHERE key_hash = $1`, keyHash).Scan(&active, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: validate: %w", err)
	}
	if !active || accountID == nil {
		return "", active, nil
	}
	return *accountID, true, nil
}

// Insert adds a new active key. name may be empty.
func (p *Postgres) Insert(ctx context.Context, keyHash, name string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO api_keys (key_hash, name) VALUES ($1, $2)`, keyHash, name)
	if err != nil {
		return fmt.Errorf("store: insert: %w", err)
	}
	return nil
}

// Revoke marks a key inactive. It reports whether a row was changed.
func (p *Postgres) Revoke(ctx context.Context, keyHash string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = now()
		 WHERE key_hash = $1 AND active = TRUE`, keyHash)
	if err != nil {
		return false, fmt.Errorf("store: revoke: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// List returns all keys ordered by creation time.
func (p *Postgres) List(ctx context.Context) ([]KeyInfo, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key_hash, COALESCE(name, ''), active, created_at, revoked_at
		 FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list query: %w", err)
	}
	defer rows.Close()

	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.KeyHash, &k.Name, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rows: %w", err)
	}
	return out, nil
}

// InsertUserKey adds a new active key owned by userID and returns the stored row.
func (p *Postgres) InsertUserKey(ctx context.Context, userID, keyHash, name, prefix string) (KeyInfo, error) {
	var info KeyInfo
	err := p.pool.QueryRow(ctx,
		`INSERT INTO api_keys (user_id, key_hash, name, key_prefix, active)
		 VALUES ($1, $2, $3, $4, TRUE)
		 RETURNING id, created_at`,
		userID, keyHash, name, prefix).Scan(&info.ID, &info.CreatedAt)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("store: insert user key: %w", err)
	}
	uid := userID
	info.UserID = &uid
	info.KeyHash = keyHash
	info.Name = name
	info.Prefix = prefix
	info.Active = true
	return info, nil
}

// ListByUser returns userID's keys (active and revoked), newest first. It never
// returns the key hash beyond the display prefix.
func (p *Postgres) ListByUser(ctx context.Context, userID string) ([]KeyInfo, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(key_prefix, ''), active, created_at, revoked_at
		 FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list by user query: %w", err)
	}
	defer rows.Close()

	uid := userID
	var out []KeyInfo
	for rows.Next() {
		k := KeyInfo{UserID: &uid}
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: list by user scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list by user rows: %w", err)
	}
	return out, nil
}

// RevokeByIDForUser marks one of userID's keys inactive and returns the revoked
// key's hash so callers can purge any cached validation verdict for it. It
// reports whether a row changed; a mismatched owner or unknown id changes
// nothing (reported as ("", false, nil)).
func (p *Postgres) RevokeByIDForUser(ctx context.Context, userID string, id int64) (keyHash string, changed bool, err error) {
	err = p.pool.QueryRow(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = now()
		 WHERE id = $1 AND user_id = $2 AND active = TRUE
		 RETURNING key_hash`, id, userID).Scan(&keyHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: revoke by id: %w", err)
	}
	return keyHash, true, nil
}

// CountActiveByUser returns how many active keys userID currently holds.
func (p *Postgres) CountActiveByUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE user_id = $1 AND active = TRUE`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active: %w", err)
	}
	return n, nil
}
