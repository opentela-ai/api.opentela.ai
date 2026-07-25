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
//   - Everything else is the API-key-gated proxy.
//
// Both proxy planes are wrapped with CORS so browsers can call them
// cross-origin and their credential-less preflight OPTIONS bypasses auth.
// corsOrigins is the same allowlist the management plane uses.
func New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, catalog http.Handler, corsOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if keyMgmt != nil {
		mux.Handle("/manage/", keyMgmt)
	}

	cors := corsmw.Middleware(corsOrigins)
	if catalog != nil {
		mux.Handle("GET /v1/services", cors(catalog))
	}
	mux.Handle("/", cors(auth.Middleware(v)(proxy)))
	return mux
}
