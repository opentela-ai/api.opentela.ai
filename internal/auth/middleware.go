package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/opentela-ai/api/internal/account"
)

const bearerPrefix = "bearer "

// Options configures Middleware. The zero value preserves the historical
// behavior: any valid key passes, and no account identity is required.
type Options struct {
	// EnforceAccount rejects active legacy keys — those that carry no owning
	// account id (api_keys.user_id IS NULL) — with 402 billing_account_required
	// before they reach a billing-gated route. It is set when BILLING_MODE=enforce.
	// In observe/off modes a legacy key passes with no account in context, so
	// the gate never acts on it.
	EnforceAccount bool
}

// TokenValidator reports whether a plaintext bearer token is valid and, when it
// is, the owning account id. accountID is empty for legacy keys; callers use
// Options.EnforceAccount to reject them.
type TokenValidator interface {
	Valid(ctx context.Context, token string) (accountID string, ok bool, err error)
}

// Middleware returns net/http middleware that requires a valid API key. The
// key may arrive as "Authorization: Bearer <key>" (OpenAI style) or as
// "x-api-key: <key>" (Anthropic style, sent by Claude Code and Anthropic SDK
// clients). On success it calls next; otherwise it writes 401
// (missing/malformed/invalid), 402 (a valid legacy key while EnforceAccount is
// on), or 503 (store error).
//
// When validation succeeds the owning account id is placed in request context
// (account.WithID); downstream handlers read it via account.ID. A legacy key
// carries no account id, so account.ID returns (\"\", false) for it.
func Middleware(v TokenValidator, opts ...Options) func(http.Handler) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := credential(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			accountID, valid, err := v.Valid(r.Context(), token)
			if err != nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			if !valid {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if o.EnforceAccount && accountID == "" {
				http.Error(w, "billing_account_required", http.StatusPaymentRequired)
				return
			}
			ctx := account.WithID(r.Context(), accountID)
			next.ServeHTTP(w, r.WithContext(ctx))
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
