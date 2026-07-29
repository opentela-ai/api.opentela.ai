package principal

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/identity"
	"github.com/opentela-ai/api/internal/neonauth"
)

type Principal struct {
	Subject       string
	Email         string
	EmailDomain   string
	EmailVerified bool
	VerifiedAt    time.Time
}

type Verifier interface {
	Verify(ctx context.Context, raw string) (neonauth.Claims, error)
}

type IdentityRefresher interface {
	RefreshIdentity(ctx context.Context, p Principal) error
}

type ctxKey int

const (
	valueKey     ctxKey = iota
	bearerPrefix        = "bearer "
)

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(valueKey).(Principal)
	return p, ok && p.Subject != ""
}

func UserID(ctx context.Context) (string, bool) {
	p, ok := FromContext(ctx)
	return p.Subject, ok
}

func Middleware(v Verifier, refresher IdentityRefresher, now func() time.Time) func(http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
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
			p := Principal{
				Subject:       claims.Subject,
				Email:         claims.Email,
				EmailVerified: claims.EmailVerified,
				VerifiedAt:    now().UTC(),
			}
			if claims.EmailVerified {
				if domain, ok := identity.DomainFromVerifiedEmail(claims.Email); ok {
					p.EmailDomain = domain
				} else {
					p.EmailVerified = false
				}
			}
			if refresher != nil {
				if err := refresher.RefreshIdentity(r.Context(), p); err != nil {
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
					return
				}
			}
			ctx := context.WithValue(r.Context(), valueKey, p)
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
