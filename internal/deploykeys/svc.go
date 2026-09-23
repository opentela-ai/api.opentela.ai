// Package deploykeys mints and manages scoped deploy keys for instance
// linking. A deploy key ("otd-...") authorizes exactly one operation —
// registering an instance under the issuing account via the libp2p
// challenge flow — and nothing else. It exists so operators never have to
// place a full account credential (Neon Auth JWT) on their nodes: keys are
// usage-capped, expire, and are revocable from the console.
package deploykeys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/opentela-ai/api/internal/store"
)

// ErrTooManyKeys is returned by Create when the account is at its cap.
var ErrTooManyKeys = errors.New("deploykeys: key limit reached")

// ErrInvalidTTL / ErrInvalidMaxUses guard mint parameters.
var (
	ErrInvalidTTL     = errors.New("deploykeys: invalid ttl")
	ErrInvalidMaxUses = errors.New("deploykeys: invalid max_uses")
)

// Bounds for mint parameters. MinTTL keeps keys from being uselessly
// ephemeral; MaxTTL keeps "forever" keys from accumulating; MaxUses keeps a
// single leaked key from silently registering unbounded peers.
const (
	MinTTL     = 5 * time.Minute
	MaxTTL     = 90 * 24 * time.Hour
	MinMaxUses = 1
	MaxMaxUses = 100
)

// Store is the persistence surface the service needs.
type Store interface {
	InsertDeployKey(ctx context.Context, userID, keyHash, keyPrefix, name string, maxUses int, expiresAt *time.Time) (store.DeployKeyInfo, error)
	ListDeployKeysByUser(ctx context.Context, userID string) ([]store.DeployKeyInfo, error)
	CountActiveDeployKeysByUser(ctx context.Context, userID string) (int, error)
	RevokeDeployKeyByIDForUser(ctx context.Context, userID string, id int64) (keyHash string, changed bool, err error)
}

// Service creates, lists, and revokes deploy keys.
type Service struct {
	store      Store
	maxPerUser int
	now        func() time.Time
}

// New wires the store with the per-user active-key cap.
func New(s Store, maxPerUser int) *Service {
	return &Service{store: s, maxPerUser: maxPerUser, now: time.Now}
}

// GenerateToken returns a random opaque token of the form
// "otd-<48 hex chars>" (same entropy budget as keysvc's "sk-" tokens).
func GenerateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "otd-" + hex.EncodeToString(b), nil
}

// Prefix returns the non-secret display prefix ("otd-" + first 8 hex chars).
func Prefix(token string) string {
	if len(token) < 12 {
		return token
	}
	return token[:12]
}

// Create mints a deploy key for userID. ttl may be zero for an
// expiry-free key; maxUses must be within [MinMaxUses, MaxMaxUses]. The
// plaintext is returned exactly once.
func (s *Service) Create(ctx context.Context, userID, name string, maxUses int, ttl time.Duration) (string, store.DeployKeyInfo, error) {
	if maxUses < MinMaxUses || maxUses > MaxMaxUses {
		return "", store.DeployKeyInfo{}, ErrInvalidMaxUses
	}
	if ttl < 0 || (ttl != 0 && ttl < MinTTL) || ttl > MaxTTL {
		return "", store.DeployKeyInfo{}, ErrInvalidTTL
	}
	n, err := s.store.CountActiveDeployKeysByUser(ctx, userID)
	if err != nil {
		return "", store.DeployKeyInfo{}, err
	}
	if n >= s.maxPerUser {
		return "", store.DeployKeyInfo{}, ErrTooManyKeys
	}
	token, err := GenerateToken()
	if err != nil {
		return "", store.DeployKeyInfo{}, err
	}
	var expiresAt *time.Time
	if ttl > 0 {
		t := s.now().UTC().Add(ttl)
		expiresAt = &t
	}
	info, err := s.store.InsertDeployKey(ctx, userID, store.HashKey(token), Prefix(token), name, maxUses, expiresAt)
	if err != nil {
		return "", store.DeployKeyInfo{}, err
	}
	return token, info, nil
}

// List returns the account's deploy keys, newest first.
func (s *Service) List(ctx context.Context, userID string) ([]store.DeployKeyInfo, error) {
	return s.store.ListDeployKeysByUser(ctx, userID)
}

// Revoke revokes one of the account's keys by id, reporting whether a row
// changed. Deploy keys authenticate only the link endpoints, which resolve
// the key from the database on every call, so no verdict cache purge is
// needed: revocation takes effect on the next request.
func (s *Service) Revoke(ctx context.Context, userID string, id int64) (bool, error) {
	_, changed, err := s.store.RevokeDeployKeyByIDForUser(ctx, userID, id)
	return changed, err
}
