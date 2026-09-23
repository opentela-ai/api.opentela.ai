package manageapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/store"
)

type verifierStub struct {
	claims neonauth.Claims
}

func (v verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return v.claims, nil
}

type identityStoreStub struct {
	calls int
	last  store.IdentityInfo
}

func (s *identityStoreStub) RefreshIdentity(_ context.Context, in store.IdentityInfo) error {
	s.calls++
	s.last = in
	return nil
}

type routeStub struct {
	status int
}

func (r routeStub) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(req.URL.Path))
	})
}

type keyServiceStub struct{}

func (keyServiceStub) Create(context.Context, string, string) (string, store.KeyInfo, error) {
	return "sk-test", store.KeyInfo{ID: 1, Prefix: "sk-test"}, nil
}
func (keyServiceStub) List(context.Context, string) ([]store.KeyInfo, error) { return nil, nil }
func (keyServiceStub) Revoke(context.Context, string, int64) (bool, error)   { return true, nil }

func TestRouterRefreshesIdentityForWalletRoutes(t *testing.T) {
	store := &identityStoreStub{}
	h := Router(keyServiceStub{}, routeStub{status: http.StatusAccepted}, nil, nil, nil, nil, nil, verifierStub{claims: neonauth.Claims{
		Subject:       "user-alice",
		Email:         "alice@example.com",
		EmailVerified: true,
	}}, store, []string{"https://app.example"})

	req := httptest.NewRequest(http.MethodGet, "/manage/wallets", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d, want 202", rec.Code)
	}
	if store.calls != 1 {
		t.Fatalf("refresh calls=%d, want 1", store.calls)
	}
	if store.last.AccountID != "user-alice" || store.last.EmailDomain != "example.com" || !store.last.EmailVerified {
		t.Fatalf("identity refresh=%+v, want normalized verified identity", store.last)
	}
	if time.Since(store.last.LastVerifiedAt) > time.Minute {
		t.Fatalf("last_verified_at=%s not refreshed recently", store.last.LastVerifiedAt)
	}
}

func TestRouterAnswersManagePreflightWithoutJWTVerification(t *testing.T) {
	h := Router(keyServiceStub{}, routeStub{status: http.StatusAccepted}, nil, nil, nil, nil, nil, verifierStub{}, &identityStoreStub{}, []string{"https://app.example"})

	req := httptest.NewRequest(http.MethodOptions, "/manage/wallets", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO=%q, want echoed origin", got)
	}
}

func TestRouterMountsInstanceRoutesOnSharedStack(t *testing.T) {
	store := &identityStoreStub{}
	h := Router(keyServiceStub{}, nil, routeStub{status: http.StatusCreated}, nil, nil, nil, nil, verifierStub{claims: neonauth.Claims{
		Subject: "user-bob",
	}}, store, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/instances", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	if store.calls != 1 || store.last.AccountID != "user-bob" {
		t.Fatalf("refresh calls=%d last=%+v, want one refresh for bob", store.calls, store.last)
	}
}

func TestRouterMountsBillingRoutesWithAuth(t *testing.T) {
	h := Router(keyServiceStub{}, nil, nil, nil, nil, routeStub{status: http.StatusTeapot}, nil, verifierStub{claims: neonauth.Claims{
		Subject: "user-carol",
	}}, &identityStoreStub{}, []string{"https://app.example"})

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("code=%d, want 418 (billing route dispatched with auth)", rec.Code)
	}

	// No Authorization header → the surrounding principal.Middleware rejects
	// before the billing handler runs.
	req2 := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401 (billing requires auth)", rec2.Code)
	}
}
