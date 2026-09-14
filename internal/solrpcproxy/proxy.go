// Package solrpcproxy exposes the API's Solana RPC connectivity to browsers.
//
// The public mainnet endpoints reject browser Origins (403), and a dedicated
// provider costs a key. The API server is a backend — it can talk to the RPC
// fine — so it proxies JSON-RPC for the browser dApps (tokens.opentela.ai,
// the console) at POST /solana-rpc with:
//
//   - a method allowlist: read calls plus transaction submission; heavy
//     indexing methods (getProgramAccounts, getLargestAccounts, …) and
//     requestAirdrop are excluded by default so the proxy cannot be turned
//     into a free chain-scanning or airdrop service,
//   - a request body cap and a small batch cap,
//   - a per-client-IP token bucket (unauthenticated endpoint).
//
// CORS itself is applied by the shared corsmw wrapper on the route, so the
// origin allowlist governs which sites may call it.
package solrpcproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultMethods is the allowlist used when SOLANA_RPC_PROXY_METHODS is unset:
// everything a wallet + Jupiter swap flow needs, nothing chain-wide-heavy.
var DefaultMethods = []string{
	"getAccountInfo",
	"getBalance",
	"getBlock",
	"getBlockTime",
	"getEpochInfo",
	"getFeeForMessage",
	"getGenesisHash",
	"getHealth",
	"getLatestBlockhash",
	"getMinimumBalanceForRentExemption",
	"getMultipleAccounts",
	"getSignatureStatuses",
	"getSignaturesForAddress",
	"getSlot",
	"getTokenAccountBalance",
	"getTokenAccountsByOwner",
	"getTransaction",
	"getVersion",
	"isBlockhashValid",
	"sendTransaction",
	"simulateTransaction",
}

const (
	// maxBody bounds a JSON-RPC request (sendTransaction payloads are ~1 KiB;
	// 1 MiB covers any sane batch many times over).
	maxBody = 1 << 20
	// maxBatch bounds JSON-RPC batch arrays; wallets send single calls,
	// batches of 10 are generous.
	maxBatch = 10
	// upstreamTimeout bounds one RPC round-trip.
	upstreamTimeout = 30 * time.Second
)

// rateLimiter is a tiny per-key token bucket. The endpoint is
// unauthenticated, so the client IP is the only identity available.
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rps       float64
	burst     int
	lastSweep time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*bucket), rps: rps, burst: burst, lastSweep: time.Now()}
}

// allow consumes one token for key; it creates the bucket on first sight and
// sweeps stale buckets roughly once per minute to bound memory.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.lastSeen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst)}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * l.rps
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Handler proxies allowed JSON-RPC calls to the configured upstream node.
type Handler struct {
	upstream *url.URL
	client   *http.Client
	allow    map[string]bool
	limiter  *rateLimiter
	now      func() time.Time
	logf     func(format string, args ...any)
}

// Option mutates a Handler at construction.
type Option func(*Handler)

// WithLogger installs a logger for upstream failures.
func WithLogger(logf func(string, ...any)) Option {
	return func(h *Handler) { h.logf = logf }
}

// WithRateLimit overrides the per-IP bucket (calls/second and burst).
func WithRateLimit(rps float64, burst int) Option {
	return func(h *Handler) { h.limiter = newRateLimiter(rps, burst) }
}

// WithClient overrides the upstream HTTP client (tests).
func WithClient(c *http.Client) Option {
	return func(h *Handler) { h.client = c }
}

// New returns a proxy to upstream, allowing exactly the named methods. An
// empty method list selects DefaultMethods.
func New(upstream *url.URL, methods []string, opts ...Option) *Handler {
	h := &Handler{
		upstream: upstream,
		client:   &http.Client{Timeout: upstreamTimeout},
		allow:    make(map[string]bool, len(methods)),
		limiter:  newRateLimiter(20, 40),
		now:      time.Now,
	}
	for _, m := range methods {
		h.allow[strings.TrimSpace(m)] = true
	}
	if len(h.allow) == 0 {
		for _, m := range DefaultMethods {
			h.allow[m] = true
		}
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// rpcRequest is one JSON-RPC 2.0 call. ID stays raw so responses echo it
// byte-for-byte (wallets match on it).
type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if h.limiter != nil && !h.limiter.allow(clientIP(r), h.now()) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Single call or batch — both are valid JSON-RPC. Every entry must pass
	// the allowlist; one denied method denies the whole request.
	var single rpcRequest
	if err := json.Unmarshal(body, &single); err == nil && single.Method != "" {
		if !h.allow[single.Method] {
			http.Error(w, "method not allowed: "+single.Method, http.StatusForbidden)
			return
		}
	} else {
		var batch []rpcRequest
		if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 {
			http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
			return
		}
		if len(batch) > maxBatch {
			http.Error(w, "batch too large", http.StatusBadRequest)
			return
		}
		for _, call := range batch {
			if !h.allow[call.Method] {
				http.Error(w, "method not allowed: "+call.Method, http.StatusForbidden)
				return
			}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.upstream.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "proxy misconfigured", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		if h.logf != nil {
			h.logf("solrpcproxy: upstream %s: %v", h.upstream.Host, err)
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// clientIP extracts the caller identity for the limiter. The server sits
// behind Cloudflare, so CF-Connecting-IP is authoritative when present.
func clientIP(r *http.Request) string {
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i > 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}
