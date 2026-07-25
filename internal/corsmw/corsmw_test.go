package corsmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okNext() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("next"))
	})
}

func TestPreflightAnsweredWithoutHittingNext(t *testing.T) {
	h := Middleware([]string{"https://app.example"})(okNext())

	pre := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	pre.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("preflight reached next (body=%q), want short-circuit", rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("preflight ACAO=%q, want echoed origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("preflight missing Access-Control-Allow-Methods")
	}
}

func TestAllowedOriginEchoedOnSimpleRequest(t *testing.T) {
	h := Middleware([]string{"https://app.example"})(okNext())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "next" {
		t.Fatalf("code=%d body=%q, want 200/next (passed through to next)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO=%q, want echoed origin", got)
	}
}

func TestDisallowedOriginGetsNoACAO(t *testing.T) {
	h := Middleware([]string{"https://app.example"})(okNext())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO=%q set for disallowed origin, want empty", got)
	}
	if rec.Body.String() != "next" {
		t.Fatalf("body=%q, want next (still forwarded)", rec.Body.String())
	}
}

func TestEmptyAllowlistIsPassThroughButStillAnswersPreflight(t *testing.T) {
	h := Middleware(nil)(okNext())

	// No CORS headers on a normal request, and it still reaches next.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO=%q with empty allowlist, want empty", got)
	}
	if rec.Body.String() != "next" {
		t.Fatalf("body=%q, want next", rec.Body.String())
	}

	// Preflight is still short-circuited to 204 (never falls through to auth).
	pre := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("empty-allowlist preflight code=%d, want 204", rec.Code)
	}
}
