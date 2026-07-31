// Package corsmw provides a small allowlist CORS middleware shared by both HTTP
// planes: the JWT-gated management plane (/manage/keys*) and the API-key-gated
// proxy plane (/v1/* and everything else forwarded upstream). Browsers call both
// cross-origin, so both need identical CORS handling.
package corsmw

import (
	"net/http"
	"strings"
)

// Middleware applies an allowlist CORS policy.
//
// With an empty allowlist it adds no CORS headers — same-origin and non-browser
// (server-to-server, curl) clients are unaffected — but it still answers
// preflight OPTIONS with 204 so a browser preflight never falls through to the
// downstream auth handler (which would reject the credential-less preflight).
//
// When the request Origin is in the allowlist it is echoed back and the allowed
// methods/headers are advertised. Preflight is answered before next runs, so it
// bypasses any downstream authentication.
func Middleware(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				// X-Api-Key/Anthropic-Version/Anthropic-Beta let browser clients using
				// the Anthropic Messages API (x-api-key auth, required version header)
				// preflight like any other caller.
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, Anthropic-Version, Anthropic-Beta")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
