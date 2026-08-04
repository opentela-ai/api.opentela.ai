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

		start := time.Now()
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
			s.OutputTokens = m.parser.outputTokens
			s.ResponseBytes = m.bytes
			rec.Observe(s)
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
