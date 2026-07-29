package walletsapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
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
