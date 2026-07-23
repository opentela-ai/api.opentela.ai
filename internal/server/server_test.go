package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubValidator struct{ valid bool }

func (s stubValidator) Valid(context.Context, string) (bool, error) { return s.valid, nil }

func proxyStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	})
}

func TestHealthzOpen(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz code = %d, want 200", rec.Code)
	}
}

func TestProxyRequiresAuth(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy code = %d, want 401", rec.Code)
	}
}

func TestProxyPassesWhenValid(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub(), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
}

func TestKeyMgmtRoutedWhenPresent(t *testing.T) {
	keyMgmt := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel proving we reached the mgmt handler
	})
	h := New(stubValidator{valid: false}, proxyStub(), keyMgmt)

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code=%d, want 418 (mgmt handler reached, not proxy auth)", rec.Code)
	}
}

func TestManageNotRoutedWhenNil(t *testing.T) {
	// With no mgmt handler, /manage/* falls through to the auth-gated proxy → 401.
	h := New(stubValidator{valid: false}, proxyStub(), nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}
