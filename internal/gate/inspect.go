// Package gate inspects inference requests on the billing path: it extracts
// the service and route from the path and the model, streaming mode, and
// output-token ceiling from a bounded prefix of the JSON body, then restores
// the body byte-for-byte so the proxy forwards exactly what the client sent.
//
// The gate (Step 3) uses a Plan to size a conservative reservation and to
// reject requests the meter cannot price. Inspect is pure and allocation-bounded:
// it never reads more than InspectCap bytes of the body for parsing, and only
// drains the whole body (up to MaxBody) when Content-Length is unknown and the
// input-token ceiling must be measured.
package gate

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	// InspectCap bounds the body bytes parsed for model/streaming/max-tokens.
	// JSON request bodies carry these fields early; past the cap the body still
	// passes through untouched and Inspect degrades to path + content-length
	// (no model, no output ceiling).
	InspectCap = 1 << 20 // 1 MiB
	// MaxBody bounds a full-body drain when Content-Length is unknown, so an
	// unbounded chunked upload cannot exhaust the gate.
	MaxBody = 64 << 20 // 64 MiB
)

// maxBody is overridable in tests (see inspect_test.go) so the unbounded-body
// case does not allocate and read the full production ceiling.
var maxBody = MaxBody

// Route is the portion of the path after /v1/service/{service}/v1/, capped so
// a pathological path cannot mint arbitrary values downstream.
type Route string

// A plan describes what the gate can bill about a request. Supported is false
// when the route is not token-metered (e.g. GET .../models) or the meter
// cannot bound the charge (generative route with no output ceiling); the gate
// rejects such requests with 400 billing_unsupported_route while enforcement
// is on.
type Plan struct {
	Service       string // "{service}" from /v1/service/{service}/v1/...
	Route         string // e.g. "chat/completions"
	Model         string // from the JSON body (best effort)
	Streaming     bool   // body requested a stream (OpenAI stream / Anthropic stream)
	IncludeUsage  bool   // OpenAI streaming set stream_options.include_usage=true
	InputCeiling  int    // conservative upper bound on input tokens (body byte length)
	OutputCeiling int    // conservative upper bound on output tokens (<= configured max)
	Generative    bool   // route produces generated tokens (reserves output)
	Supported     bool   // route is meterable and the charge is bounded
}

// Options configures Inspect. OutputMax is the operator's per-request
// output-token cap (BILLING_OUTPUT_TOKEN_MAX); a generative request with no
// body max_tokens and no OutputMax is unsupported.
type Options struct {
	OutputMax int
}

var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Inspect extracts a Plan from req, reading a bounded prefix of the body for
// JSON fields and restoring it byte-for-byte. It never returns an error for a
// malformed or oversized body: instead the Plan degrades (no model, no output
// ceiling) so the gate can decide whether to reject.
//
// The returned request is req with its body replaced by a reader yielding the
// original bytes in order; Content-Length is left untouched so the proxy
// forwards the exact payload.
func Inspect(req *http.Request, opts Options) (Plan, *http.Request) {
	p := Plan{Service: serviceOf(req.URL.Path), Route: routeOf(req.URL.Path)}
	p.Generative, p.Supported = classifyRoute(p.Route, opts)
	inspectBody(req, &p, opts)
	return p, req
}

// serviceOf / routeOf mirror perf.splitServiceRoute but live here so the gate
// does not depend on the response-side parser package. "/v1/service/llm/v1/
// chat/completions" → ("llm", "chat/completions").
func serviceOf(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1/service/")
	if !ok {
		return ""
	}
	svc, _, ok := strings.Cut(rest, "/")
	if !ok || svc == "" {
		return ""
	}
	return svc
}

func routeOf(path string) string {
	rest, ok := strings.CutPrefix(path, "/v1/service/")
	if !ok {
		return ""
	}
	if _, tail, ok := strings.Cut(rest, "/"); ok {
		route := strings.TrimPrefix(tail, "v1/")
		if len(route) > 48 {
			route = route[:48]
		}
		return route
	}
	return ""
}

// generativeRoutes produce output tokens that the meter must reserve.
var generativeRoutes = map[string]bool{
	"chat/completions": true,
	"completions":      true,
	"messages":         true, // Anthropic
	"responses":        true, // OpenAI Responses
}

// classifyRoute reports whether the route is generative and whether it is a
// meterable inference route at all. Non-inference routes (e.g. "models") are
// not supported. Supported is provisional here; inspectBody may clear it when
// a generative route has no bounded output ceiling.
func classifyRoute(route string, opts Options) (generative, supported bool) {
	if route == "" {
		return false, false
	}
	if !generativeRoutes[route] {
		// Embeddings and other input-only routes are metered but produce no
		// generated output (output ceiling 0). Only known inference routes are
		// supported; anything else is rejected as unsupported_route.
		switch route {
		case "embeddings":
			return false, true
		}
		return false, false
	}
	return true, true
}

// bodyShape is the subset of a request body Inspect reads. Unknown fields —
// the actual prompt! — are skipped by the streaming decoder.
type bodyShape struct {
	Model           string
	Streaming       bool
	IncludeUsage    bool
	MaxTokens       *int
	MaxCompletion   *int
	MaxOutputTokens *int
}

func inspectBody(req *http.Request, p *Plan, opts Options) {
	if req.Body == nil {
		// No body: an input ceiling is still derivable from Content-Length,
		// and a generative route without an output ceiling is unsupported.
		p.InputCeiling = ceilingFromLength(req.ContentLength)
		p.OutputCeiling = resolveOutputCeiling(p.Generative, nil, nil, nil, opts)
		if p.Generative && p.OutputCeiling == 0 {
			p.Supported = false
		}
		return
	}

	// Read up to InspectCap bytes for JSON fields. If Content-Length is known
	// we stop at min(cap, len); if unknown we may need the full body for the
	// input ceiling (drained below).
	limit := InspectCap
	if req.ContentLength >= 0 && req.ContentLength < int64(limit) {
		limit = int(req.ContentLength)
	}
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	n, _ := io.CopyN(buf, req.Body, int64(limit))
	shape := decodeShape(buf.Bytes())
	p.Model = shape.Model
	p.Streaming = shape.Streaming
	p.IncludeUsage = shape.IncludeUsage

	// Input ceiling. Prefer Content-Length (no further reading needed). When
	// unknown (chunked), drain the rest of the body up to MaxBody so the
	// ceiling reflects the true size; the bytes already in buf count first.
	switch {
	case req.ContentLength >= 0 && req.ContentLength <= int64(maxBody):
		p.InputCeiling = int(req.ContentLength)
	case req.ContentLength > int64(maxBody):
		// Larger than the gate is willing to bound.
		p.InputCeiling = 0
		p.Supported = false
	default: // Content-Length unknown: drain the remainder.
		rest := new(bytes.Buffer)
		m, copyErr := io.CopyN(rest, req.Body, int64(maxBody)-int64(n))
		buf.Write(rest.Bytes()) // buf now holds the whole body, in order
		switch {
		case copyErr == io.EOF:
			// Reached the end inside maxBody → buf.Len() is the true size.
			p.InputCeiling = buf.Len()
		default:
			// Either exactly maxBody (ambiguous) or CopyN was cut off. Treat
			// a body that filled the budget without EOF as unbounded: the gate
			// cannot prove the true size, so it rejects the request.
			p.InputCeiling = maxBody
			p.Supported = false
		}
		_ = m
	}

	p.OutputCeiling = resolveOutputCeiling(p.Generative, shape.MaxTokens, shape.MaxCompletion, shape.MaxOutputTokens, opts)
	if p.Generative && p.OutputCeiling == 0 {
		p.Supported = false
	}

	// When the gate can bound output, drain the full body and clamp the
	// request's max-tokens fields down to the ceiling so the upstream cannot
	// generate past the reservation. Without this, a client that sends
	// max_tokens=100000 against an operator cap of 4096 would cause the
	// upstream to generate 100000 tokens while the gate reserved only 4096.
	// When the gate cannot bound output (unsupported, or input-only route)
	// the body is restored byte-for-byte.
	if p.Supported && p.Generative && p.OutputCeiling > 0 {
		// Drain whatever is still unread; the prefix is already in buf. In
		// the supported case the body fits in MaxBody.
		if req.ContentLength < 0 || req.ContentLength > int64(buf.Len()) {
			_, _ = io.CopyN(buf, req.Body, int64(maxBody)-int64(buf.Len()))
		}
		if clamped, ok := rewriteOutputCeiling(buf.Bytes(), p.Route, p.OutputCeiling); ok {
			req.Body = io.NopCloser(bytes.NewReader(clamped))
			req.ContentLength = int64(len(clamped))
			req.Header.Set("Content-Length", strconv.Itoa(len(clamped)))
		} else {
			// No max field exceeded the ceiling (or the body wasn't JSON):
			// forward the drained body verbatim.
			req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
			req.ContentLength = int64(buf.Len())
			req.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
		}
		return
	}

	// Restore the body byte-for-byte: the captured prefix (and any drained
	// remainder) followed by whatever is still unread. Content-Length is left
	// as-is so the proxy forwards the original payload size.
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), req.Body))
}

// maxFieldKeys are the output-token ceiling fields the gate recognizes
// across OpenAI chat/completions (max_tokens, max_completion_tokens) and the
// Responses API (max_output_tokens).
var maxFieldKeys = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// rewriteOutputCeiling rewrites any max-tokens field in body that exceeds
// ceiling and injects a route-compatible field when the operator cap is the
// only usable output bound. It preserves every other field's value. ok=true
// means the body must be forwarded in rewritten form; otherwise the caller can
// keep the original bytes verbatim.
//
// Re-marshaling reorders keys (Go sorts map keys) but never alters another
// field's value, so the request remains valid JSON for any provider.
func rewriteOutputCeiling(body []byte, route string, ceiling int) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	rewritten := false
	hasPositiveBound := false
	for _, key := range maxFieldKeys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var cur int
		if err := json.Unmarshal(raw, &cur); err != nil {
			continue // non-integer value; leave untouched
		}
		if cur > 0 {
			hasPositiveBound = true
		}
		if cur > ceiling {
			m[key] = json.RawMessage(strconv.Itoa(ceiling))
			rewritten = true
		}
	}
	if !hasPositiveBound {
		m[maxFieldForRoute(route)] = json.RawMessage(strconv.Itoa(ceiling))
		rewritten = true
	}
	if !rewritten {
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

func maxFieldForRoute(route string) string {
	if route == "responses" {
		return "max_output_tokens"
	}
	return "max_tokens"
}

// resolveOutputCeiling picks the conservative output bound: the smallest of
// the request's stated max and the operator's cap, with zero meaning unknown.
func resolveOutputCeiling(generative bool, maxTokens, maxCompletion, maxOutput *int, opts Options) int {
	if !generative {
		return 0 // input-only route; output ceiling is irrelevant.
	}
	candidates := []int{}
	for _, m := range []*int{maxTokens, maxCompletion, maxOutput} {
		if m != nil && *m > 0 {
			candidates = append(candidates, *m)
		}
	}
	if opts.OutputMax > 0 {
		candidates = append(candidates, opts.OutputMax)
	}
	if len(candidates) == 0 {
		return 0
	}
	ceiling := candidates[0]
	for _, c := range candidates[1:] {
		if c < ceiling {
			ceiling = c
		}
	}
	return ceiling
}

// ceilingFromLength maps a Content-Length to an input-token ceiling. A length
// of -1 (unknown) yields 0, signalling the caller that the body must be drained
// (or the request rejected).
func ceilingFromLength(length int64) int {
	if length < 0 || length > int64(maxBody) {
		return 0
	}
	return int(length)
}

// decodeShape reads model/streaming/max-tokens from a prefix of a JSON
// request body using a streaming decoder, so the early fields are recovered
// even when the body is larger than InspectCap (only the prefix is buffered).
// A truncated or malformed body degrades gracefully: whatever was decoded
// before the error is kept.
func decodeShape(b []byte) bodyShape {
	var s bodyShape
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return s
	}
	for {
		key, err := dec.Token()
		if err != nil || key == json.Delim('}') {
			return s
		}
		k, ok := key.(string)
		if !ok {
			return s
		}
		switch k {
		case "model":
			if v, err := dec.Token(); err == nil {
				if str, ok := v.(string); ok {
					s.Model = str
				}
			}
		case "stream":
			if v, err := dec.Token(); err == nil {
				if b, ok := v.(bool); ok {
					s.Streaming = b
				}
			}
		case "max_tokens":
			s.MaxTokens = readIntPtr(dec)
		case "max_completion_tokens":
			s.MaxCompletion = readIntPtr(dec)
		case "max_output_tokens":
			s.MaxOutputTokens = readIntPtr(dec)
		case "stream_options":
			s.IncludeUsage = readIncludeUsage(dec)
		default:
			skipValue(dec)
		}
	}
}

// readIntPtr reads the next token as a positive integer pointer, or nil on any
// mismatch. A JSON number is accepted; other shapes leave the pointer nil.
func readIntPtr(dec *json.Decoder) *int {
	v, err := dec.Token()
	if err != nil {
		return nil
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err == nil && i > 0 {
			ii := int(i)
			return &ii
		}
	case float64:
		if n > 0 {
			i := int(n)
			return &i
		}
	}
	return nil
}

// readIncludeUsage scans a stream_options object for include_usage.
func readIncludeUsage(dec *json.Decoder) bool {
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return false
	}
	for {
		key, err := dec.Token()
		if err != nil || key == json.Delim('}') {
			return false
		}
		k, ok := key.(string)
		if !ok {
			return false
		}
		if k == "include_usage" {
			if v, err := dec.Token(); err == nil {
				if b, ok := v.(bool); ok {
					return b
				}
			}
			return false
		}
		skipValue(dec)
	}
}

// skipValue consumes one JSON value (scalar, object, or array) at the current
// decoder position so an unknown field does not derail field extraction.
func skipValue(dec *json.Decoder) {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
			if depth <= 0 {
				return
			}
		default:
			if depth == 0 {
				return // scalar consumed
			}
		}
	}
}
