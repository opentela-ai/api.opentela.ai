// Package server assembles the HTTP routes: an open health check and an
// authenticated catch-all that forwards to the proxy.
package server

import (
	"net/http"

	"github.com/opentela-ai/api/internal/auth"
)

// New builds the top-level handler. /healthz is unauthenticated; every other path
// passes through the Bearer auth middleware before reaching proxy.
func New(v auth.TokenValidator, proxy http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", auth.Middleware(v)(proxy))
	return mux
}
