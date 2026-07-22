package store

import (
	"context"
	"errors"

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
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate executes an arbitrary DDL string (used to apply migration files).
func (p *Postgres) Migrate(ctx context.Context, ddl string) error {
	_, err := p.pool.Exec(ctx, ddl)
	return err
}

// Validate reports whether an active key exists for the given hash.
func (p *Postgres) Validate(ctx context.Context, keyHash string) (bool, error) {
	var active bool
	err := p.pool.QueryRow(ctx,
		`SELECT active FROM api_keys WHERE key_hash = $1`, keyHash).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active, nil
}

// Insert adds a new active key. name may be empty.
func (p *Postgres) Insert(ctx context.Context, keyHash, name string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO api_keys (key_hash, name) VALUES ($1, $2)`, keyHash, name)
	return err
}

// Revoke marks a key inactive. It reports whether a row was changed.
func (p *Postgres) Revoke(ctx context.Context, keyHash string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = now()
		 WHERE key_hash = $1 AND active = TRUE`, keyHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// List returns all keys ordered by creation time.
func (p *Postgres) List(ctx context.Context) ([]KeyInfo, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key_hash, COALESCE(name, ''), active, created_at, revoked_at
		 FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.KeyHash, &k.Name, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
