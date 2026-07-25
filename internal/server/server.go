// Package server assembles the HTTP routes: an open health check and an
// authenticated catch-all that forwards to the proxy.
package server

import (
	"net/http"

	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/corsmw"
)

// New builds the top-level handler. /healthz is unauthenticated. When keyMgmt is
// non-nil, /manage/ is served by it (its own CORS + JWT auth). Every other path
// is the API-key-gated proxy, wrapped with CORS so browsers can call /v1/*
// cross-origin and their credential-less preflight OPTIONS bypasses the API-key
// auth. corsOrigins is the same allowlist the management plane uses.
func New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler, corsOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if keyMgmt != nil {
		mux.Handle("/manage/", keyMgmt)
	}
	mux.Handle("/", corsmw.Middleware(corsOrigins)(auth.Middleware(v)(proxy)))
	return mux
}
