package server

import (
	"context"
	"github.com/opentela-ai/api/internal/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubValidator struct {
	valid     bool
	accountID string
}

func (s stubValidator) Valid(context.Context, string) (string, bool, error) {
	if !s.valid {
		return "", false, nil
	}
	return s.accountID, true, nil
}

func proxyStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	})
}

// do issues a request with an optional Bearer key and returns the recorder.
func do(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
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

// GET /v1/leaderboard is permissionless like /v1/services: reachable without
// a key while neighboring inference routes stay gated.
func TestLeaderboardIsPermissionless(t *testing.T) {
	leader := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := NewWithInternal(stubValidator{valid: false}, proxyStub(), nil, nil, nil, leader, nil)
	if rec := do(h, http.MethodGet, "/v1/leaderboard", ""); rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated leaderboard code = %d, want 200", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/v1/service/x/v1/chat/completions", ""); rec.Code == http.StatusOK {
		t.Fatalf("inference route unexpectedly open")
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

// Claude Code / Anthropic SDK clients authenticate with x-api-key; the
// Anthropic Messages API endpoint must reach the proxy with it.
func TestProxyPassesWithXAPIKey(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub(), nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Api-Key", "good")
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
	h := NewWithInternal(stubValidator{valid: false}, proxyStub(), nil, internal, nil, nil, nil)

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
		{http.MethodPost, "/v1/messages"},
		{http.MethodPost, "/v1/messages/count_tokens"},
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

// GET /v1/service/{service}/v1/models is served locally by the catalogue
// (OpenAI-shaped list) behind the API key, never forwarded to the upstream.
// Inference traffic on the same prefix still proxies.
func TestModelsRouteServedLocallyAndGated(t *testing.T) {
	catalogue := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // reached the catalogue, not the proxy
	})
	h := New(stubValidator{valid: true}, proxyStub(), nil, catalogue, nil)

	// No key → gated, never reaches the catalogue or the proxy.
	if rec := do(h, http.MethodGet, "/v1/service/llm/v1/models", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key code=%d, want 401", rec.Code)
	}

	// Valid key → served by the catalogue (418), not forwarded ("proxied").
	rec := do(h, http.MethodGet, "/v1/service/llm/v1/models", "good")
	if rec.Code != http.StatusTeapot {
		t.Fatalf("valid-key code=%d body=%q, want 418 (catalogue served locally, not proxied)", rec.Code, rec.Body.String())
	}

	// Any other service on the same path shape is served locally too.
	if rec := do(h, http.MethodGet, "/v1/service/sandbox/v1/models", "good"); rec.Code != http.StatusTeapot {
		t.Fatalf("sandbox code=%d body=%q, want 418", rec.Code, rec.Body.String())
	}

	// A POST on the same prefix is NOT claimed by the GET-only route, so it
	// reaches the proxy for inference as before.
	if rec := do(h, http.MethodPost, "/v1/service/llm/v1/chat/completions", "good"); rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("post chat code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
}

// With no catalogue wired, GET /v1/service/{service}/v1/models falls through
// to the auth-gated proxy like everything else (no open passthrough).
func TestModelsRouteFallsThroughWhenCatalogueAbsent(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub(), nil, nil, nil)
	if rec := do(h, http.MethodGet, "/v1/service/llm/v1/models", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

// In enforce mode a valid legacy key (no owning account id) is rejected with
// 402 billing_account_required before reaching the proxy, while an account
// key forwards normally.
func TestEnforceRejectsLegacyKeyThroughProxy(t *testing.T) {
	h := NewWithControlPlanes(
		stubValidator{valid: true, accountID: ""}, proxyStub(),
		nil, nil, nil, nil, nil, nil, nil, nil,
		auth.Options{EnforceAccount: true},
	)
	rec := do(h, http.MethodPost, "/v1/service/llm/v1/chat/completions", "legacy")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("legacy key code = %d, want 402; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "billing_account_required") {
		t.Fatalf("body = %q, want billing_account_required", rec.Body.String())
	}

	// An account key forwards to the proxy.
	h2 := NewWithControlPlanes(
		stubValidator{valid: true, accountID: "acct-9"}, proxyStub(),
		nil, nil, nil, nil, nil, nil, nil, nil,
		auth.Options{EnforceAccount: true},
	)
	rec2 := do(h2, http.MethodPost, "/v1/service/llm/v1/chat/completions", "owned")
	if rec2.Code != http.StatusOK || rec2.Body.String() != "proxied" {
		t.Fatalf("owned key code=%d body=%q, want 200/proxied", rec2.Code, rec2.Body.String())
	}
}

// Off/observe (the default Options) never reject a legacy key; it forwards and
// the gate declines to charge because no account is in context.
func TestDefaultModeAllowsLegacyKeyThroughProxy(t *testing.T) {
	h := NewWithControlPlanes(
		stubValidator{valid: true, accountID: ""}, proxyStub(),
		nil, nil, nil, nil, nil, nil, nil, nil,
		auth.Options{},
	)
	rec := do(h, http.MethodPost, "/v1/service/llm/v1/chat/completions", "legacy")
	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("legacy key code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
}
