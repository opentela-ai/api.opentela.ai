package perf

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PeerHeader is stamped by the mesh head on every routed response.
const PeerHeader = "X-Computing-Node"

const routePrefix = "/v1/service/"

// SettleUsage is the parsed token accounting handed to a settlement
// callback at body end, after the perf sample is observed. Unlike Sample it
// carries the RAW served peer id (from X-Computing-Node) so billing can
// resolve it against the immutable quote snapshot — perf itself never
// persists raw peer ids.
type SettleUsage struct {
	Service           string
	Route             string
	Model             string
	ServedPeerID      string // raw X-Computing-Node, "" when the mesh didn't stamp one
	Status            int
	ClientAbort       bool
	InputTokens       int // billable regular input after dialect normalization
	CachedInputTokens int
	OutputTokens      int
	Complete          bool // usage had a final, authoritative token count
}

// Hook returns a reverse-proxy ModifyResponse step that swaps the response
// body for a measuring wrapper and queues a Sample when the body ends. Only
// responses stamped by the mesh (X-Computing-Node) on inference service
// routes are sampled; everything else (local endpoints, failures before
// routing, health checks) passes through untouched and unrecorded.
//
// The hook runs after any body-rewriting steps in the proxy chain, so the
// measurements reflect the stream the client actually receives. Streams are
// never buffered; per-request cost is bounded by the parser's line buffer.
func Hook(rec Recorder, gpus *Resolver) func(*http.Response) error {
	return HookWithSettle(rec, gpus, nil)
}

// HookWithSettle is Hook plus an optional settlement callback invoked at body
// end, after the perf Sample is observed, with the same parser state — no
// second parse. settle may be nil; when non-nil it runs even for non-2xx,
// aborted, or usage-less responses so the caller can release the reservation.
// The perf package does not depend on billing: the caller converts SettleUsage
// into its own settlement types.
func HookWithSettle(rec Recorder, gpus *Resolver, settle func(resp *http.Response, u SettleUsage)) func(*http.Response) error {
	return func(resp *http.Response) error {
		peerID := resp.Header.Get(PeerHeader)
		service, route := splitServiceRoute(requestPath(resp))
		if peerID == "" || service == "" || resp.Body == nil {
			return nil
		}

		base := Sample{
			TS:      time.Now(),
			Service: service,
			Route:   route,
			PeerFP:  fingerprint(peerID),
			Status:  resp.StatusCode,
			GPUMs:   headerInt64(resp.Header, "X-Usage-Gpu-Ms"),
		}
		if gpus != nil {
			info := gpus.Resolve(resp.Request.Context(), peerID)
			base.GPUModel = info.Model
			base.GPUCount = info.Count
		}

		// Compressed bodies (unusual — the transport already transparently
		// decodes what it negotiated) aren't token-parsed: timings still hold.
		contentType := resp.Header.Get("Content-Type")
		if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			contentType = ""
		}

		// Anchor the clock at request-write when the proxy instrumented the
		// outbound request (see trace.go): for non-streaming responses the
		// whole generation window sits between request-write and ModifyResponse.
		start := clockStart(resp.Request, time.Now())
		resp.Body = wrapBody(resp.Body, contentType, start, func(m *measureBody) {
			s := base
			s.Model = m.parser.model
			s.ClientAbort = !m.eof
			s.TTFTMs = durationMs(m.ttft)
			if m.firstToken && m.parser.kind == parserSSE {
				s.FirstTokenMs = durationMs(m.firstTok)
			}
			s.TotalMs = durationMs(m.total())
			s.InputTokens = m.parser.inputTokens
			s.CachedInputTokens = m.parser.cachedInputTokens
			s.OutputTokens = m.parser.outputTokens
			s.ResponseBytes = m.bytes
			rec.Observe(s)
			if settle != nil {
				settle(resp, SettleUsage{
					Service:           s.Service,
					Route:             s.Route,
					Model:             s.Model,
					ServedPeerID:      peerID,
					Status:            s.Status,
					ClientAbort:       s.ClientAbort,
					InputTokens:       m.parser.settleInputTokens(),
					CachedInputTokens: s.CachedInputTokens,
					OutputTokens:      s.OutputTokens,
					// A usage record is complete when the response carried final
					// token counts (2xx with usage), not an abort or empty parse.
					Complete: s.Status >= 200 && s.Status < 300 &&
						!s.ClientAbort && (s.InputTokens > 0 || s.CachedInputTokens > 0 || s.OutputTokens > 0),
				})
			}
		})
		return nil
	}
}

// splitServiceRoute maps "/v1/service/{svc}/v1/chat/completions" to
// ("{svc}", "chat/completions"). The route is capped so a pathological path
// cannot mint arbitrary LowCardinality values in ClickHouse.
func splitServiceRoute(path string) (service, route string) {
	rest, ok := strings.CutPrefix(path, routePrefix)
	if !ok {
		return "", ""
	}
	svc, tail, ok := strings.Cut(rest, "/")
	if !ok || svc == "" {
		return "", ""
	}
	route = strings.TrimPrefix(tail, "v1/")
	if len(route) > 48 {
		route = route[:48]
	}
	return svc, route
}

func requestPath(resp *http.Response) string {
	if resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.Path
}

// fingerprint irreversibly anonymizes a peer id for provider counting.
// 16 hex chars (64 bits) keeps collisions negligible at mesh scale.
func fingerprint(peerID string) string {
	sum := sha256.Sum256([]byte(peerID))
	return hex.EncodeToString(sum[:])[:16]
}

func headerInt64(h http.Header, name string) int64 {
	v, err := strconv.ParseInt(h.Get(name), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func durationMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
