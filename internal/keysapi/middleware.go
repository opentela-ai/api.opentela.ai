// Package keysapi is the HTTP surface for user-owned key management: a JWT-gated
// set of JSON endpoints under /manage/keys, plus their CORS handling.
package keysapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/opentela-ai/api/internal/neonauth"
)

// Verifier verifies a raw bearer JWT and returns its claims.
type Verifier interface {
	Verify(ctx context.Context, raw string) (neonauth.Claims, error)
}

type ctxKey int

const userIDKey ctxKey = 0

const bearerPrefix = "bearer "

// UserID returns the authenticated subject placed in ctx by Middleware.
func UserID(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(userIDKey).(string)
	return s, ok && s != ""
}

// Middleware requires a valid Neon Auth JWT and stores its subject in the request
// context. Missing/malformed/invalid tokens get 401. (This plane is deliberately
// independent of the proxy plane's Bearer parsing.)
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := v.Verify(r.Context(), token)
			if err != nil || claims.Subject == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, claims.Subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	return token, token != ""
}

// CORS applies a minimal allowlist policy to the management plane. With an empty
// list it is a pass-through (no CORS headers). Preflight OPTIONS is answered 204
// before any downstream auth runs.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
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
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
