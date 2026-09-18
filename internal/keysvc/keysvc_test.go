package keysvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// *cache.Cache[T] must satisfy RevocationCache regardless of its value type.
var _ RevocationCache = (*cache.Cache[int])(nil)

// fakeStore is an in-memory Store for unit tests.
type fakeStore struct {
	active     int
	insertErr  error
	lastUserID string
	lastHash   string
	lastPrefix string

	revokeHash    string // hash returned by RevokeByIDForUser when it succeeds
	revokeChanged bool
	revokeErr     error
}

func (f *fakeStore) InsertUserKey(_ context.Context, userID, keyHash, name, prefix string) (store.KeyInfo, error) {
	if f.insertErr != nil {
		return store.KeyInfo{}, f.insertErr
	}
	f.lastUserID, f.lastHash, f.lastPrefix = userID, keyHash, prefix
	f.active++
	uid := userID
	return store.KeyInfo{ID: 1, UserID: &uid, Name: name, Prefix: prefix, Active: true}, nil
}
func (f *fakeStore) CountActiveByUser(context.Context, string) (int, error) { return f.active, nil }
func (f *fakeStore) ListByUser(context.Context, string) ([]store.KeyInfo, error) {
	return []store.KeyInfo{{ID: 1}}, nil
}
func (f *fakeStore) RevokeByIDForUser(context.Context, string, int64) (string, bool, error) {
	if f.revokeErr != nil {
		return "", false, f.revokeErr
	}
	return f.revokeHash, f.revokeChanged, nil
}

// fakeCache records Delete calls.
type fakeCache struct{ deleted []string }

func (f *fakeCache) Delete(keyHash string) { f.deleted = append(f.deleted, keyHash) }

func TestGenerateTokenFormat(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(tok, "sk-") || len(tok) != 3+48 {
		t.Fatalf("token %q has wrong shape", tok)
	}
	if Prefix(tok) != tok[:11] {
		t.Fatalf("Prefix(%q) = %q", tok, Prefix(tok))
	}
	if a, _ := GenerateToken(); a == tok {
		t.Fatal("GenerateToken returned identical tokens")
	}
}

func TestCreateStoresHashAndPrefix(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, 10, nil)
	tok, info, err := svc.Create(context.Background(), "alice", "laptop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fs.lastHash != store.HashKey(tok) {
		t.Fatal("Create stored a hash that does not match the returned token")
	}
	if fs.lastPrefix != tok[:11] || info.Prefix != tok[:11] {
		t.Fatalf("prefix mismatch: stored=%q info=%q token=%q", fs.lastPrefix, info.Prefix, tok[:11])
	}
	if fs.lastUserID != "alice" {
		t.Fatalf("owner = %q, want alice", fs.lastUserID)
	}
}

func TestCreateEnforcesCap(t *testing.T) {
	fs := &fakeStore{active: 3}
	svc := New(fs, 3, nil)
	if _, _, err := svc.Create(context.Background(), "alice", ""); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("Create at cap: err = %v, want ErrTooManyKeys", err)
	}
}

func TestRevokeInvalidatesCachedVerdict(t *testing.T) {
	fs := &fakeStore{revokeHash: "abc123", revokeChanged: true}
	fc := &fakeCache{}
	svc := New(fs, 10, fc)
	changed, err := svc.Revoke(context.Background(), "alice", 1)
	if err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "abc123" {
		t.Fatalf("cache deletes = %v, want [abc123]", fc.deleted)
	}
}

func TestRevokeMissDoesNotInvalidate(t *testing.T) {
	fs := &fakeStore{revokeChanged: false}
	fc := &fakeCache{}
	svc := New(fs, 10, fc)
	changed, err := svc.Revoke(context.Background(), "alice", 99)
	if err != nil || changed {
		t.Fatalf("Revoke = (%v,%v), want (false,nil)", changed, err)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("cache deletes = %v, want none", fc.deleted)
	}
}

func TestRevokeErrorDoesNotInvalidate(t *testing.T) {
	fs := &fakeStore{revokeErr: errors.New("db down")}
	fc := &fakeCache{}
	svc := New(fs, 10, fc)
	if _, err := svc.Revoke(context.Background(), "alice", 1); err == nil {
		t.Fatal("Revoke expected error, got nil")
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("cache deletes = %v, want none", fc.deleted)
	}
}

func TestRevokeNilCacheIsSafe(t *testing.T) {
	fs := &fakeStore{revokeHash: "abc123", revokeChanged: true}
	svc := New(fs, 10, nil)
	if changed, err := svc.Revoke(context.Background(), "alice", 1); err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}
}

// validatingStore additionally answers store.KeyStore.Validate from an active
// set, mimicking Postgres after each mutation.
type validatingStore struct {
	fakeStore
	valid map[string]bool
}

func (f *validatingStore) Validate(_ context.Context, h string) (string, bool, error) {
	return "alice", f.valid[h], nil
}

// TestRevokeInvalidatesValidationCacheEndToEnd is the regression test for the
// rotation bug: a revoked key must stop authenticating immediately, even though
// its positive verdict was cached with a long TTL.
func TestRevokeInvalidatesValidationCacheEndToEnd(t *testing.T) {
	fs := &validatingStore{valid: map[string]bool{}}
	c := cache.New[auth.Verdict](0)
	defer c.Close()
	svc := New(fs, 10, c)

	tok, info, err := svc.Create(context.Background(), "alice", "laptop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := store.HashKey(tok)
	fs.valid[h] = true

	v := auth.NewValidator(fs, c, time.Hour, time.Minute) // posTTL=1h: invalidation must not depend on TTL
	if _, ok, err := v.Valid(context.Background(), tok); err != nil || !ok {
		t.Fatalf("Valid(new key) = (_, %v, %v), want (alice, true, nil)", ok, err)
	}

	// Simulate the DB commit and revoke through the service.
	fs.valid[h] = false
	fs.revokeHash, fs.revokeChanged = h, true
	changed, err := svc.Revoke(context.Background(), "alice", info.ID)
	if err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}

	// The cached positive verdict was purged: the plaintext key is dead now.
	if _, ok, err := v.Valid(context.Background(), tok); err != nil || ok {
		t.Fatalf("Valid(revoked key) = (_, %v, %v), want ok=false immediately", ok, err)
	}
}
