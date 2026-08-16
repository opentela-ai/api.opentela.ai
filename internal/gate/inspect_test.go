package gate

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newReq(t *testing.T, method, path string, body string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	return req
}

func drain(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	return string(b)
}

func TestInspectServiceAndRoute(t *testing.T) {
	for _, tc := range []struct {
		path       string
		service    string
		route      string
		generative bool
		supported  bool
	}{
		{"/v1/service/llm/v1/chat/completions", "llm", "chat/completions", true, true},
		{"/v1/service/llm/v1/completions", "llm", "completions", true, true},
		{"/v1/service/anthropic/v1/messages", "anthropic", "messages", true, true},
		{"/v1/service/openai/v1/responses", "openai", "responses", true, true},
		{"/v1/service/llm/v1/embeddings", "llm", "embeddings", false, true},
		{"/v1/service/llm/v1/models", "llm", "models", false, false},
		{"/healthz", "", "", false, false},
		{"/v1/service/llm/", "llm", "", false, false}, // no route after service // no route after service
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := newReq(t, http.MethodPost, tc.path, `{"model":"x"}`)
			p, _ := Inspect(req, Options{OutputMax: 1024})
			if p.Service != tc.service || p.Route != tc.route {
				t.Fatalf("service/route = (%q,%q), want (%q,%q)", p.Service, p.Route, tc.service, tc.route)
			}
			if p.Generative != tc.generative {
				t.Fatalf("generative = %v, want %v", p.Generative, tc.generative)
			}
			if p.Supported != tc.supported {
				t.Fatalf("supported = %v, want %v", p.Supported, tc.supported)
			}
		})
	}
}

func TestInspectExtractsModelStreamAndMaxTokens(t *testing.T) {
	body := `{"model":"llama3.1-70b","stream":true,"max_tokens":512,"messages":[]}`
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
	p, restored := Inspect(req, Options{OutputMax: 1024})
	if p.Model != "llama3.1-70b" || !p.Streaming || p.OutputCeiling != 512 || !p.Supported {
		t.Fatalf("plan = %+v", p)
	}
	// Output ceiling is the min of body max_tokens (512) and operator cap (1024).
	if got := drain(t, restored); got != body {
		t.Fatalf("body not restored byte-for-byte:\n got %q\nwant %q", got, body)
	}
}

func TestInspectOutputCeilingUsesOperatorCap(t *testing.T) {
	// Body asks for 4096; operator caps at 1024 → reserve conservatively at 1024.
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", `{"model":"x","max_tokens":4096}`)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.OutputCeiling != 1024 {
		t.Fatalf("output ceiling = %d, want 1024 (operator cap)", p.OutputCeiling)
	}
}

func TestInspectClampsMaxTokensToCeiling(t *testing.T) {
	// The operator caps output at 1024 but the client asks for 4096. The gate
	// must rewrite max_tokens down to 1024 so the upstream cannot generate
	// past the reservation — otherwise the buyer is charged for tokens the
	// meter never authorized.
	for _, tc := range []struct{ name, in string }{
		{"max_tokens", `{"model":"x","max_tokens":4096,"messages":[]}`},
		{"max_completion_tokens", `{"model":"x","max_completion_tokens":4096,"messages":[]}`},
		{"max_output_tokens", `{"model":"x","max_output_tokens":4096,"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", tc.in)
			req2 := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", tc.in)
			p, restored := Inspect(req, Options{OutputMax: 1024})
			if p.OutputCeiling != 1024 {
				t.Fatalf("ceiling = %d, want 1024", p.OutputCeiling)
			}
			got := drain(t, restored)
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Fatalf("rewritten body not JSON: %v; got %q", err, got)
			}
			var v int
			if err := json.Unmarshal(m[tc.name], &v); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if v != 1024 {
				t.Fatalf("%s = %d, want 1024 (clamped)", tc.name, v)
			}
			// Non-max fields are preserved.
			if string(m["model"]) != `"x"` {
				t.Fatalf("model altered: %s", m["model"])
			}
			// Content-Length matches the rewritten body.
			if restored.ContentLength != int64(len(got)) {
				t.Fatalf("Content-Length = %d, want %d", restored.ContentLength, len(got))
			}
			_ = req2 // keep the original-comparison intent explicit
		})
	}
}

func TestInspectInjectsOperatorCapWhenBodyOmitsOutputBound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		body      string
		fieldName string
	}{
		{
			name:      "chat completions",
			path:      "/v1/service/llm/v1/chat/completions",
			body:      `{"model":"x","messages":[]}`,
			fieldName: "max_tokens",
		},
		{
			name:      "responses",
			path:      "/v1/service/openai/v1/responses",
			body:      `{"model":"x","input":"hello"}`,
			fieldName: "max_output_tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(t, http.MethodPost, tc.path, tc.body)
			p, restored := Inspect(req, Options{OutputMax: 1024})
			if p.OutputCeiling != 1024 || !p.Supported {
				t.Fatalf("plan = %+v", p)
			}
			got := drain(t, restored)
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got), &m); err != nil {
				t.Fatalf("rewritten body not JSON: %v; got %q", err, got)
			}
			var v int
			if err := json.Unmarshal(m[tc.fieldName], &v); err != nil {
				t.Fatalf("%s: %v", tc.fieldName, err)
			}
			if v != 1024 {
				t.Fatalf("%s = %d, want 1024", tc.fieldName, v)
			}
		})
	}
}

func TestInspectDoesNotClampWhenWithinCeiling(t *testing.T) {
	// max_tokens (512) is within the operator cap (1024): the gate must
	// forward the body byte-for-byte, not reformat it.
	body := `{"model":"x","max_tokens":512,"messages":[]}`
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
	_, restored := Inspect(req, Options{OutputMax: 1024})
	if got := drain(t, restored); got != body {
		t.Fatalf("within-ceiling body must be unchanged:\n got %q\nwant %q", got, body)
	}
}

func TestInspectOpenAICompletionMaxField(t *testing.T) {
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", `{"model":"x","max_completion_tokens":256}`)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.OutputCeiling != 256 {
		t.Fatalf("output ceiling = %d, want 256", p.OutputCeiling)
	}
}

func TestInspectResponsesMaxOutputTokens(t *testing.T) {
	req := newReq(t, http.MethodPost, "/v1/service/openai/v1/responses", `{"model":"x","max_output_tokens":128}`)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.OutputCeiling != 128 {
		t.Fatalf("output ceiling = %d, want 128", p.OutputCeiling)
	}
}

func TestInspectGenerativeWithoutCeilingUnsupported(t *testing.T) {
	// No body max_tokens and no operator cap → the meter cannot bound output.
	for _, body := range []string{`{"model":"x"}`, `{"model":"x","max_tokens":0}`} {
		req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
		p, _ := Inspect(req, Options{}) // OutputMax 0
		if p.Supported {
			t.Fatalf("body %q: want unsupported, got %+v", body, p)
		}
	}
	// With a cap it becomes supported again.
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", `{"model":"x"}`)
	p, _ := Inspect(req, Options{OutputMax: 2048})
	if !p.Supported || p.OutputCeiling != 2048 {
		t.Fatalf("with cap: %+v", p)
	}
}

func TestInspectInputCeilingFromContentLength(t *testing.T) {
	body := strings.Repeat("a", 5000)
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
	req.ContentLength = int64(len(body)) // httptest sets this from the reader; assert anyway
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.InputCeiling != len(body) {
		t.Fatalf("input ceiling = %d, want %d", p.InputCeiling, len(body))
	}
}

func TestInspectInputCeilingDrainsUnknownLength(t *testing.T) {
	// Chunked: ContentLength unknown. Inspect drains the whole body (≤ MaxBody)
	// for the ceiling and restores it.
	body := `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(body))
	req.ContentLength = -1
	p, restored := Inspect(req, Options{OutputMax: 1024})
	if p.Model != "x" || p.OutputCeiling != 10 {
		t.Fatalf("plan = %+v", p)
	}
	if p.InputCeiling != len(body) {
		t.Fatalf("input ceiling = %d, want %d", p.InputCeiling, len(body))
	}
	if got := drain(t, restored); got != body {
		t.Fatalf("body not restored:\n got %q\nwant %q", got, body)
	}
}

func TestInspectStreamingIncludeUsage(t *testing.T) {
	body := `{"model":"x","stream":true,"stream_options":{"include_usage":true},"max_tokens":16}`
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if !p.Streaming || !p.IncludeUsage {
		t.Fatalf("streaming=%v includeUsage=%v, want true/true", p.Streaming, p.IncludeUsage)
	}
	// A streaming request without include_usage flags it for the meter; the
	// gate (Step 3) rejects such requests while enforcement is on.
	req2 := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", `{"model":"x","stream":true,"max_tokens":16}`)
	p2, _ := Inspect(req2, Options{OutputMax: 1024})
	if p2.Streaming && p2.IncludeUsage {
		t.Fatalf("includeUsage should be false when stream_options omitted")
	}
}

func TestInspectNonStreamingAnthropic(t *testing.T) {
	body := `{"model":"claude-3-5-sonnet","max_tokens":64,"messages":[]}`
	req := newReq(t, http.MethodPost, "/v1/service/anthropic/v1/messages", body)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.Service != "anthropic" || p.Route != "messages" || p.OutputCeiling != 64 || p.Streaming {
		t.Fatalf("plan = %+v", p)
	}
}

func TestInspectLargeBodyDegradesButRestores(t *testing.T) {
	// A body larger than InspectCap: the model may still be found in the
	// prefix; the full body is restored regardless.
	prefix := `{"model":"llama","max_tokens":8,"prompt":"`
	tail := strings.Repeat("x", InspectCap+1024)
	full := prefix + tail + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/completions", strings.NewReader(full))
	req.ContentLength = int64(len(full))
	p, restored := Inspect(req, Options{OutputMax: 1024})
	if p.Model != "llama" || p.OutputCeiling != 8 {
		t.Fatalf("plan = %+v", p)
	}
	if got := drain(t, restored); got != full {
		t.Fatalf("body not restored (len got=%d want=%d)", len(got), len(full))
	}
}

func TestInspectUnboundedChunkedIsRejected(t *testing.T) {
	// A chunked body exceeding maxBody: Inspect cannot bound the input charge.
	// Lower the budget so the test does not allocate the production ceiling.
	saved := maxBody
	maxBody = 4096
	defer func() { maxBody = saved }()
	body := strings.Repeat("y", maxBody+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(body))
	req.ContentLength = -1
	p, restored := Inspect(req, Options{OutputMax: 1024})
	if p.Supported {
		t.Fatalf("want unsupported for chunked > maxBody, got %+v", p)
	}
	// The body is still restored (the gate rejects the request; the proxy never forwards).
	if got := drain(t, restored); len(got) != len(body) {
		t.Fatalf("body not restored (len got=%d want=%d)", len(got), len(body))
	}
}

func TestInspectIgnoresUnknownFields(t *testing.T) {
	// The prompt content is unknown to the shape and must round-trip intact.
	body := `{"model":"x","max_tokens":4,"messages":[{"role":"user","content":"secret payload 🤖"}]}`
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/chat/completions", body)
	p, restored := Inspect(req, Options{OutputMax: 1024})
	if !bytes.Equal([]byte(drain(t, restored)), []byte(body)) {
		t.Fatalf("body altered; model=%q", p.Model)
	}
}

func TestInspectNonGeneratedRouteNoOutputCeiling(t *testing.T) {
	req := newReq(t, http.MethodPost, "/v1/service/llm/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`)
	p, _ := Inspect(req, Options{OutputMax: 1024})
	if p.Generative || p.OutputCeiling != 0 || !p.Supported {
		t.Fatalf("embeddings plan = %+v", p)
	}
	if p.InputCeiling == 0 {
		t.Fatalf("embeddings should still bound input tokens")
	}
}
