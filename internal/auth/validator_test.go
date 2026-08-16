package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// fakeStore records how many times Validate is called and returns a scripted
// result: the owning account id (empty for legacy keys), validity, and error.
type fakeStore struct {
	accountID string
	valid     bool
	err       error
	calls     int
}

func (f *fakeStore) Validate(_ context.Context, _ string) (string, bool, error) {
	f.calls++
	if !f.valid {
		return "", false, f.err
	}
	return f.accountID, true, f.err
}

func TestValidatorValidCaches(t *testing.T) {
	fs := &fakeStore{valid: true, accountID: "alice"}
	c := cache.New[Verdict](0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Second)

	for i := 0; i < 3; i++ {
		id, ok, err := v.Valid(context.Background(), "tok")
		if err != nil || !ok || id != "alice" {
			t.Fatalf("Valid = (%q,%v,%v), want (alice,true,nil)", id, ok, err)
		}
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times, want 1 (cache should serve the rest)", fs.calls)
	}
}

func TestValidatorValidLegacyCaches(t *testing.T) {
	// A legacy key (no user_id) is valid but carries an empty account id; the
	// verdict is still cached positively.
	fs := &fakeStore{valid: true, accountID: ""}
	c := cache.New[Verdict](0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Second)

	for i := 0; i < 3; i++ {
		id, ok, err := v.Valid(context.Background(), "legacy")
		if err != nil || !ok || id != "" {
			t.Fatalf("Valid(legacy) = (%q,%v,%v), want (\"\",true,nil)", id, ok, err)
		}
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times, want 1", fs.calls)
	}
}

func TestValidatorInvalidCachedNegatively(t *testing.T) {
	fs := &fakeStore{valid: false}
	c := cache.New[Verdict](0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	id, ok, err := v.Valid(context.Background(), "bad")
	if err != nil || ok || id != "" {
		t.Fatalf("Valid = (%q,%v,%v), want (\"\",false,nil)", id, ok, err)
	}
	// Cached negatively → no second store call.
	if _, _, _ = v.Valid(context.Background(), "bad"); fs.calls != 1 {
		t.Fatalf("store called %d times, want 1", fs.calls)
	}
	// The cached key is the hash, and the verdict is invalid.
	if val, hit := c.Get(store.HashKey("bad")); !hit || val.Valid {
		t.Fatalf("cache Get = (%+v,%v), want (valid:false,true)", val, hit)
	}
}

func TestValidatorStoreErrorNotCached(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	c := cache.New[Verdict](0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	if _, _, err := v.Valid(context.Background(), "tok"); err == nil {
		t.Fatal("Valid expected error, got nil")
	}
	if _, hit := c.Get(store.HashKey("tok")); hit {
		t.Fatal("store error must not be cached")
	}
}
