// Package auth validates API keys against a cache-fronted key store and provides
// the Bearer-token HTTP middleware that gates the proxy.
package auth

import (
	"context"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// Verdict is the cached outcome of a key lookup: whether the key is active and,
// when it is, the owning account id (empty for legacy keys created before user
// ownership).
type Verdict struct {
	Valid     bool
	AccountID string
}

// NewValidationCache returns the in-memory cache used to front key validation,
// with an optional background janitor that evicts expired entries.
func NewValidationCache(janitorEvery time.Duration) *cache.Cache[Verdict] {
	return cache.New[Verdict](janitorEvery)
}

// Validator answers whether a plaintext token is valid and, when it is, the
// owning account id, consulting an in-memory cache before the backing store.
// Positive results live for posTTL, negatives for negTTL. Store errors are
// surfaced and never cached.
type Validator struct {
	store  store.KeyStore
	cache  *cache.Cache[Verdict]
	posTTL time.Duration
	negTTL time.Duration
}

// NewValidator wires a store and cache together with the two TTLs.
func NewValidator(s store.KeyStore, c *cache.Cache[Verdict], posTTL, negTTL time.Duration) *Validator {
	return &Validator{store: s, cache: c, posTTL: posTTL, negTTL: negTTL}
}

// Valid hashes the token, checks the cache, and on a miss consults the store and
// caches the result with the TTL appropriate to the outcome. accountID is the
// owning account id for an active key, or empty for a legacy key (one created
// before user ownership). When accountID is empty but ok is true, the key is a
// legacy key that cannot be charged; callers in enforcement mode reject such
// requests with 402 billing_account_required (see account.ID).
func (v *Validator) Valid(ctx context.Context, token string) (accountID string, ok bool, err error) {
	h := store.HashKey(token)
	if val, hit := v.cache.Get(h); hit {
		return val.AccountID, val.Valid, nil
	}
	id, valid, err := v.store.Validate(ctx, h)
	if err != nil {
		return "", false, err
	}
	ttl := v.posTTL
	if !valid {
		ttl = v.negTTL
	}
	v.cache.Set(h, Verdict{Valid: valid, AccountID: id}, ttl)
	return id, valid, nil
}
