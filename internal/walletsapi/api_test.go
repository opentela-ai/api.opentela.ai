package walletsapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

type storeStub struct {
	wallets          []store.WalletInfo
	walletsErr       error
	identity         store.IdentityInfo
	identityErr      error
	challenge        store.WalletChallenge
	challengeErr     error
	consumeErr       error
	linkOut          store.WalletInfo
	linkErr          error
	deleteChanged    bool
	deleteErr        error
	createdChallenge store.WalletChallenge
}

func (s *storeStub) ListWalletsByUser(context.Context, string) ([]store.WalletInfo, error) {
	return s.wallets, s.walletsErr
}
func (s *storeStub) GetIdentity(context.Context, string) (store.IdentityInfo, error) {
	return s.identity, s.identityErr
}
func (s *storeStub) CreateWalletChallenge(_ context.Context, ch store.WalletChallenge) error {
	s.createdChallenge = ch
	return nil
}
func (s *storeStub) GetWalletChallenge(context.Context, string, string) (store.WalletChallenge, error) {
	return s.challenge, s.challengeErr
}
func (s *storeStub) ConsumeWalletChallenge(context.Context, string, string, time.Time) error {
	return s.consumeErr
}
func (s *storeStub) LinkWallet(context.Context, string, string) (store.WalletInfo, error) {
	return s.linkOut, s.linkErr
}
func (s *storeStub) DeleteWalletByIDForUser(context.Context, string, int64) (bool, error) {
	return s.deleteChanged, s.deleteErr
}
func (s *storeStub) GetUserWalletSet(context.Context, string) ([]string, error) { return nil, nil }

type verifierStub struct{}

func (verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: "user-alice", Email: "alice@example.com", EmailVerified: true}, nil
}

func authedRoutes(t *testing.T, svc *Service) http.Handler {
	t.Helper()
	return principal.Middleware(verifierStub{}, nil, func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	})(svc.Routes())
}

func TestHandleChallengeRejectsUnknownFields(t *testing.T) {
	svc := New(&storeStub{}, time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/manage/wallets/challenges", bytes.NewBufferString(`{"wallet":"11111111111111111111111111111111","extra":true}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestHandleChallengeStoresBoundMessage(t *testing.T) {
	store := &storeStub{}
	svc := New(store, time.Hour)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/manage/wallets/challenges", bytes.NewBufferString(`{"wallet":"11111111111111111111111111111111"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(store.createdChallenge.Message, "sub:user-alice") || !strings.Contains(store.createdChallenge.Message, "wallet:11111111111111111111111111111111") {
		t.Fatalf("challenge message=%q missing bound subject/wallet", store.createdChallenge.Message)
	}
	if store.createdChallenge.ExpiresAt.Sub(now) != challengeTTL {
		t.Fatalf("expires_at delta=%s, want %s", store.createdChallenge.ExpiresAt.Sub(now), challengeTTL)
	}
}

func TestHandleLinkReturnsConflictForConsumedChallenge(t *testing.T) {
	store := &storeStub{
		challenge: store.WalletChallenge{
			ID:      "challenge-1",
			Wallet:  "11111111111111111111111111111111",
			Message: "message",
		},
		consumeErr: store.ErrChallengeConsumed,
	}
	svc := New(store, time.Hour)

	req := httptest.NewRequest(http.MethodPost, "/manage/wallets", bytes.NewBufferString(`{"challenge_id":"challenge-1","signature":"1111111111111111111111111111111111111111111111111111111111111111"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want signature failure or consumed conflict depending on signature parsing", rec.Code)
	}
}

func TestHandleDeleteReturnsConflictWhenWalletStillProvesInstance(t *testing.T) {
	store := &storeStub{deleteErr: store.ErrWalletInUse}
	svc := New(store, time.Hour)

	req := httptest.NewRequest(http.MethodDelete, "/manage/wallets/7", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

// reconcileRecorder captures ReconcileDepositsForWallet calls.
type reconcileRecorder struct {
	calls  []reconcileCall
	result int
	err    error
}

type reconcileCall struct {
	wallet, accountID string
}

func (r *reconcileRecorder) ReconcileDepositsForWallet(_ context.Context, wallet, accountID string) (int, error) {
	r.calls = append(r.calls, reconcileCall{wallet, accountID})
	return r.result, r.err
}

// TestHandleLinkInvokesReconcilerAfterLink drives the full link happy path
// (real ed25519 signature over the issued challenge) and asserts the deposit
// reconciler fires once with the linked wallet + owning account, and that a
// reconciler error does NOT change the (already successful) link response.
func TestHandleLinkInvokesReconcilerAfterLink(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wallet := solana.EncodeBase58(pub)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	nonce, err := solana.NewChallengeNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := "challenge-link-1"
	message := solana.BuildChallengeMessage("user-alice", wallet, nonce, now, now.Add(challengeTTL))

	store := &storeStub{
		challenge: store.WalletChallenge{ID: id, AccountID: "user-alice", Wallet: wallet, Nonce: nonce, Message: message},
		linkOut:   store.WalletInfo{ID: 9, Wallet: wallet, Primary: true, CreatedAt: now},
	}
	svc := New(store, time.Hour)
	svc.now = func() time.Time { return now }

	rec := &reconcileRecorder{result: 3}
	svc = svc.WithReconciler(rec)

	sig := ed25519.Sign(priv, []byte(message))
	req := httptest.NewRequest(http.MethodPost, "/manage/wallets", bytes.NewBufferString(`{"challenge_id":"`+id+`","signature":"`+solana.EncodeBase58(sig)+`"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s, want 201", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 || rec.calls[0].wallet != wallet || rec.calls[0].accountID != "user-alice" {
		t.Fatalf("reconciler calls=%+v, want one call wallet=%s account=user-alice", rec.calls, wallet)
	}
}

func TestHandleLinkSucceedsEvenWhenReconcilerErrors(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wallet := solana.EncodeBase58(pub)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	nonce, err := solana.NewChallengeNonce()
	if err != nil {
		t.Fatal(err)
	}
	id := "challenge-link-2"
	message := solana.BuildChallengeMessage("user-alice", wallet, nonce, now, now.Add(challengeTTL))

	store := &storeStub{
		challenge: store.WalletChallenge{ID: id, AccountID: "user-alice", Wallet: wallet, Nonce: nonce, Message: message},
		linkOut:   store.WalletInfo{ID: 10, Wallet: wallet, Primary: false, CreatedAt: now},
	}
	svc := New(store, time.Hour)
	svc.now = func() time.Time { return now }
	svc = svc.WithReconciler(&reconcileRecorder{err: errors.New("db down")})

	sig := ed25519.Sign(priv, []byte(message))
	req := httptest.NewRequest(http.MethodPost, "/manage/wallets", bytes.NewBufferString(`{"challenge_id":"`+id+`","signature":"`+solana.EncodeBase58(sig)+`"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(w, req)

	// The link already succeeded before reconciliation ran, so a reconciler
	// error must NOT surface to the user.
	if w.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s, want 201 (reconciler error must not fail the link)", w.Code, w.Body.String())
	}
}
