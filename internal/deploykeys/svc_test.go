package deploykeys

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/store"
)

type fakeStore struct {
	keys    map[string]store.DeployKeyInfo // keyHash -> row
	nextID  int64
	userCap int
}

func newFakeStore(cap int) *fakeStore {
	return &fakeStore{keys: map[string]store.DeployKeyInfo{}, userCap: cap}
}

func (f *fakeStore) InsertDeployKey(_ context.Context, userID, keyHash, keyPrefix, name string, maxUses int, expiresAt *time.Time) (store.DeployKeyInfo, error) {
	f.nextID++
	k := store.DeployKeyInfo{
		ID: f.nextID, UserID: userID, KeyHash: keyHash, KeyPrefix: keyPrefix,
		Name: name, MaxUses: maxUses, ExpiresAt: expiresAt, Active: true,
		CreatedAt: time.Now().UTC(),
	}
	f.keys[keyHash] = k
	return k, nil
}

func (f *fakeStore) ListDeployKeysByUser(_ context.Context, userID string) ([]store.DeployKeyInfo, error) {
	var out []store.DeployKeyInfo
	for _, k := range f.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeStore) CountActiveDeployKeysByUser(_ context.Context, userID string) (int, error) {
	n := 0
	for _, k := range f.keys {
		if k.UserID == userID && k.RevokedAt == nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) RevokeDeployKeyByIDForUser(_ context.Context, userID string, id int64) (string, bool, error) {
	for h, k := range f.keys {
		if k.ID == id && k.UserID == userID && k.RevokedAt == nil {
			now := time.Now().UTC()
			k.RevokedAt = &now
			k.Active = false
			f.keys[h] = k
			return h, true, nil
		}
	}
	return "", false, nil
}

func TestCreateMintsOtdToken(t *testing.T) {
	s := New(newFakeStore(5), 5)
	tok, info, err := s.Create(context.Background(), "user-1", "ci node", 1, 24*time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(tok, "otd-") || len(tok) != len("otd-")+48 {
		t.Fatalf("token shape: %q", tok)
	}
	if info.KeyPrefix != tok[:12] {
		t.Fatalf("prefix %q, want %q", info.KeyPrefix, tok[:12])
	}
	if info.ExpiresAt == nil || !info.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expires_at not in the future: %v", info.ExpiresAt)
	}
}

func TestCreateRejectsBadBounds(t *testing.T) {
	s := New(newFakeStore(5), 5)
	if _, _, err := s.Create(context.Background(), "u", "n", 0, time.Hour); err != ErrInvalidMaxUses {
		t.Fatalf("max_uses=0: %v", err)
	}
	if _, _, err := s.Create(context.Background(), "u", "n", 101, time.Hour); err != ErrInvalidMaxUses {
		t.Fatalf("max_uses=101: %v", err)
	}
	if _, _, err := s.Create(context.Background(), "u", "n", 1, time.Second); err != ErrInvalidTTL {
		t.Fatalf("ttl=1s: %v", err)
	}
	if _, _, err := s.Create(context.Background(), "u", "n", 1, 91*24*time.Hour); err != ErrInvalidTTL {
		t.Fatalf("ttl=91d: %v", err)
	}
	// ttl=0 (no expiry) and exactly MinTTL are legal.
	if _, _, err := s.Create(context.Background(), "u", "n", 1, 0); err != nil {
		t.Fatalf("ttl=0: %v", err)
	}
	if _, _, err := s.Create(context.Background(), "u", "n", 1, MinTTL); err != nil {
		t.Fatalf("ttl=min: %v", err)
	}
}

func TestCreateEnforcesPerUserCap(t *testing.T) {
	s := New(newFakeStore(2), 2)
	for i := 0; i < 2; i++ {
		if _, _, err := s.Create(context.Background(), "u", "n", 1, time.Hour); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, _, err := s.Create(context.Background(), "u", "n", 1, time.Hour); err != ErrTooManyKeys {
		t.Fatalf("want ErrTooManyKeys, got %v", err)
	}
}

func TestRevokeReportsChanged(t *testing.T) {
	st := newFakeStore(5)
	s := New(st, 5)
	_, info, err := s.Create(context.Background(), "u", "n", 1, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err := s.Revoke(context.Background(), "u", info.ID)
	if err != nil || !ok {
		t.Fatalf("revoke: changed=%v err=%v", ok, err)
	}
	if ok, _ := s.Revoke(context.Background(), "u", info.ID); ok {
		t.Fatal("second revoke must not change anything")
	}
	if ok, _ := s.Revoke(context.Background(), "other", info.ID); ok {
		t.Fatal("revoking another user's key must not change anything")
	}
}
