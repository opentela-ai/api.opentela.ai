package store

import (
	"context"
	"os"
	"testing"
)

// newTestStore connects to TEST_DATABASE_URL and applies the schema. It skips the
// test when the variable is unset so unit runs stay hermetic.
func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	p, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx, "DROP TABLE IF EXISTS api_keys;"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	ddl, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := p.Migrate(ctx, string(ddl)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return p
}

func TestPostgresValidateLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	hash := HashKey("token-1")

	// Unknown key → not valid, no error.
	if ok, err := p.Validate(ctx, hash); err != nil || ok {
		t.Fatalf("Validate(unknown) = (%v,%v), want (false,nil)", ok, err)
	}

	// Insert → valid.
	if err := p.Insert(ctx, hash, "alice"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if ok, err := p.Validate(ctx, hash); err != nil || !ok {
		t.Fatalf("Validate(active) = (%v,%v), want (true,nil)", ok, err)
	}

	// Revoke → not valid.
	changed, err := p.Revoke(ctx, hash)
	if err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}
	if ok, err := p.Validate(ctx, hash); err != nil || ok {
		t.Fatalf("Validate(revoked) = (%v,%v), want (false,nil)", ok, err)
	}

	// List returns the row.
	rows, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "alice" || rows[0].Active {
		t.Fatalf("List = %+v, want one inactive row named alice", rows)
	}
}
