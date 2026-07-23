package keysvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/store"
)

// fakeStore is an in-memory Store for unit tests.
type fakeStore struct {
	active     int
	insertErr  error
	lastUserID string
	lastHash   string
	lastPrefix string
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
func (f *fakeStore) RevokeByIDForUser(context.Context, string, int64) (bool, error) { return true, nil }

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
	svc := New(fs, 10)
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
	svc := New(fs, 3)
	if _, _, err := svc.Create(context.Background(), "alice", ""); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("Create at cap: err = %v, want ErrTooManyKeys", err)
	}
}
