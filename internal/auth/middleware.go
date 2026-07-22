package auth

import (
	"context"
	"net/http"
	"strings"
)

// TokenValidator reports whether a plaintext bearer token is valid.
type TokenValidator interface {
	Valid(ctx context.Context, token string) (bool, error)
}

const bearerPrefix = "bearer "

// Middleware returns net/http middleware that requires a valid
// "Authorization: Bearer <token>" header. On success it calls next; otherwise it
// writes 401 (missing/malformed/invalid) or 503 (store error).
func Middleware(v TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			valid, err := v.Valid(r.Context(), token)
			if err != nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			if !valid {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization header value, case-insensitive
// on the scheme. It returns ok=false for a missing scheme or empty token.
func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
