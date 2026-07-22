// Package proxy builds a streaming reverse proxy to the opentela upstream.
package proxy

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// New returns a reverse proxy that forwards every request to target, preserving
// method, path, query, headers, and body. Responses stream back immediately
// (FlushInterval -1), which matters for opentela's SSE/LLM output. Upstream
// failures produce a 502.
func New(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)  // routes to target scheme/host, joins base path, sets Host to target
			r.SetXForwarded() // sets X-Forwarded-For/Host/Proto
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy: upstream error for %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
