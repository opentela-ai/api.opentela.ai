package perf

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// wireTimings carries outbound-request trace stamps from Instrument to Hook.
type wireTimings struct {
	wroteAt atomic.Int64 // unix nanos; set when the request hit the wire
}

type timingsContextKey struct{}

// Instrument returns a shallow copy of req whose context traces the outbound
// request lifecycle, stamping the moment the request is fully written to the
// wire — after the (possibly large) request body has been sent, the closest
// observable point to "generation started". Hook anchors its clock there.
//
// Without it, non-streaming responses are mismeasured: workers send headers
// only after finishing the body, and on a fast link headers and body arrive
// coalesced, so a clock started at ModifyResponse (headers parsed) sees only
// the gateway-side buffer drain — fractions of a millisecond whatever the
// true generation latency was. Streaming responses (headers sent up front)
// are unaffected beyond a sub-millisecond queue/connection shift.
func Instrument(req *http.Request) *http.Request {
	t := &wireTimings{}
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			t.wroteAt.Store(time.Now().UnixNano())
		},
	}
	ctx := httptrace.WithClientTrace(req.Context(), trace)
	ctx = context.WithValue(ctx, timingsContextKey{}, t)
	return req.WithContext(ctx)
}

// clockStart is the measurement anchor: the request's wire time when
// Instrument stamped it, else the fallback (ModifyResponse time).
func clockStart(req *http.Request, fallback time.Time) time.Time {
	if req == nil {
		return fallback
	}
	t, ok := req.Context().Value(timingsContextKey{}).(*wireTimings)
	if !ok {
		return fallback
	}
	if ns := t.wroteAt.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return fallback
}
