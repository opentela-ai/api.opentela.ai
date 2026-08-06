// Package proxy builds a streaming reverse proxy to the opentela upstream.
package proxy

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/opentela-ai/api/internal/perf"
	"github.com/opentela-ai/api/internal/thinkfix"
)

// corsResponseHeaders are set by this service's own CORS middleware. The
// upstream sets its own (more permissive) copies; both would be written to the
// client, and a response carrying two different Access-Control-Allow-Origin
// values is rejected outright by browsers. Strip the upstream's so only this
// service's allowlist policy reaches the client.
var corsResponseHeaders = []string{
	"Access-Control-Allow-Origin",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers",
	"Access-Control-Allow-Credentials",
	"Access-Control-Expose-Headers",
	"Access-Control-Max-Age",
}

// New returns a reverse proxy that forwards every request to target, preserving
// method, path, query, headers, and body. Responses stream back immediately
// (FlushInterval -1), which matters for opentela's SSE/LLM output. Upstream
// failures produce a 502.
//
// One deliberate exception to transparency: Anthropic Messages responses are
// scanned for leaked reasoning-section markers and rewritten into proper
// thinking blocks (internal/thinkfix). The rewrite is marker-triggered —
// marker-free responses (e.g. vLLM backends) pass through byte-for-byte.
func New(target *url.URL) *httputil.ReverseProxy {
	return NewWithPerfHook(target, nil)
}

// NewWithPerfHook is New plus an optional response hook (internal/perf),
// invoked last in the ModifyResponse chain — after CORS stripping and the
// thinkfix rewrite — so performance measurement observes the exact stream the
// client receives. The hook wraps the body in a measuring reader; it must not
// buffer. When the hook is installed the outbound request also carries an
// httptrace (perf.Instrument) anchoring the measurement clock at
// request-write, so non-streaming latency includes upstream generation.
func NewWithPerfHook(target *url.URL, perfHook func(*http.Response) error) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)  // routes to target scheme/host, joins base path, sets Host to target
			r.SetXForwarded() // sets X-Forwarded-For/Host/Proto
			if perfHook != nil {
				// Anchor the perf clock at request-write: otherwise
				// non-streaming latency vanishes when a worker's
				// headers and body arrive coalesced (perf.Instrument).
				r.Out = perf.Instrument(r.Out)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			for _, h := range corsResponseHeaders {
				resp.Header.Del(h)
			}
			if err := thinkfix.MaybeWrapResponse(resp); err != nil {
				return err
			}
			if perfHook != nil {
				return perfHook(resp)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy: upstream error for %s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
