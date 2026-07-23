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
	ddl2, err := os.ReadFile("../../migrations/0002_user_keys.sql")
	if err != nil {
		t.Fatalf("read migration 0002: %v", err)
	}
	if err := p.Migrate(ctx, string(ddl2)); err != nil {
		t.Fatalf("migrate 0002: %v", err)
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

func TestPostgresUserKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const alice, bob = "user-alice", "user-bob"

	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(empty) = (%d,%v), want (0,nil)", n, err)
	}

	info, err := p.InsertUserKey(ctx, alice, HashKey("tok-a"), "laptop", "sk-1a2b3c4d")
	if err != nil {
		t.Fatalf("InsertUserKey: %v", err)
	}
	if info.ID == 0 || info.Name != "laptop" || info.Prefix != "sk-1a2b3c4d" || !info.Active {
		t.Fatalf("InsertUserKey returned %+v", info)
	}
	if info.CreatedAt.IsZero() {
		t.Fatal("InsertUserKey CreatedAt is zero")
	}

	// The inserted key validates through the existing proxy path.
	if ok, err := p.Validate(ctx, HashKey("tok-a")); err != nil || !ok {
		t.Fatalf("Validate(new user key) = (%v,%v), want (true,nil)", ok, err)
	}

	// Listing is owner-scoped and never leaks another user's keys.
	if _, err := p.InsertUserKey(ctx, bob, HashKey("tok-b"), "bob-key", "sk-9999abcd"); err != nil {
		t.Fatalf("InsertUserKey(bob): %v", err)
	}
	rows, err := p.ListByUser(ctx, alice)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != info.ID || rows[0].Prefix != "sk-1a2b3c4d" {
		t.Fatalf("ListByUser(alice) = %+v, want only alice's key", rows)
	}

	// Bob cannot revoke Alice's key.
	if changed, err := p.RevokeByIDForUser(ctx, bob, info.ID); err != nil || changed {
		t.Fatalf("RevokeByIDForUser(bob, alice's id) = (%v,%v), want (false,nil)", changed, err)
	}
	// Alice can.
	if changed, err := p.RevokeByIDForUser(ctx, alice, info.ID); err != nil || !changed {
		t.Fatalf("RevokeByIDForUser(alice) = (%v,%v), want (true,nil)", changed, err)
	}
	// Revoked keys drop out of the active count.
	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(after revoke) = (%d,%v), want (0,nil)", n, err)
	}
}
