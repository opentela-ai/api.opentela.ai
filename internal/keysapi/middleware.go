// Package keysapi is the HTTP surface for user-owned key management: a JWT-gated
// set of JSON endpoints under /manage/keys. Cross-origin handling is applied by
// the shared corsmw middleware, wired in Router.
package keysapi

import (
	"context"
	"net/http"

	"github.com/opentela-ai/api/internal/principal"
)

type Verifier = principal.Verifier

func UserID(ctx context.Context) (string, bool) {
	return principal.UserID(ctx)
}

func Middleware(v Verifier) func(http.Handler) http.Handler {
	return principal.Middleware(v, nil, nil)
}
