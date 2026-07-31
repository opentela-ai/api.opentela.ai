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

// Middleware returns net/http middleware that requires a valid API key. The
// key may arrive as "Authorization: Bearer <key>" (OpenAI style) or as
// "x-api-key: <key>" (Anthropic style, sent by Claude Code and Anthropic SDK
// clients). On success it calls next; otherwise it writes 401
// (missing/malformed/invalid) or 503 (store error).
func Middleware(v TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := credential(r)
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

// credential extracts the API key from the request. A well-formed bearer token
// wins; only when no bearer token is present does the Anthropic-style
// x-api-key header apply (an empty x-api-key — some Anthropic clients send one
// alongside a bearer token — counts as absent). Once a credential is selected
// it alone is validated: there is no falling back to the second credential
// when the first fails.
func credential(r *http.Request) (string, bool) {
	if token, ok := bearerToken(r.Header.Get("Authorization")); ok {
		return token, true
	}
	if key := strings.TrimSpace(r.Header.Get("X-Api-Key")); key != "" {
		return key, true
	}
	return "", false
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
