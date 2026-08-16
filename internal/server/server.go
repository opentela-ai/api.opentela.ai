// Package server assembles the HTTP routes: an open health check, a public
// model catalogue, and an authenticated catch-all that forwards to the proxy.
package server

import (
	"net/http"

	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/corsmw"
)

// New builds the top-level handler.
//
//   - /healthz is unauthenticated.
//   - /manage/ is the key-management plane (its own CORS + JWT auth), when non-nil.
//   - GET /v1/services is the permissionless surface: a distilled catalogue of
//     what the mesh serves, when catalog is non-nil. It is derived from the
//     upstream node table but carries only service names, their models and
//     provider counts — the raw table (operator wallet keys, peer addresses,
//     hardware) stays behind the API key like everything else.
//     Every other /v1 path, including anything under /v1/service/{service}/
//     that spends GPU time, stays behind the API-key middleware.
//   - GET /v1/leaderboard is the anonymized GPU performance leaderboard
//     (internal/leaderboard), when non-nil; permissionless for the same
//     abuse-screened reason as the catalogue.
//   - GET /v1/service/{service}/v1/models is the OpenAI-shaped model list for
//     one service, served from the same distilled table behind the API key so
//     OpenAI-compatible clients that hard-code GET {base}/models get a list
//     instead of the upstream's "no provider found" (the mesh route is for
//     inference, not listing).
//   - Everything else is the API-key-gated proxy.
//
// Both proxy planes are wrapped with CORS so browsers can call them
// cross-origin and their credential-less preflight OPTIONS bypasses auth.
// corsOrigins is the same allowlist the management plane uses.
func New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, catalog http.Handler, corsOrigins []string) http.Handler {
	return NewWithInternal(v, proxy, keyMgmt, nil, catalog, nil, corsOrigins)
}

func NewWithInternal(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, internalACL http.Handler, catalog http.Handler, leaderboard http.Handler, corsOrigins []string) http.Handler {
	return NewWithInternalOpts(v, proxy, keyMgmt, internalACL, catalog, leaderboard, corsOrigins, auth.Options{})
}

// NewWithInternalOpts is NewWithInternal plus billing options applied to the
// API-key middleware on every gated route.
func NewWithInternalOpts(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, internalACL http.Handler, catalog http.Handler, leaderboard http.Handler, corsOrigins []string, billing auth.Options) http.Handler {
	return NewWithControlPlanes(v, proxy, keyMgmt, internalACL, nil, nil, nil, catalog, leaderboard, corsOrigins, billing)
}

func NewWithControlPlanes(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, internalACLv1 http.Handler, internalACLv2 http.Handler, nodeChallenge http.Handler, nodeIssue http.Handler, catalog http.Handler, leaderboard http.Handler, corsOrigins []string, billing auth.Options) http.Handler {
	return NewWithBilling(v, proxy, keyMgmt, internalACLv1, internalACLv2, nodeChallenge, nodeIssue, catalog, leaderboard, corsOrigins, billing, nil, nil, nil, nil)
}

// NewWithBilling is NewWithControlPlanes plus the billing gate (which wraps
// the inference proxy when non-nil) and the seller-pricing route (POST
// /internal/pricing, when non-nil). billingGate must already wrap the proxy
// with the API-key middleware (the gate reads the owning account from
// context), so callers pass the gate in place of the raw proxy.
func NewWithBilling(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, internalACLv1 http.Handler, internalACLv2 http.Handler, nodeChallenge http.Handler, nodeIssue http.Handler, catalog http.Handler, leaderboard http.Handler, corsOrigins []string, billing auth.Options, billingGate http.Handler, pricing http.Handler, pricingChallenge http.Handler, pricingIssue http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if keyMgmt != nil {
		mux.Handle("/manage/", keyMgmt)
	}
	if internalACLv1 != nil {
		mux.Handle("/internal/acl/evaluate", internalACLv1)
	}
	if internalACLv2 != nil {
		mux.Handle("/internal/acl/evaluate-v2", internalACLv2)
	}
	if nodeChallenge != nil {
		mux.Handle("/internal/node-credentials/challenges", nodeChallenge)
	}
	if nodeIssue != nil {
		mux.Handle("/internal/node-credentials", nodeIssue)
	}
	if pricing != nil {
		mux.Handle("/internal/pricing", pricing)
	}
	if pricingChallenge != nil {
		mux.Handle("/internal/pricing/challenges", pricingChallenge)
	}
	if pricingIssue != nil {
		mux.Handle("/internal/pricing/issue", pricingIssue)
	}

	cors := corsmw.Middleware(corsOrigins)
	// Permissionless like /v1/services: the leaderboard serves aggregated,
	// anonymized performance only (no peer identity, no keys).
	if leaderboard != nil {
		mux.Handle("GET /v1/leaderboard", cors(leaderboard))
	}
	if catalog != nil {
		mux.Handle("GET /v1/services", cors(catalog))
		// OpenAI-compatible clients hard-code GET {base}/models. Serve the
		// catalogue as an OpenAI-shaped list for the service in the path
		// instead of forwarding to the upstream, which answers "no provider
		// found" — that route exists for inference, not listing. Like the rest
		// of the proxy plane it is API-key gated.
		mux.Handle("GET /v1/service/{service}/v1/models", cors(auth.Middleware(v, billing)(catalog)))
	}
	// The billing gate (non-nil only when BILLING_MODE != off) wraps the
	// proxy so it runs on every inference route before forwarding. It is
	// applied here rather than in the proxy package so the gate has no
	// dependency on the reverse-proxy internals.
	forward := proxy
	if billingGate != nil {
		forward = billingGate
	}
	mux.Handle("/", cors(auth.Middleware(v, billing)(forward)))
	return mux
}
