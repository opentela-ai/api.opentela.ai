// Package auth validates API keys against a cache-fronted key store and provides
// the Bearer-token HTTP middleware that gates the proxy.
package auth

import (
	"context"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// Validator answers whether a plaintext token is valid, consulting an in-memory
// cache before the backing store. Positive results live for posTTL, negatives for
// negTTL. Store errors are surfaced and never cached.
type Validator struct {
	store  store.KeyStore
	cache  *cache.Cache
	posTTL time.Duration
	negTTL time.Duration
}

// NewValidator wires a store and cache together with the two TTLs.
func NewValidator(s store.KeyStore, c *cache.Cache, posTTL, negTTL time.Duration) *Validator {
	return &Validator{store: s, cache: c, posTTL: posTTL, negTTL: negTTL}
}

// Valid hashes the token, checks the cache, and on a miss consults the store and
// caches the result with the TTL appropriate to the outcome.
func (v *Validator) Valid(ctx context.Context, token string) (bool, error) {
	h := store.HashKey(token)
	if val, ok := v.cache.Get(h); ok {
		return val, nil
	}
	valid, err := v.store.Validate(ctx, h)
	if err != nil {
		return false, err
	}
	if valid {
		v.cache.Set(h, true, v.posTTL)
	} else {
		v.cache.Set(h, false, v.negTTL)
	}
	return valid, nil
}
