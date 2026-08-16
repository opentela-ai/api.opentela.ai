// Package account carries the API-key owner resolved on the request path.
//
// The billing gate reads the owning account id from request context to charge
// the right buyer. It is established by auth.Middleware after key validation
// and is intentionally separate from the principal package's Neon-JWT
// identity: API-key requests have no JWT, and the account id (api_keys.user_id)
// is the billing identity for the hot path.
package account

import "context"

type ctxKey struct{}

// WithID returns ctx carrying the owning account id. An empty id is stored as
// absent so downstream callers can rely on ID returning ok=false for legacy
// keys that carry no user_id.
func WithID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// ID returns the owning account id established on the request path, and
// whether one is present.
func ID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKey{}).(string)
	return v, ok && v != ""
}
