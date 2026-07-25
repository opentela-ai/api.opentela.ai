package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestProxyForwardsRequestAndResponse(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth, gotCustom, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Custom")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "pong")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat?q=1", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("X-Custom", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotMethod != http.MethodPost {
		t.Errorf("upstream got method=%q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/v1/chat" || gotQuery != "q=1" {
		t.Errorf("upstream got path=%q query=%q, want /v1/chat q=1", gotPath, gotQuery)
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("upstream Authorization = %q, want Bearer abc", gotAuth)
	}
	if gotCustom != "abc" {
		t.Errorf("upstream X-Custom = %q, want abc", gotCustom)
	}
	if gotBody != "hello" {
		t.Errorf("upstream body = %q, want hello", gotBody)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("missing upstream response header")
	}
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // block until the test has read the first chunk
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n') // should arrive before "second" is sent
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first chunk = %q, want it to contain 'first'", line)
	}
	close(release) // now allow the upstream to send the rest
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "second") {
		t.Fatalf("rest = %q, want it to contain 'second'", rest)
	}
}

// TestProxyStreamsIncrementallyWithKnownContentLength targets FlushInterval
// specifically. httputil.ReverseProxy auto-forces immediate flushing
// (bypassing FlushInterval entirely) in two cases: a text/event-stream
// Content-Type, or an unknown response length (res.ContentLength == -1,
// i.e. chunked/streaming). TestProxyStreamsIncrementally above triggers the
// first of those, so it would still pass even if FlushInterval: -1 were
// removed from proxy.go. This test avoids both override conditions — a
// plain Content-Type and an explicit, correct Content-Length — so its
// incremental delivery depends solely on proxy.go's FlushInterval: -1.
// If that field regressed to its zero value, copyResponse would write
// directly to the destination ResponseWriter without an immediate flush,
// the small first chunk below (well under the ~4KB connection write buffer)
// would stay buffered, and the read below (issued before release is closed)
// would block/time out instead of returning "chunk-1|".
func TestProxyStreamsIncrementallyWithKnownContentLength(t *testing.T) {
	const part1 = "chunk-1|"
	const part2 = "chunk-2"
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(part1)+len(part2)))
		_, _ = io.WriteString(w, part1)
		fl.Flush()
		<-release // block until the test has read the first chunk
		_, _ = io.WriteString(w, part2)
		fl.Flush()
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/plain")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	first := make([]byte, len(part1))
	if _, err := io.ReadFull(br, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(first) != part1 {
		t.Fatalf("first chunk = %q, want %q", first, part1)
	}
	close(release) // now allow the upstream to send the rest
	rest, _ := io.ReadAll(br)
	if string(rest) != part2 {
		t.Fatalf("rest = %q, want %q", rest, part2)
	}
}

func TestProxyUpstreamDownReturns502(t *testing.T) {
	// Point at a closed server to force a dial failure.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, _ := url.Parse(dead.URL)
	dead.Close()

	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// The upstream sets its own permissive CORS headers. If they survive alongside
// this service's, the browser sees two Access-Control-Allow-Origin values and
// rejects the response — so the proxy must strip the upstream's copies.
func TestUpstreamCORSHeadersAreStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "authorization, origin")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	rec := httptest.NewRecorder()
	// Simulate this service's CORS middleware having already set its policy.
	rec.Header().Set("Access-Control-Allow-Origin", "https://app.example")
	New(target).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := rec.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != "https://app.example" {
		t.Fatalf("Access-Control-Allow-Origin = %v, want exactly [https://app.example]", got)
	}
	for _, h := range []string{"Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Allow-Credentials"} {
		if got := rec.Header().Values(h); len(got) != 0 {
			t.Fatalf("%s = %v, want upstream copy stripped", h, got)
		}
	}
	// The actual payload must still come through untouched.
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q, want upstream body preserved", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want non-CORS headers preserved", ct)
	}
}
