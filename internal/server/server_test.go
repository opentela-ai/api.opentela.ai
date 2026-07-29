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
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz code = %d, want 200", rec.Code)
	}
}

func TestProxyRequiresAuth(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy code = %d, want 401", rec.Code)
	}
}

func TestProxyPassesWhenValid(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub(), nil, nil, nil)
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
	h := New(stubValidator{valid: false}, proxyStub(), keyMgmt, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code=%d, want 418 (mgmt handler reached, not proxy auth)", rec.Code)
	}
}

func TestInternalACLRouteBeatsProxyCatchAll(t *testing.T) {
	internal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("internal"))
	})
	h := NewWithInternal(stubValidator{valid: false}, proxyStub(), nil, internal, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != "internal" {
		t.Fatalf("code=%d body=%q, want 201/internal", rec.Code, rec.Body.String())
	}
}

func TestManageNotRoutedWhenNil(t *testing.T) {
	// With no mgmt handler, /manage/* falls through to the auth-gated proxy → 401.
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

// The proxy plane is CORS-enabled so browsers can call /v1/*. A preflight OPTIONS
// carries no credentials, so it must be answered 204 with the echoed origin
// WITHOUT requiring a Bearer key — i.e. it bypasses the API-key auth and never
// reaches the proxy.
func TestProxyPreflightBypassesAuth(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, []string{"https://app.example"})
	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d, want 204 (auth bypassed)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("preflight ACAO=%q, want echoed origin", got)
	}
	if rec.Body.String() == "proxied" {
		t.Fatal("preflight reached the proxy, want short-circuit at CORS")
	}
}

// A real (non-preflight) proxied request from an allowed origin gets the ACAO
// header on the response so the browser can read it.
func TestProxyCORSHeaderOnValidRequest(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub(), nil, nil, []string{"https://app.example"})
	req := httptest.NewRequest(http.MethodGet, "/v1/dnt/table", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO=%q, want echoed origin", got)
	}
}

// A proxied request from a NON-allowlisted origin still works for non-browser
// callers but carries no ACAO header (a browser would then block reading it).
func TestProxyNoCORSForDisallowedOrigin(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub(), nil, nil, []string{"https://app.example"})
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO=%q set for disallowed origin, want empty", got)
	}
}

// /v1/services is the permissionless surface, served by the catalogue handler.
func TestServicesCatalogueIsPublic(t *testing.T) {
	catalogue := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel proving we reached the catalogue
	})
	h := New(stubValidator{valid: false}, proxyStub(), nil, catalogue, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code=%d, want 418 (catalogue reached without a key)", rec.Code)
	}
}

// The raw node table and everything that spends GPU time stay behind the key.
func TestOnlyTheCatalogueIsPublic(t *testing.T) {
	catalogue := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := New(stubValidator{valid: false}, proxyStub(), nil, catalogue, nil)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/dnt/table"},
		{http.MethodGet, "/v1/dnt/peers"},
		{http.MethodPost, "/v1/services"},
		{http.MethodGet, "/v1/service/llm/v1/models"},
		{http.MethodPost, "/v1/service/llm/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: code=%d, want 401 (still key-gated)", c.method, c.path, rec.Code)
		}
	}
}

// With no catalogue wired, /v1/services must not become an open passthrough.
func TestServicesGatedWhenCatalogueAbsent(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/services", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

// A public route still needs CORS headers so a browser can read the response.
func TestPublicCatalogueCarriesCORS(t *testing.T) {
	catalogue := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := New(stubValidator{valid: false}, proxyStub(), nil, catalogue, []string{"https://app.example"})

	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO=%q, want echoed origin", got)
	}
}
