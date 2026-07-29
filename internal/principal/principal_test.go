package principal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/neonauth"
)

type verifierStub struct {
	claims neonauth.Claims
	err    error
}

func (v verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return v.claims, v.err
}

type refresherStub struct {
	got Principal
	err error
}

func (r *refresherStub) RefreshIdentity(_ context.Context, p Principal) error {
	r.got = p
	return r.err
}

func TestMiddlewareStoresVerifiedEmailDomainFromFinalAt(t *testing.T) {
	refresher := &refresherStub{}
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	var got Principal
	h := Middleware(verifierStub{claims: neonauth.Claims{
		Subject:       "user-alice",
		Email:         "alice@sub@Example.COM",
		EmailVerified: true,
	}}, refresher, func() time.Time { return now })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		got, ok = FromContext(r.Context())
		if !ok {
			t.Fatal("principal missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	if got.EmailDomain != "example.com" || !got.EmailVerified {
		t.Fatalf("principal=%+v, want verified example.com domain", got)
	}
	if !refresher.got.VerifiedAt.Equal(now) {
		t.Fatalf("refresh verified_at=%s, want %s", refresher.got.VerifiedAt, now)
	}
}

func TestMiddlewareClearsVerifiedFlagForInvalidEmailDomain(t *testing.T) {
	var got Principal
	h := Middleware(verifierStub{claims: neonauth.Claims{
		Subject:       "user-alice",
		Email:         "alice@bücher.example",
		EmailVerified: true,
	}}, nil, func() time.Time { return time.Unix(0, 0) })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	if got.EmailVerified || got.EmailDomain != "" {
		t.Fatalf("principal=%+v, want email verification suppressed", got)
	}
}

func TestMiddlewareReturns503WhenIdentityRefreshFails(t *testing.T) {
	h := Middleware(verifierStub{claims: neonauth.Claims{Subject: "user-alice"}}, &refresherStub{
		err: errors.New("db down"),
	}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not run when identity refresh fails")
	}))

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", rec.Code)
	}
}
