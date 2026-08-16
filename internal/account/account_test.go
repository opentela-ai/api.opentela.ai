package account

import (
	"context"
	"testing"
)

func TestWithIDRoundTrip(t *testing.T) {
	ctx := WithID(context.Background(), "user-123")
	id, ok := ID(ctx)
	if !ok {
		t.Fatal("ID returned ok=false after WithID")
	}
	if id != "user-123" {
		t.Fatalf("ID=%q, want user-123", id)
	}
}

func TestIDAbsentFromBareContext(t *testing.T) {
	id, ok := ID(context.Background())
	if ok {
		t.Fatalf("ID returned ok=true with no value, id=%q", id)
	}
	if id != "" {
		t.Fatalf("ID=%q, want empty", id)
	}
}

// An empty id is stored as absent so legacy keys that carry no user_id cannot
// be mistaken for a present billing identity on the hot path.
func TestWithIDEmptyIsAbsent(t *testing.T) {
	ctx := WithID(context.Background(), "")
	id, ok := ID(ctx)
	if ok {
		t.Fatalf("ID returned ok=true for empty id, id=%q", id)
	}
	if id != "" {
		t.Fatalf("ID=%q, want empty", id)
	}
}

func TestWithIDPreservesExistingContextValues(t *testing.T) {
	type k int
	ctx := context.WithValue(context.Background(), k(1), "keep")
	ctx = WithID(ctx, "user-9")
	if v := ctx.Value(k(1)); v != "keep" {
		t.Fatalf("existing value=%v, want keep", v)
	}
	if id, ok := ID(ctx); !ok || id != "user-9" {
		t.Fatalf("ID=(%q,%v), want (user-9,true)", id, ok)
	}
}
