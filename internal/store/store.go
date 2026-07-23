// Package store defines the API-key persistence contract and its Postgres
// implementation. Keys are addressed by their SHA-256 hex digest, never plaintext.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// HashKey returns the lowercase SHA-256 hex digest of a plaintext API token.
func HashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// KeyStore is the minimal contract the request path depends on: given a key hash,
// report whether a matching active key exists.
type KeyStore interface {
	Validate(ctx context.Context, keyHash string) (bool, error)
}

// KeyInfo is a row of the api_keys table. ID, Prefix, and UserID are populated
// by the user-scoped queries; the admin List (by hash) leaves them zero.
type KeyInfo struct {
	ID        int64
	KeyHash   string
	UserID    *string
	Name      string
	Prefix    string
	Active    bool
	CreatedAt time.Time
	RevokedAt *time.Time
}
