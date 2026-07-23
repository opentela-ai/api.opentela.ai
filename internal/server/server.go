// Package server assembles the HTTP routes: an open health check and an
// authenticated catch-all that forwards to the proxy.
package server

import (
	"net/http"

	"github.com/opentela-ai/api/internal/auth"
)

// New builds the top-level handler. /healthz is unauthenticated. When keyMgmt is
// non-nil, /manage/ is served by it (its own JWT auth); every other path passes
// through the Bearer API-key middleware before reaching proxy.
func New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if keyMgmt != nil {
		mux.Handle("/manage/", keyMgmt)
	}
	mux.Handle("/", auth.Middleware(v)(proxy))
	return mux
}
