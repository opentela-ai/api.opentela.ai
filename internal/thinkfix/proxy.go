package thinkfix

import (
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
		// Buffer lazily, on first Read — never inside ModifyResponse. The
		// upstream body has not transferred yet at this point, and steps
		// further down the chain wrap the body to time/measure the stream
		// the client receives; an eager ReadAll would not return until the
		// worker finished generating, hiding that entire latency.
		resp.Body = &jsonRewriteBody{rc: resp.Body}
		dropLength(resp)
	}
	return nil
}

// jsonRewriteBody defers the whole-body buffering a JSON rewrite needs until
// the body is actually read, then serves the rewritten (or verified
// marker-free) bytes. Reads past the end of the rewritten buffer for over-cap
// bodies stream the unread remainder through untouched.
type jsonRewriteBody struct {
	rc   io.ReadCloser
	buf  []byte        // buffered (possibly rewritten) body, once loaded
	off  int           // read offset into buf
	rest io.ReadCloser // remainder passthrough for over-cap bodies
	err  error         // sticky upstream read failure, surfaced after buf
	done bool
}

func (w *jsonRewriteBody) load() {
	if w.done {
		return
	}
	w.done = true
	body, err := io.ReadAll(io.LimitReader(w.rc, MaxJSONRewriteBody+1))
	if err != nil {
		// Broken upstream body: deliver what arrived, then surface the error.
		w.buf, w.err = body, err
		_ = w.rc.Close()
		return
	}
	if len(body) > MaxJSONRewriteBody {
		// Over the rewrite cap: pass through untouched — the lookahead first,
		// then the remainder straight from the upstream without buffering it.
		w.buf, w.rest = body, w.rc
		return
	}
	_ = w.rc.Close()
	if out, changed, rerr := RewriteAnthropicMessage(body); rerr == nil && changed {
		body = out
	}
	w.buf = body
}

func (w *jsonRewriteBody) Read(p []byte) (int, error) {
	w.load()
	if w.off < len(w.buf) {
		n := copy(p, w.buf[w.off:])
		w.off += n
		return n, nil
	}
	if w.rest != nil {
		return w.rest.Read(p)
	}
	if w.err != nil {
		return 0, w.err
	}
	return 0, io.EOF
}

func (w *jsonRewriteBody) Close() error { return w.rc.Close() }

// dropLength invalidates stale framing after a body replacement; the proxy
// falls back to chunked/close-delimited delivery.
func dropLength(resp *http.Response) {
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.TransferEncoding = nil
}
