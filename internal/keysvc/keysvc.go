// Package keysvc mints and manages user-owned API keys. It owns token generation
// (shared with keyctl) and enforces the per-user active-key cap.
package keysvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/opentela-ai/api/internal/store"
)

// ErrTooManyKeys is returned by Create when the user is at their active-key cap.
var ErrTooManyKeys = errors.New("keysvc: key limit reached")

// Store is the persistence surface keysvc needs.
type Store interface {
	InsertUserKey(ctx context.Context, userID, keyHash, name, prefix string) (store.KeyInfo, error)
	CountActiveByUser(ctx context.Context, userID string) (int, error)
	ListByUser(ctx context.Context, userID string) ([]store.KeyInfo, error)
	RevokeByIDForUser(ctx context.Context, userID string, id int64) (bool, error)
}

// Service creates, lists, and revokes user-owned keys.
type Service struct {
	store      Store
	maxPerUser int
}

// New wires a Store with the per-user active-key cap.
func New(s Store, maxPerUser int) *Service {
	return &Service{store: s, maxPerUser: maxPerUser}
}

// GenerateToken returns a random opaque token of the form "sk-<48 hex chars>".
func GenerateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// Prefix returns the non-secret display prefix ("sk-" + first 8 hex chars).
func Prefix(token string) string {
	if len(token) < 11 {
		return token
	}
	return token[:11]
}

// Create mints a new key for userID, returning the plaintext exactly once along
// with the stored row. It fails with ErrTooManyKeys if the user is at the cap.
func (s *Service) Create(ctx context.Context, userID, name string) (string, store.KeyInfo, error) {
	// Soft cap: a benign race could let a user exceed it by one; acceptable.
	n, err := s.store.CountActiveByUser(ctx, userID)
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	if n >= s.maxPerUser {
		return "", store.KeyInfo{}, ErrTooManyKeys
	}
	token, err := GenerateToken()
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	info, err := s.store.InsertUserKey(ctx, userID, store.HashKey(token), name, Prefix(token))
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	return token, info, nil
}

// List returns userID's keys, newest first.
func (s *Service) List(ctx context.Context, userID string) ([]store.KeyInfo, error) {
	return s.store.ListByUser(ctx, userID)
}

// Revoke revokes one of userID's keys by id, reporting whether a row changed.
func (s *Service) Revoke(ctx context.Context, userID string, id int64) (bool, error) {
	return s.store.RevokeByIDForUser(ctx, userID, id)
}
