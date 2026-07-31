package thinkfix

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestJSONCleanPassthrough(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`
	out, changed, err := RewriteAnthropicMessage([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("clean body reported changed")
	}
	if string(out) != body {
		t.Fatal("clean body must be returned byte-identical")
	}
}

func TestJSONLeakTranslated(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"<|open|>think<|sep|reasoning here<|close|>think<|sep|answer here"},{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"echo '<|close|>'"}}],"stop_reason":"tool_use"}`
	out, changed, err := RewriteAnthropicMessage([]byte(body))
	if !changed || err != nil {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("want 3 blocks, got %d: %s", len(content), out)
	}
	assert := func(i int, typ, field, text string) {
		b := content[i].(map[string]any)
		if b["type"] != typ || b[field] != text {
			t.Fatalf("block %d: %#v", i, b)
		}
	}
	assert(0, "thinking", "thinking", "reasoning here")
	assert(1, "text", "text", "answer here")
	// Tool args pass through unscanned: the marker inside input stays literal
	// JSON data, which is correct — it is user content, not a section header.
	tool := content[2].(map[string]any)
	if tool["type"] != "tool_use" || !strings.Contains(tool["input"].(map[string]any)["cmd"].(string), "<|close|>") {
		t.Fatalf("tool block mangled: %#v", tool)
	}
}

func TestJSONNonMessageBody(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"not_found_error","message":"nope"}}`,
		`{"input_tokens":42}`,
		`{"content":"not-an-array"}`,
	} {
		out, changed, err := RewriteAnthropicMessage([]byte(body))
		if err != nil || changed || string(out) != body {
			t.Fatalf("%s: changed=%v err=%v", body, changed, err)
		}
	}
}

// --- MaybeWrapResponse gating ---

func mkResp(t *testing.T, method, path, ct, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "https://api.opentela.ai"+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
	return resp
}

func TestMaybeWrapSSEMessagesPath(t *testing.T) {
	in := msgStart() + blkStart(0, "text") + textDelta(0, "hello") + blkStop(0) + msgEnd()
	resp := mkResp(t, http.MethodPost, "/v1/service/llm/v1/messages", "text/event-stream", in)
	resp.Header.Set("Content-Length", "123")
	if err := MaybeWrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Length") != "" || resp.ContentLength != -1 {
		t.Fatal("stale Content-Length must be dropped on a wrapped body")
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != in {
		t.Fatal("marker-free SSE must stream through identical")
	}
}

func TestMaybeWrapSkips(t *testing.T) {
	leak := msgStart() + blkStart(0, "text") + textDelta(0, "<|open|>think<|sep|x<|close|>think<|sep|y") + blkStop(0) + msgEnd()
	cases := []struct {
		name   string
		method string
		path   string
		ct     string
	}{
		{"root messages still matches", http.MethodPost, "/v1/messages", "text/event-stream"}, // wrapped: listed for contrast below
		{"count_tokens", http.MethodPost, "/v1/messages/count_tokens", "text/event-stream"},
		{"chat completions", http.MethodPost, "/v1/service/llm/v1/chat/completions", "text/event-stream"},
		{"get", http.MethodGet, "/v1/messages", "text/event-stream"},
		{"other content type", http.MethodPost, "/v1/messages", "text/html"},
	}
	wantWrap := map[string]bool{"root messages still matches": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mkResp(t, tc.method, tc.path, tc.ct, leak)
			if err := MaybeWrapResponse(resp); err != nil {
				t.Fatal(err)
			}
			// Detect wrapping by whether Content-Length survived: unwrapped
			// responses keep whatever framing they had (none here, so use a
			// marker: wrapped SSE bodies have length -1 AND translators).
			out, _ := io.ReadAll(resp.Body)
			translated := !strings.Contains(string(out), "<|open|>")
			if wantWrap[tc.name] && !translated {
				t.Fatal("expected translation, got passthrough")
			}
			if !wantWrap[tc.name] && translated {
				t.Fatal("unexpected translation on a gated path")
			}
		})
	}
}

func TestMaybeWrapSkipsCompressed(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(msgStart()))
	_ = gz.Close()
	resp := mkResp(t, http.MethodPost, "/v1/messages", "text/event-stream", buf.String())
	resp.Header.Set("Content-Encoding", "gzip")
	if err := MaybeWrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(out, buf.Bytes()) {
		t.Fatal("compressed body must pass through byte-identical")
	}
}

func TestMaybeWrapJSONTranslated(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"a<|open|>think<|sep|b<|close|>think<|sep|c"}]}`
	resp := mkResp(t, http.MethodPost, "/v1/messages", "application/json", body)
	if err := MaybeWrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("want 3 blocks, got %s", out)
	}
}

func TestMaybeWrapJSONNoMarkerIdentical(t *testing.T) {
	body := `{"id":"msg_1","content":[{"type":"text","text":"plain answer"}]}`
	resp := mkResp(t, http.MethodPost, "/v1/messages", "application/json", body)
	if err := MaybeWrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != body {
		t.Fatalf("marker-free JSON must return identical, got %s", out)
	}
}

func TestMaybeWrapNonOKSkipped(t *testing.T) {
	resp := mkResp(t, http.MethodPost, "/v1/messages", "application/json", `{"content":[{"type":"text","text":"<|open|>think<|sep|"}]}`)
	resp.StatusCode = http.StatusInternalServerError
	if err := MaybeWrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "<|open|>") {
		t.Fatal("non-200 must not be rewritten")
	}
}
