package manageapi

import (
	"context"
	"net/http"

	"github.com/opentela-ai/api/internal/corsmw"
	"github.com/opentela-ai/api/internal/keysapi"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

type WalletRoutes interface{ Routes() http.Handler }
type InstanceRoutes interface{ Routes() http.Handler }
type RegionRoutes interface{ Routes() http.Handler }
type FaucetRoutes interface{ Routes() http.Handler }
type BillingRoutes interface{ Routes() http.Handler }
type DeployKeyRoutes interface{ ManageRoutes() http.Handler }

type identityStore interface {
	RefreshIdentity(ctx context.Context, in store.IdentityInfo) error
}

type identityRefresher struct{ store identityStore }

func (r identityRefresher) RefreshIdentity(ctx context.Context, p principal.Principal) error {
	return r.store.RefreshIdentity(ctx, store.IdentityInfo{
		AccountID:      p.Subject,
		Email:          p.Email,
		EmailDomain:    p.EmailDomain,
		EmailVerified:  p.EmailVerified,
		LastVerifiedAt: p.VerifiedAt,
	})
}

func Router(keySvc keysapi.Service, wallets WalletRoutes, instances InstanceRoutes, regions RegionRoutes, faucetRoutes FaucetRoutes, billingRoutes BillingRoutes, deployKeys DeployKeyRoutes, v principal.Verifier, pg identityStore, corsOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/manage/keys", keysapi.Routes(keySvc))
	mux.Handle("/manage/keys/", keysapi.Routes(keySvc))
	if deployKeys != nil {
		h := deployKeys.ManageRoutes()
		mux.Handle("/manage/deploy-keys", h)
		mux.Handle("/manage/deploy-keys/", h)
	}
	if wallets != nil {
		h := wallets.Routes()
		mux.Handle("/manage/wallets", h)
		mux.Handle("/manage/wallets/", h)
	}
	if instances != nil {
		h := instances.Routes()
		mux.Handle("/manage/instances", h)
		mux.Handle("/manage/instances/", h)
	}
	if regions != nil {
		h := regions.Routes()
		mux.Handle("/manage/regions", h)
		mux.Handle("/manage/regions/", h)
	}
	if faucetRoutes != nil {
		h := faucetRoutes.Routes()
		mux.Handle("/manage/faucet", h)
		mux.Handle("/manage/faucet/", h)
	}
	if billingRoutes != nil {
		h := billingRoutes.Routes()
		mux.Handle("/manage/billing", h)
		mux.Handle("/manage/billing/", h)
	}
	return corsmw.Middleware(corsOrigins)(principal.Middleware(v, identityRefresher{store: pg}, nil)(mux))
}
