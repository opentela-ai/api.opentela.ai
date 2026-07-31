package thinkfix

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// AnthropicSSERewriter wraps a text/event-stream response body for the
// Anthropic Messages API and reclassifies leaked reasoning markers into proper
// thinking blocks. Until a marker is seen, the stream passes through
// byte-for-byte; afterwards the stream is restructured (block boundaries and
// indices are remapped) and stays in rewrite mode for the rest of the
// response.
//
// Rewriting a stream means closing the current content block when a marker
// flips the section kind and opening a new block of the other kind.
// Downstream block indices are re-allocated densely (0, 1, 2, …), which the
// Messages protocol requires; upstream indices are never reused after a flip.
type AnthropicSSERewriter struct {
	src    *bufio.Reader
	out    bytes.Buffer
	sc     *Scanner
	closed bool

	rewriting bool

	// raw mode (pre-marker): track the upstream block currently flowing so a
	// first marker can synthesize its stop before opening the thinking block.
	rawIdx  int
	rawKind string // "text" or "thinking"

	// rewrite mode
	nextSynth int
	cur       *synthBlock      // currently open downstream block, if any
	blocks    map[int]*upBlock // upstream index -> state
}

type synthBlock struct {
	idx      int
	thinking bool
	up       int
}

type upBlock struct {
	kind       string // upstream content_block type
	synthIdx   int    // downstream index currently used for this block
	started    bool   // a downstream start has been emitted
	otherBlock bool   // non text/thinking (e.g. tool_use): remapped verbatim
}

// NewAnthropicSSERewriter returns an io.Reader yielding the rewritten stream.
func NewAnthropicSSERewriter(r io.Reader) io.Reader {
	return &AnthropicSSERewriter{
		src:    bufio.NewReader(r),
		sc:     NewScanner(),
		blocks: map[int]*upBlock{},
	}
}

func (w *AnthropicSSERewriter) Read(p []byte) (int, error) {
	for w.out.Len() == 0 {
		if w.closed {
			return 0, io.EOF
		}
		if err := w.step(); err != nil {
			if err == io.EOF {
				w.finish()
				w.closed = true
				continue
			}
			return 0, err
		}
	}
	return w.out.Read(p)
}

// finish runs end-of-stream cleanup: drop a truncated marker tail and close a
// dangling synthetic block so downstream sees a well-formed stream ending.
func (w *AnthropicSSERewriter) finish() {
	w.sc.Flush()
	if w.cur != nil {
		w.emitEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": w.cur.idx,
		})
		w.cur = nil
	}
}

// step reads one SSE frame from the source and processes it.
func (w *AnthropicSSERewriter) step() error {
	frame, err := readSSEFrame(w.src)
	if err != nil {
		return err
	}
	name, dataFrame, ok := parseSSEFrame(frame)
	if !ok {
		// Comments, keep-alives, multi-line data and anything unusual: verbatim.
		w.out.Write(frame)
		return nil
	}
	var msg map[string]any
	if err := json.Unmarshal(dataFrame, &msg); err != nil {
		w.out.Write(frame)
		return nil
	}
	typ, _ := msg["type"].(string)
	if name == "" {
		name = typ
	}
	switch typ {
	case "content_block_start":
		w.onBlockStart(name, frame, msg)
	case "content_block_delta":
		w.onBlockDelta(name, frame, msg)
	default:
		// content_block_stop, message_start, message_delta, ping, …
		if typ == "content_block_stop" {
			w.onBlockStop(frame, msg)
		} else {
			if typ == "message_stop" && w.cur != nil {
				w.emitEvent("content_block_stop", map[string]any{
					"type": "content_block_stop", "index": w.cur.idx,
				})
				w.cur = nil
			}
			w.out.Write(frame)
		}
	}
	return nil
}

// onBlockStart handles content_block_start.
func (w *AnthropicSSERewriter) onBlockStart(name string, frame []byte, msg map[string]any) {
	idx := asInt(msg["index"])
	cb, _ := msg["content_block"].(map[string]any)
	kind, _ := cb["type"].(string)
	scannable := kind == "text" || kind == "thinking"

	if !w.rewriting {
		// Verbatim passthrough; remember the block for first-flip synthesis.
		w.rawIdx = idx
		if scannable {
			w.rawKind = kind
			w.sc.Seed(kind == "thinking")
		} else {
			w.rawKind = ""
		}
		w.out.Write(frame)
		return
	}

	if w.cur != nil { // protocol says upstream stops first; close defensively
		w.closeCur()
	}
	up := &upBlock{kind: kind, synthIdx: -1}
	w.blocks[idx] = up
	if !scannable {
		// Tool blocks & co: forwarded verbatim except the index, which must be
		// a fresh downstream index. Thinking-section state is unaffected:
		// markers are not scanned inside tool JSON.
		up.otherBlock = true
		up.synthIdx = w.allocIdx()
		up.started = true
		msg["index"] = up.synthIdx
		w.emitEvent(name, msg)
		w.cur = &synthBlock{idx: up.synthIdx, thinking: false, up: idx}
		return
	}
	// Text/thinking: defer the start until the first delta disambiguates the
	// kind (a text block may open with a think marker and vice versa).
	w.sc.Seed(kind == "thinking")
}

// onBlockDelta handles content_block_delta.
func (w *AnthropicSSERewriter) onBlockDelta(name string, frame []byte, msg map[string]any) {
	idx := asInt(msg["index"])
	delta, _ := msg["delta"].(map[string]any)
	dtype, _ := delta["type"].(string)

	var text string
	switch dtype {
	case "text_delta":
		text, _ = delta["text"].(string)
	case "thinking_delta":
		text, _ = delta["thinking"].(string)
	default:
		// input_json_delta (tool args): no scanning. Forward, remapping the
		// index when rewriting.
		if w.rewriting {
			if up := w.blocks[idx]; up != nil {
				msg["index"] = up.synthIdx
				w.emitEvent(name, msg)
				return
			}
		}
		w.out.Write(frame)
		return
	}

	if !w.rewriting {
		segs := w.sc.Feed(text)
		if !w.sc.Flipped() {
			// Marker-free so far. The scanner may be holding back a suffix of
			// this delta that could yet become a marker; those bytes must NOT
			// go out yet, or a marker completing in a later delta would leak
			// its first half verbatim. Identity fast-path: if the classified
			// output equals the input text, forward the original frame.
			emitted := ""
			for _, s := range segs {
				emitted += s.Text
			}
			if emitted == text {
				w.out.Write(frame)
				return
			}
			if emitted == "" {
				return // whole delta held back as a potential marker
			}
			field := "text"
			if dtype == "thinking_delta" {
				field = "thinking"
			}
			w.emitEvent(name, map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": dtype, field: emitted},
			})
			return
		}
		// First marker seen: switch this response into rewrite mode. The raw
		// block already flowing upstream index idx keeps its index until the
		// first kind change; downstream indices after that are allocated from
		// idx+1. Upstream may not have advertised the current block as idx (it
		// should), but rawIdx tracks the last start we saw.
		w.rewriting = true
		w.nextSynth = idx + 1
		up := &upBlock{kind: w.rawKind, synthIdx: idx, started: true}
		w.blocks[idx] = up
		w.cur = &synthBlock{idx: idx, thinking: w.rawKind == "thinking", up: idx}
		w.emitSegments(name, up, segs)
		return
	}

	up := w.blocks[idx]
	if up == nil {
		// No start seen for this index (protocol violation): safest is
		// verbatim for this frame.
		w.out.Write(frame)
		return
	}
	if up.otherBlock {
		msg["index"] = up.synthIdx
		w.emitEvent(name, msg)
		return
	}
	w.emitSegments(name, up, w.sc.Feed(text))
}

// emitSegments writes classified segments as content_block_delta events,
// opening/closing synthetic blocks so each segment lands in a block of the
// matching kind.
func (w *AnthropicSSERewriter) emitSegments(name string, up *upBlock, segs []Segment) {
	// Merge adjacent same-kind segments to keep the event count sane.
	merged := segs[:0]
	for _, s := range segs {
		if n := len(merged); n > 0 && merged[n-1].Thinking == s.Thinking {
			merged[n-1].Text += s.Text
		} else {
			merged = append(merged, s)
		}
	}
	for _, s := range merged {
		w.ensureOpen(up, s.Thinking)
		if s.Text == "" {
			continue // a bare marker with no surrounding text: flip only
		}
		kindField, dtype := "text", "text_delta"
		if s.Thinking {
			kindField, dtype = "thinking", "thinking_delta"
		}
		w.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.cur.idx,
			"delta": map[string]any{"type": dtype, kindField: s.Text},
		})
	}
}

// ensureOpen makes a downstream block of the wanted kind the current open
// block, closing the previous one when the kind changes.
func (w *AnthropicSSERewriter) ensureOpen(up *upBlock, thinking bool) {
	if w.cur != nil && w.cur.up == upIndex(up, w) && w.cur.thinking == thinking {
		return
	}
	if w.cur != nil {
		w.closeCur()
	}
	idx := w.allocIdx()
	up.synthIdx = idx
	up.started = true
	kind, empty := "text", ""
	if thinking {
		kind = "thinking"
	}
	w.emitEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": kind, kind: empty},
	})
	w.cur = &synthBlock{idx: idx, thinking: thinking, up: upIndex(up, w)}
}

func (w *AnthropicSSERewriter) closeCur() {
	w.emitEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": w.cur.idx,
	})
	w.cur = nil
}

// onBlockStop handles content_block_stop.
func (w *AnthropicSSERewriter) onBlockStop(frame []byte, msg map[string]any) {
	if !w.rewriting {
		w.out.Write(frame)
		return
	}
	idx := asInt(msg["index"])
	up := w.blocks[idx]
	if up == nil {
		w.out.Write(frame)
		return
	}
	delete(w.blocks, idx)
	if up.otherBlock {
		// Current open block belongs to this upstream block: close it.
		if w.cur != nil && w.cur.up == idx {
			w.closeCur()
		} else {
			w.emitEvent("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": up.synthIdx,
			})
		}
		return
	}
	if w.cur != nil && w.cur.up == idx {
		w.closeCur()
		return
	}
	// A text/thinking block whose start was deferred and that never produced a
	// delta: preserve it as an (empty) block of its original kind.
	if !up.started {
		kind := up.kind
		synth := w.allocIdx()
		w.emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": synth,
			"content_block": map[string]any{"type": kind, kind: ""},
		})
		w.emitEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": synth,
		})
		return
	}
	// Its synthetic blocks were already closed by a flip; nothing to emit.
}

func (w *AnthropicSSERewriter) allocIdx() int {
	i := w.nextSynth
	w.nextSynth++
	return i
}

// upIndex returns the upstream index for a block state (reverse lookup only
// used on the ensureOpen path where identity is what matters).
func upIndex(up *upBlock, w *AnthropicSSERewriter) int {
	for i, b := range w.blocks {
		if b == up {
			return i
		}
	}
	return -1
}

func (w *AnthropicSSERewriter) emitEvent(name string, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return // payload shapes are statically known; marshal cannot fail
	}
	if name == "" {
		fmt.Fprintf(&w.out, "data: %s\n\n", data)
		return
	}
	fmt.Fprintf(&w.out, "event: %s\ndata: %s\n\n", name, data)
}

// readSSEFrame returns the next SSE frame, including its terminating blank
// line. A final unterminated frame (stream cut) is returned as-is.
func readSSEFrame(r *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		frame.WriteString(line)
		if err != nil {
			if frame.Len() > 0 && err == io.EOF {
				return frame.Bytes(), nil
			}
			return nil, err
		}
		if strings.TrimRight(line, "\r\n") == "" {
			return frame.Bytes(), nil
		}
	}
}

// parseSSEFrame extracts the event name and single-line data payload. ok=false
// means the frame does not follow the simple event:/data: shape (comments,
// multi-line data, field lines) and should pass through untouched.
func parseSSEFrame(frame []byte) (name string, data []byte, ok bool) {
	for _, line := range strings.Split(strings.TrimRight(string(frame), "\r\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			if name != "" {
				return "", nil, false
			}
			name = strings.TrimSpace(line[len("event: "):])
		case strings.HasPrefix(line, "data: "):
			if data != nil {
				return "", nil, false
			}
			data = []byte(line[len("data: "):])
		default:
			return "", nil, false
		}
	}
	if data == nil {
		return "", nil, false
	}
	return name, data, true
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
