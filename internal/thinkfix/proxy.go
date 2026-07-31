package thinkfix

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// MaybeWrapResponse conditionally rewrites a proxied response for reasoning
// marker translation. It applies only to successful Anthropic Messages
// responses:
//
//   - POST …/v1/messages (the token-count endpoint and everything else are
//     excluded by the path suffix),
//   - 200 OK,
//   - uncompressed (the proxy cannot scan an opaque gzip/brotli body),
//   - Content-Type text/event-stream (streaming) or application/json.
//
// Everything else — including every response that simply contains no leaked
// markers — is delivered exactly as the upstream sent it.
func MaybeWrapResponse(resp *http.Response) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	req := resp.Request
	if req.Method != http.MethodPost ||
		!strings.HasSuffix(req.URL.Path, "/v1/messages") ||
		resp.StatusCode != http.StatusOK {
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil
	}

	switch ct := resp.Header.Get("Content-Type"); {
	case strings.HasPrefix(ct, "text/event-stream"):
		resp.Body = struct {
			io.Reader
			io.Closer
		}{NewAnthropicSSERewriter(resp.Body), resp.Body}
		dropLength(resp)
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxJSONRewriteBody+1))
		if err != nil {
			return nil // pass the drained-remainder stream through untouched
		}
		_ = resp.Body.Close()
		if len(body) > MaxJSONRewriteBody {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			dropLength(resp)
			return nil
		}
		if out, changed, err := RewriteAnthropicMessage(body); err == nil && changed {
			body = out
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		dropLength(resp)
	}
	return nil
}

// dropLength invalidates stale framing after a body replacement; the proxy
// falls back to chunked/close-delimited delivery.
func dropLength(resp *http.Response) {
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.TransferEncoding = nil
}
