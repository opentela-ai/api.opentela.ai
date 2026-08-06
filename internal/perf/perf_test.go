package perf

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- parser -----------------------------------------------------------------

func parseAll(t *testing.T, contentType, body string) *streamParser {
	t.Helper()
	p := newStreamParser(contentType)
	p.feed([]byte(body))
	p.finish()
	return p
}

func TestParserOpenAISSE(t *testing.T) {
	// Split mid-line exercises the line buffer.
	sse := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"Hel" +
		"lo\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"!\"}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	p := parseAll(t, "text/event-stream", sse)
	if p.model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", p.model)
	}
	if p.inputTokens != 12 || p.outputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 12/2", p.inputTokens, p.outputTokens)
	}
	if !p.sawToken {
		t.Error("sawToken = false, want true")
	}
}

func TestParserAnthropicSSE(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":25}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	p := parseAll(t, "text/event-stream; charset=utf-8", sse)
	if p.model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5", p.model)
	}
	if p.inputTokens != 25 || p.outputTokens != 9 {
		t.Errorf("tokens = %d/%d, want 25/9", p.inputTokens, p.outputTokens)
	}
	if !p.sawToken {
		t.Error("sawToken = false, want true")
	}
}

func TestParserAnthropicCumulativeUsageKeepsMax(t *testing.T) {
	sse := "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"
	p := parseAll(t, "text/event-stream", sse)
	if p.outputTokens != 40 {
		t.Errorf("outputTokens = %d, want 40 (cumulative max)", p.outputTokens)
	}
}

func TestParserJSONResponse(t *testing.T) {
	body := `{"model":"qwen3:14b","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":31}}`
	p := parseAll(t, "application/json", body)
	if p.model != "qwen3:14b" || p.inputTokens != 7 || p.outputTokens != 31 || !p.sawToken {
		t.Errorf("probe = %+v, want qwen3:14b 7/31 sawToken", p)
	}
}

func TestParserJSONTrailingNewline(t *testing.T) {
	// json.Encoder-terminated bodies end with '\n'; usage must still parse.
	body := `{"model":"qwen3:14b","usage":{"prompt_tokens":7,"completion_tokens":31}}` + "\n"
	p := parseAll(t, "application/json", body)
	if p.model != "qwen3:14b" || p.inputTokens != 7 || p.outputTokens != 31 || !p.sawToken {
		t.Errorf("probe = %+v, want qwen3:14b 7/31 sawToken", p)
	}
}

func TestParserUnknownContentTypeIgnored(t *testing.T) {
	p := parseAll(t, "application/octet-stream", "data: {\"model\":\"x\"}\n\n")
	if p.model != "" || p.sawToken {
		t.Errorf("probe = %+v, want empty", p)
	}
}

// --- body wrapper -----------------------------------------------------------

type scriptReader struct {
	chunks []string
	i      int
	eof    bool
	closed bool
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		r.eof = true
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	if n < len(r.chunks[r.i]) {
		r.chunks[r.i] = r.chunks[r.i][n:]
		return n, nil
	}
	r.i++
	return n, nil
}

func (r *scriptReader) Close() error { r.closed = true; return nil }

func TestMeasureBodyRecordsTTFTTokensBytes(t *testing.T) {
	src := &scriptReader{chunks: []string{
		"data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	}}
	var got Sample
	m := wrapBody(src, "text/event-stream", time.Now(), func(m *measureBody) {
		got = Sample{Model: m.parser.model, InputTokens: m.parser.inputTokens,
			OutputTokens: m.parser.outputTokens, ResponseBytes: m.bytes,
			FirstTokenMs: durationMs(m.firstTok), TTFTMs: durationMs(m.ttft), ClientAbort: !m.eof, TotalMs: durationMs(m.total())}
	})
	out, err := io.ReadAll(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil { // the reverse proxy always closes the body
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "[DONE]\n\n") {
		t.Errorf("stream mangled: %q", out)
	}
	if got.Model != "m" || got.InputTokens != 3 || got.OutputTokens != 9 {
		t.Errorf("sample = %+v", got)
	}
	if got.ClientAbort {
		t.Error("clean EOF flagged as abort")
	}
	if got.TTFTMs == 0 || got.FirstTokenMs == 0 || got.TotalMs == 0 {
		t.Errorf("timings missing: %+v", got)
	}
	if got.FirstTokenMs < got.TTFTMs {
		t.Errorf("first token (%v) before first byte (%v)", got.FirstTokenMs, got.TTFTMs)
	}
	if got.ResponseBytes != int64(len(src.chunks[0])+len(src.chunks[1])+len(src.chunks[2])) {
		t.Errorf("bytes = %d", got.ResponseBytes)
	}
	if !src.closed {
		t.Error("underlying Close not delegated")
	}
}

func TestMeasureBodyClientAbort(t *testing.T) {
	src := &scriptReader{chunks: []string{"data: {}\n\n", "data: {}\n\n"}}
	calls := 0
	m := wrapBody(src, "text/event-stream", time.Now(), func(m *measureBody) {
		calls++
		if m.eof {
			t.Error("abort before EOF marked complete")
		}
	})
	buf := make([]byte, 4096)
	if _, err := m.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// Double Close must finalize exactly once.
	_ = m.Close()
	if calls != 1 {
		t.Fatalf("done called %d times, want 1", calls)
	}
}

// --- hook -------------------------------------------------------------------

type sliceRecorder struct{ samples []Sample }

func (r *sliceRecorder) Observe(s Sample) { r.samples = append(r.samples, s) }

func newResponse(targetURL, body, contentType string, hdr http.Header) *http.Response {
	req := httptest.NewRequest(http.MethodPost, targetURL, nil)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHookSamplesStampedInferenceResponse(t *testing.T) {
	rec := &sliceRecorder{}
	nodeTable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dnt/table" {
			t.Errorf("unexpected resolver path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"peer-1":{"hardware":{"gpus":[{"name":"NVIDIA GeForce  RTX 4090"}]}}}`))
	}))
	defer nodeTable.Close()
	upstream, _ := url.Parse(nodeTable.URL)
	hook := Hook(rec, NewResolver(upstream, time.Minute))

	hdr := http.Header{}
	hdr.Set(PeerHeader, "peer-1")
	hdr.Set("Content-Type", "text/event-stream")
	resp := newResponse("https://api/v1/service/chat/v1/chat/completions",
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n",
		"text/event-stream", hdr)
	if err := hook(resp); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if len(rec.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(rec.samples))
	}
	s := rec.samples[0]
	if s.Service != "chat" || s.Route != "chat/completions" {
		t.Errorf("service/route = %q/%q", s.Service, s.Route)
	}
	if s.Model != "gpt-4o" || s.InputTokens != 5 || s.OutputTokens != 7 {
		t.Errorf("probe = %q %d/%d", s.Model, s.InputTokens, s.OutputTokens)
	}
	if s.GPUModel != "NVIDIA GeForce RTX 4090" || s.GPUCount != 1 {
		t.Errorf("gpu = %q x%d (want normalized name; resolver not consulted?)", s.GPUModel, s.GPUCount)
	}
	if s.PeerFP == "" || s.PeerFP == "peer-1" || len(s.PeerFP) != 16 {
		t.Errorf("peer_fp = %q: must be anonymized 16-hex fingerprint", s.PeerFP)
	}
}

func TestHookSamplesNonStreamingJSON(t *testing.T) {
	rec := &sliceRecorder{}
	hook := Hook(rec, nil)
	hdr := http.Header{}
	hdr.Set(PeerHeader, "peer-1")
	hdr.Set("Content-Type", "application/json")
	body := `{"model":"claude-sonnet-4-5","usage":{"input_tokens":25,"output_tokens":9}}` + "\n"
	resp := newResponse("https://api/v1/service/chat/v1/messages", body, "application/json", hdr)
	if err := hook(resp); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Fatal("measuring wrapper must deliver bytes untouched")
	}
	if len(rec.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(rec.samples))
	}
	s := rec.samples[0]
	if s.Model != "claude-sonnet-4-5" || s.InputTokens != 25 || s.OutputTokens != 9 {
		t.Errorf("probe = %q %d/%d", s.Model, s.InputTokens, s.OutputTokens)
	}
	if s.FirstTokenMs != 0 {
		t.Errorf("first_token_ms = %v, want 0 for non-streaming (rollup falls back to total)", s.FirstTokenMs)
	}
	if s.TTFTMs == 0 || s.TotalMs == 0 {
		t.Errorf("timings missing: %+v", s)
	}
	if s.ClientAbort {
		t.Error("clean EOF flagged as abort")
	}
}

// TestHookAnchorsClockAtRequestWrite is the non-streaming latency
// regression test: the upstream sends headers and body together after a
// generation delay, so they arrive coalesced. Without Instrument the body is
// already buffered at ModifyResponse time and TotalMs collapses toward zero;
// anchored at request-write it must span the delay.
func TestHookAnchorsClockAtRequestWrite(t *testing.T) {
	const delay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay) // generation window
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":3,"completion_tokens":9}}`)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/service/chat/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(Instrument(req))
	if err != nil {
		t.Fatal(err)
	}
	resp.Header.Set(PeerHeader, "peer-1") // stamped by the mesh head in production

	rec := &sliceRecorder{}
	hook := Hook(rec, nil)
	if err := hook(resp); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if len(rec.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(rec.samples))
	}
	s := rec.samples[0]
	if s.TotalMs < 100 || s.TTFTMs < 100 {
		t.Errorf("TotalMs/TTFTMs = %v/%v, want >= ~%v (generation window); clock not anchored at request-write?",
			s.TotalMs, s.TTFTMs, delay)
	}
	if s.OutputTokens != 9 || s.InputTokens != 3 {
		t.Errorf("tokens = %d/%d", s.InputTokens, s.OutputTokens)
	}
}

func TestClockStartFallback(t *testing.T) {
	fallback := time.Now()
	if got := clockStart(nil, fallback); !got.Equal(fallback) {
		t.Error("nil request must fall back")
	}
	req := httptest.NewRequest(http.MethodPost, "https://api/x", nil)
	if got := clockStart(req, fallback); !got.Equal(fallback) {
		t.Error("uninstrumented request must fall back")
	}
	if got := clockStart(Instrument(req), fallback); !got.Equal(fallback) {
		t.Error("instrumented but unwritten request must fall back")
	}
}

func TestHookIgnoresUnstampedOrNonServiceResponses(t *testing.T) {
	rec := &sliceRecorder{}
	hook := Hook(rec, nil)

	cases := []struct {
		name string
		url  string
		hdr  http.Header
	}{
		{"no peer header", "https://api/v1/service/chat/v1/chat/completions", http.Header{}},
		{"peer but not service route", "https://api/v1/services", http.Header{PeerHeader: {"peer-1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := newResponse(tc.url, "ok", "text/event-stream", tc.hdr)
			before := resp.Body
			if err := hook(resp); err != nil {
				t.Fatal(err)
			}
			if resp.Body != before {
				t.Error("body wrapped despite no sampling")
			}
			_, _ = io.ReadAll(resp.Body)
		})
	}
	if len(rec.samples) != 0 {
		t.Fatalf("samples = %d, want 0", len(rec.samples))
	}
}

func TestHookTimingOnlyForEncodedBody(t *testing.T) {
	rec := &sliceRecorder{}
	hook := Hook(rec, nil)
	hdr := http.Header{PeerHeader: {"peer-1"}, "Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}}
	resp := newResponse("https://api/v1/service/chat/v1/chat/completions", "not-gzip", "application/json", hdr)
	if err := hook(resp); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if len(rec.samples) != 1 {
		t.Fatalf("samples = %d, want 1 (timing-only)", len(rec.samples))
	}
	if rec.samples[0].Model != "" || rec.samples[0].OutputTokens != 0 {
		t.Errorf("encoded body parsed despite skip: %+v", rec.samples[0])
	}
}

// --- resolver ---------------------------------------------------------------

func TestResolverCachesAndHandlesCPUOnly(t *testing.T) {
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		// Production shape: DNT keys carry a leading slash; the slash-less id
		// field inside the entry is what X-Computing-Node stamps carry.
		_, _ = w.Write([]byte(`{"/gpu-peer":{"id":"gpu-peer","hardware":{"gpus":[{"name":"Tesla T4"}]}},"/id-less":{"hardware":{"gpus":[{"name":"NVIDIA GB10"}]}},"/cpu-peer":{"id":"cpu-peer","hardware":{"gpus":[]}}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := NewResolver(u, time.Minute)

	if got := r.Resolve(context.Background(), "gpu-peer"); got.Model != "Tesla T4" || got.Count != 1 {
		t.Errorf("gpu-peer = %+v", got)
	}
	if got := r.Resolve(context.Background(), "id-less"); got.Model != "NVIDIA GB10" {
		t.Errorf("id-less (slash-key fallback) = %+v", got)
	}
	if got := r.Resolve(context.Background(), "cpu-peer"); got.Model != "" || got.Count != 0 {
		t.Errorf("cpu-peer = %+v, want empty", got)
	}
	if got := r.Resolve(context.Background(), "unknown"); got != (GPUInfo{}) {
		t.Errorf("unknown = %+v, want zero", got)
	}
	if fetches != 1 {
		t.Errorf("fetches = %d, want 1 (cached)", fetches)
	}
}

func TestResolverFetchFailureIsSoft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := NewResolver(u, time.Minute)
	if got := r.Resolve(context.Background(), "any"); got != (GPUInfo{}) {
		t.Errorf("got %+v, want soft-empty on fetch failure", got)
	}
}

// --- sink -------------------------------------------------------------------

func TestSinkBatchesAndFlushes(t *testing.T) {
	// The handler runs on a server goroutine; bodies needs a guard.
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-ClickHouse-Key"); got != "secret" {
			t.Errorf("X-ClickHouse-Key = %q", got)
		}
		if q := r.URL.Query().Get("query"); !strings.Contains(q, "INSERT INTO perf_samples") {
			t.Errorf("query = %q", q)
		}
		if db := r.URL.Query().Get("database"); db != "testdb" {
			t.Errorf("database = %q", db)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	firstBody := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(bodies) == 0 {
			return ""
		}
		return bodies[0]
	}
	u, _ := url.Parse(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	sink := NewClickHouseSink(u, "testdb", "default", "secret", 10*time.Millisecond, 100, 100)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); sink.Run(ctx) }()

	sink.Observe(Sample{TS: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Service: "chat", Model: "m", PeerFP: "0123456789abcdef", GPUModel: "Tesla T4", OutputTokens: 5})
	deadline := time.Now().Add(2 * time.Second)
	for firstBody() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	body := firstBody()
	if body == "" {
		t.Fatal("no insert happened")
	}
	for _, want := range []string{`"ts":"2025-01-01 00:00:00.000"`, `"model":"m"`, `"gpu_model":"Tesla T4"`, `"output_tokens":5`} {
		if !strings.Contains(body, want) {
			t.Errorf("insert body missing %s: %s", want, body)
		}
	}

	cancel()
	<-done
}

func TestSinkFinalFlushOnShutdown(t *testing.T) {
	flushed := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		flushed <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	sink := NewClickHouseSink(u, "db", "", "", time.Hour /* never tick */, 100, 100)
	done := make(chan struct{})
	go func() { defer close(done); sink.Run(ctx) }()
	sink.Observe(Sample{TS: time.Now(), Model: "shutdown-sample"})
	cancel()
	<-done
	select {
	case body := <-flushed:
		if !strings.Contains(body, "shutdown-sample") {
			t.Errorf("final flush body = %s", body)
		}
	default:
		t.Fatal("final flush did not happen")
	}
}

func TestSinkDropWhenQueueFull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	sink := NewClickHouseSink(u, "db", "", "", time.Hour, 10, 1) // queue of 1, never flushed
	sink.Observe(Sample{})
	sink.Observe(Sample{}) // must not block
	if sink.dropped != 1 {
		t.Fatalf("dropped = %d, want 1", sink.dropped)
	}
}

// --- misc -------------------------------------------------------------------

func TestSplitServiceRoute(t *testing.T) {
	cases := map[string][2]string{
		"/v1/service/chat/v1/chat/completions": {"chat", "chat/completions"},
		"/v1/service/chat/v1/messages":         {"chat", "messages"},
		"/v1/service/vision/v1/embeddings":     {"vision", "embeddings"},
		"/v1/services":                         {"", ""},
		"/v1/service/chat":                     {"", ""},
		"/v1/service/chat/very/deep/route":     {"chat", "very/deep/route"},
		"/v1/service//v1/x":                    {"", ""},
	}
	for path, want := range cases {
		svc, route := splitServiceRoute(path)
		if svc != want[0] || route != want[1] {
			t.Errorf("splitServiceRoute(%q) = (%q,%q), want (%q,%q)", path, svc, route, want[0], want[1])
		}
	}
}

func TestFingerprintStableAndAnonymous(t *testing.T) {
	a, b := fingerprint("peer-1"), fingerprint("peer-1")
	if a != b || len(a) != 16 {
		t.Fatalf("fingerprint unstable: %q vs %q", a, b)
	}
	if bytes.Contains([]byte(a), []byte("peer")) {
		t.Fatal("fingerprint leaks peer id")
	}
}
