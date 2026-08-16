package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/account"
)

type stubValidator struct {
	valid     bool
	err       error
	accountID string
	gotToken  string
}

func (s *stubValidator) Valid(_ context.Context, token string) (string, bool, error) {
	s.gotToken = token
	if !s.valid {
		return "", false, s.err
	}
	return s.accountID, true, s.err
}

func nextOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("passed"))
	})
}

func doRequest(t *testing.T, v TokenValidator, header string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if header != "" {
		headers["Authorization"] = header
	}
	return doRequestHeaders(t, v, headers)
}

func doRequestHeaders(t *testing.T, v TokenValidator, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h := Middleware(v)(nextOK())
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareMissingHeader(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareMalformedHeader(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "Basic abc")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareInvalidKey(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: false}, "Bearer nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareStoreError(t *testing.T) {
	rec := doRequest(t, &stubValidator{err: errors.New("db down")}, "Bearer x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestMiddlewareValidPassesToken(t *testing.T) {
	sv := &stubValidator{valid: true}
	rec := doRequest(t, sv, "Bearer secret-token")
	if rec.Code != http.StatusOK || rec.Body.String() != "passed" {
		t.Fatalf("code=%d body=%q, want 200/passed", rec.Code, rec.Body.String())
	}
	if sv.gotToken != "secret-token" {
		t.Fatalf("validator got token %q, want secret-token", sv.gotToken)
	}
}

func TestMiddlewareSchemeCaseInsensitive(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "BEARER secret-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestMiddlewareEmptyToken(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "Bearer ")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// Anthropic-style credential: Claude Code and the Anthropic SDK send the key
// as x-api-key (from ANTHROPIC_API_KEY).
func TestMiddlewareXAPIKeyPassesToken(t *testing.T) {
	sv := &stubValidator{valid: true}
	rec := doRequestHeaders(t, sv, map[string]string{"X-Api-Key": "secret-token"})
	if rec.Code != http.StatusOK || rec.Body.String() != "passed" {
		t.Fatalf("code=%d body=%q, want 200/passed", rec.Code, rec.Body.String())
	}
	if sv.gotToken != "secret-token" {
		t.Fatalf("validator got token %q, want secret-token", sv.gotToken)
	}
}

func TestMiddlewareXAPIKeyInvalid(t *testing.T) {
	rec := doRequestHeaders(t, &stubValidator{valid: false}, map[string]string{"X-Api-Key": "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// An empty x-api-key — sent by Anthropic clients when only ANTHROPIC_AUTH_TOKEN
// is set — counts as absent, not as an empty credential.
func TestMiddlewareXAPIKeyEmpty(t *testing.T) {
	rec := doRequestHeaders(t, &stubValidator{valid: true}, map[string]string{"X-Api-Key": "  "})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// Claude Code sets both ANTHROPIC_AUTH_TOKEN and ANTHROPIC_API_KEY; the bearer
// token takes precedence and is the only credential validated.
func TestMiddlewareBearerTakesPrecedenceOverXAPIKey(t *testing.T) {
	sv := &stubValidator{valid: true}
	rec := doRequestHeaders(t, sv, map[string]string{
		"Authorization": "Bearer secret-token",
		"X-Api-Key":     "other-token",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if sv.gotToken != "secret-token" {
		t.Fatalf("validator got token %q, want secret-token (bearer wins)", sv.gotToken)
	}
}

// A non-bearer Authorization scheme is not a credential; a valid x-api-key on
// the same request still applies.
func TestMiddlewareNonBearerAuthorizationFallsBackToXAPIKey(t *testing.T) {
	sv := &stubValidator{valid: true}
	rec := doRequestHeaders(t, sv, map[string]string{
		"Authorization": "Basic abc",
		"X-Api-Key":     "secret-token",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if sv.gotToken != "secret-token" {
		t.Fatalf("validator got token %q, want secret-token", sv.gotToken)
	}
}

// doRequestWith runs Middleware with the given options and reports the body
// the downstream handler wrote, so tests can assert on the threaded account id.
func doRequestWith(t *testing.T, v TokenValidator, opts Options, headers map[string]string) (int, string) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := account.ID(r.Context())
		if ok {
			_, _ = w.Write([]byte("acct=" + id))
		} else {
			_, _ = w.Write([]byte("no-account"))
		}
	})
	h := Middleware(v, opts)(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader("{}"))
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// A key with an owning account is threaded into request context for the gate
// to charge, in every mode.
func TestMiddlewareThreadsAccountID(t *testing.T) {
	sv := &stubValidator{valid: true, accountID: "acct-7"}
	for _, opts := range []Options{{}, {EnforceAccount: true}} {
		code, body := doRequestWith(t, sv, opts, map[string]string{"Authorization": "Bearer t"})
		if code != http.StatusOK || body != "acct=acct-7" {
			t.Fatalf("(%+v) code=%d body=%q, want 200/acct=acct-7", opts, code, body)
		}
	}
}

// In enforcement mode an active legacy key (no user_id) is rejected with 402
// billing_account_required before it reaches a billing-gated route.
func TestMiddlewareEnforceRejectsLegacyKey(t *testing.T) {
	sv := &stubValidator{valid: true, accountID: ""}
	code, body := doRequestWith(t, sv, Options{EnforceAccount: true}, map[string]string{"Authorization": "Bearer t"})
	if code != http.StatusPaymentRequired || !strings.Contains(body, "billing_account_required") {
		t.Fatalf("code=%d body=%q, want 402/billing_account_required", code, body)
	}
}

// Outside enforcement a legacy key passes with no account in context, so the
// gate never charges it.
func TestMiddlewareObserveAllowsLegacyKey(t *testing.T) {
	sv := &stubValidator{valid: true, accountID: ""}
	code, body := doRequestWith(t, sv, Options{}, map[string]string{"Authorization": "Bearer t"})
	if code != http.StatusOK || body != "no-account" {
		t.Fatalf("code=%d body=%q, want 200/no-account", code, body)
	}
}
