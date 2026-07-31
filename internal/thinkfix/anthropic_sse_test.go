package thinkfix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// ev builds one SSE frame. HTML escaping is disabled so the byte stream looks
// like a real upstream's (sglang/Python does not escape < > & in JSON).
func ev(name string, payload map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	data := strings.TrimRight(buf.String(), "\n")
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data)
}

func msgStart() string {
	return ev("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_x", "type": "message", "role": "assistant",
			"model": "m", "content": []any{}, "stop_reason": nil,
		},
	})
}

func blkStart(idx int, kind string) string {
	return ev("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": kind, kind: ""},
	})
}

func toolStart(idx int) string {
	return ev("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}},
	})
}

func delta(idx int, field, dtype, text string) string {
	return ev("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": dtype, field: text},
	})
}

func textDelta(idx int, text string) string  { return delta(idx, "text", "text_delta", text) }
func thinkDelta(idx int, text string) string { return delta(idx, "thinking", "thinking_delta", text) }
func jsonDelta(idx int, jsonFrag string) string {
	return delta(idx, "partial_json", "input_json_delta", jsonFrag)
}

func blkStop(idx int) string {
	return ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

func msgEnd() string {
	return ev("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 5},
	}) + ev("message_stop", map[string]any{"type": "message_stop"})
}

// event is a parsed SSE frame used for assertions.
type event struct {
	name string
	data map[string]any
}

func parseStream(t *testing.T, s string) []event {
	t.Helper()
	var out []event
	for _, frame := range strings.Split(strings.TrimRight(s, "\n"), "\n\n") {
		if frame == "" {
			continue
		}
		var e event
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				e.name = line[len("event: "):]
			} else if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(line[len("data: "):]), &e.data); err != nil {
					t.Fatalf("bad json in frame %q: %v", frame, err)
				}
			} else {
				t.Fatalf("unexpected line %q in frame", line)
			}
		}
		out = append(out, e)
	}
	return out
}

func rewrite(t *testing.T, in string) string {
	t.Helper()
	out, err := io.ReadAll(NewAnthropicSSERewriter(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// describe renders a compact block-level trace of a stream:
//
//	msg_start [0:text]+"hi" [0:stop] msg_delta msg_stop
func describe(evs []event) string {
	var parts []string
	for _, e := range evs {
		switch e.name {
		case "message_start":
			parts = append(parts, "msg_start")
		case "content_block_start":
			cb := e.data["content_block"].(map[string]any)
			parts = append(parts, fmt.Sprintf("[%v:%s]", e.data["index"], cb["type"]))
		case "content_block_delta":
			d := e.data["delta"].(map[string]any)
			kind := d["type"].(string)
			var text string
			for _, f := range []string{"text", "thinking", "partial_json"} {
				if v, ok := d[f].(string); ok {
					text = v
				}
			}
			parts = append(parts, fmt.Sprintf("+%s%q", kind, text))
		case "content_block_stop":
			parts = append(parts, fmt.Sprintf("[%v:stop]", e.data["index"]))
		case "message_delta":
			parts = append(parts, "msg_delta")
		case "message_stop":
			parts = append(parts, "msg_stop")
		default:
			parts = append(parts, e.name)
		}
	}
	return strings.Join(parts, " ")
}

// A marker-free stream (vLLM, or a healthy sglang) passes through untouched.
func TestSSECleanPassthroughByteIdentical(t *testing.T) {
	in := msgStart() +
		blkStart(0, "thinking") + thinkDelta(0, "reasoning ") + thinkDelta(0, "done") + blkStop(0) +
		blkStart(1, "text") + textDelta(1, "The ") + textDelta(1, "answer.") + blkStop(1) +
		msgEnd()
	if got := rewrite(t, in); got != in {
		t.Fatalf("clean stream modified:\n--- in ---\n%s\n--- out ---\n%s", in, got)
	}
}

// The user's live leak: the entire reply arrives as one text block that opens
// with <|open|>think<|sep|.
func TestSSEFullLeakTranslated(t *testing.T) {
	in := msgStart() +
		blkStart(0, "text") +
		textDelta(0, "<|open|>think<|sep|Let me think. ") +
		textDelta(0, "More reasoning.") +
		textDelta(0, "<|close|>think<|sep|") +
		textDelta(0, "The answer is 42.") +
		blkStop(0) +
		msgEnd()
	got := describe(parseStream(t, rewrite(t, in)))
	want := `msg_start [0:text] [0:stop] [1:thinking] +thinking_delta"Let me think. " +thinking_delta"More reasoning." [1:stop] [2:text] +text_delta"The answer is 42." [2:stop] msg_delta msg_stop`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// No marker bytes may survive.
	if strings.Contains(rewrite(t, in), "<|open|>") || strings.Contains(rewrite(t, in), "<|close|>") {
		t.Fatal("marker bytes leaked into output")
	}
}

// Markers split across delta boundaries must still translate.
func TestSSEMarkerSplitAcrossDeltas(t *testing.T) {
	in := msgStart() +
		blkStart(0, "text") +
		textDelta(0, "<|ope") + textDelta(0, "n|>think<|se") + textDelta(0, "p|") +
		textDelta(0, "why hello") +
		textDelta(0, "<|close|>think<|sep|world") +
		blkStop(0) +
		msgEnd()
	got := describe(parseStream(t, rewrite(t, in)))
	want := `msg_start [0:text] [0:stop] [1:thinking] +thinking_delta"why hello" [1:stop] [2:text] +text_delta"world" [2:stop] msg_delta msg_stop`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The live-observed truncation artifact: a thinking block whose last delta
// ends in a bare "<|close|>" must not deliver the fragment.
func TestSSETruncatedMarkerTailDropped(t *testing.T) {
	in := msgStart() +
		blkStart(0, "thinking") +
		thinkDelta(0, "Summarize the repo state. ") +
		thinkDelta(0, "Need concise.<|close|>") +
		blkStop(0) +
		msgEnd()
	out := rewrite(t, in)
	if strings.Contains(out, "<|close|>") {
		t.Fatalf("truncated marker tail leaked:\n%s", out)
	}
	got := describe(parseStream(t, out))
	want := `msg_start [0:thinking] +thinking_delta"Summarize the repo state. " +thinking_delta"Need concise." [0:stop] msg_delta msg_stop`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Tool blocks and their JSON arguments are never scanned or restructured,
// even mid-rewrite.
func TestSSEToolBlockUntouched(t *testing.T) {
	in := msgStart() +
		blkStart(0, "text") + textDelta(0, "<|open|>think<|sep|thinking...<|close|>think<|sep|calling a tool") + blkStop(0) +
		toolStart(1) + jsonDelta(1, `{"cmd":"echo '<|open|>think<|sep|'"}`) + blkStop(1) +
		msgEnd()
	out := rewrite(t, in)
	got := describe(parseStream(t, out))
	want := `msg_start [0:text] [0:stop] [1:thinking] +thinking_delta"thinking..." [1:stop] [2:text] +text_delta"calling a tool" [2:stop] [3:tool_use] +input_json_delta"{\"cmd\":\"echo '<|open|>think<|sep|'\"}" [3:stop] msg_delta msg_stop`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A marker appearing mid-text-block: text before the marker stays in the
// original block; the block closes at the marker.
func TestSSEMarkerMidBlock(t *testing.T) {
	in := msgStart() +
		blkStart(0, "text") +
		textDelta(0, "Some visible text. ") +
		textDelta(0, "<|open|>think<|sep|hidden") +
		textDelta(0, "<|close|>think<|sep|visible again") +
		blkStop(0) +
		msgEnd()
	got := describe(parseStream(t, rewrite(t, in)))
	want := `msg_start [0:text] +text_delta"Some visible text. " [0:stop] [1:thinking] +thinking_delta"hidden" [1:stop] [2:text] +text_delta"visible again" [2:stop] msg_delta msg_stop`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Stream cut without message_stop: dangling synthetic block is closed.
func TestSSECutStreamClosed(t *testing.T) {
	in := msgStart() +
		blkStart(0, "text") +
		textDelta(0, "<|open|>think<|sep|still thinking")
	out := rewrite(t, in)
	got := describe(parseStream(t, out))
	want := `msg_start [0:text] [0:stop] [1:thinking] +thinking_delta"still thinking" [1:stop]`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A pristine thinking block followed by text (clean sglang shape) is
// byte-identical, deltas arriving in odd network-sized chunks.
func TestSSECleanStreamOddNetworkChunks(t *testing.T) {
	in := msgStart() +
		blkStart(0, "thinking") + thinkDelta(0, "abc") + thinkDelta(0, "def") + blkStop(0) +
		blkStart(1, "text") + textDelta(1, "xy") + blkStop(1) +
		msgEnd()
	// Chop the byte stream arbitrarily; output must equal the input.
	for _, step := range []int{1, 3, 7, 64} {
		r := NewAnthropicSSERewriter(&chunkReader{s: in, step: step})
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Fatalf("step %d: clean stream modified", step)
		}
	}
}

// chunkReader returns s in step-sized reads to exercise frame reassembly.
type chunkReader struct {
	s    string
	step int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := r.step
	if n > len(r.s) {
		n = len(r.s)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}
