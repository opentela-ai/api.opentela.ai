package perf

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Token usage and model identity are read out of the response body as it
// streams past, for the two dialects the mesh serves: OpenAI
// (chat/completions & completions, SSE or JSON) and Anthropic Messages
// (SSE or JSON). Streams are parsed incrementally without buffering beyond a
// single line; non-stream JSON is captured up to jsonCaptureCap and parsed at
// end of body. Anything else (binary, unknown encodings, malformed frames)
// degrades to byte/timing-only samples — never to an error on the hot path.

const (
	// jsonCaptureCap bounds the buffered bytes of a non-streaming body parsed
	// for usage. Past the cap the body still passes through untouched; token
	// counts are simply not extracted.
	jsonCaptureCap = 256 << 10
	// sseLineCap skips JSON-parsing of a single overlong SSE line (e.g. huge
	// tool-call argument frames) without derailing the rest of the stream.
	sseLineCap = 1 << 20
)

type parserKind int

const (
	parserNone parserKind = iota
	parserSSE
	parserJSON
)

// streamParser accumulates model/usage/first-token observations.
type streamParser struct {
	kind parserKind

	// SSE incremental state.
	line     []byte // partial current line
	skipLine bool   // current line exceeded sseLineCap

	// JSON capture state.
	body   []byte
	parsed bool // finish() ran; body parsing is idempotent

	model        string
	inputTokens  int
	outputTokens int
	sawToken     bool // a generated-content token delta was observed
}

func newStreamParser(contentType string) *streamParser {
	switch {
	case strings.HasPrefix(contentType, "text/event-stream"):
		return &streamParser{kind: parserSSE}
	case strings.HasPrefix(contentType, "application/json"):
		return &streamParser{kind: parserJSON}
	default:
		return &streamParser{kind: parserNone}
	}
}

// feed observes bytes flowing to the client.
func (p *streamParser) feed(b []byte) {
	switch p.kind {
	case parserSSE:
		p.feedSSE(b)
	case parserJSON:
		if len(p.body) < jsonCaptureCap {
			keep := jsonCaptureCap - len(p.body)
			if keep > len(b) {
				keep = len(b)
			}
			p.body = append(p.body, b[:keep]...)
		}
	}
}

// finish parses a fully captured JSON body. SSE streams need no end-of-body
// step: every frame was handled as it completed.
func (p *streamParser) finish() {
	if p.parsed {
		return
	}
	p.parsed = true
	if p.kind == parserJSON && len(p.body) > 0 && p.body[len(p.body)-1] == '}' {
		p.observe(payloadProbe(p.body))
	}
}

func (p *streamParser) feedSSE(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		var chunk []byte
		if i < 0 {
			chunk, b = b, nil
		} else {
			chunk, b = b[:i+1], b[i+1:]
		}
		switch {
		case p.skipLine:
			// Discarding an overlong line until its terminator.
			if i >= 0 {
				p.skipLine = false
				p.line = p.line[:0]
			}
		case len(p.line)+len(chunk) > sseLineCap:
			p.skipLine = i < 0 // whole line already consumed when it ended here
			p.line = p.line[:0]
		default:
			p.line = append(p.line, chunk...)
			if i >= 0 {
				p.observeLine(p.line)
				p.line = p.line[:0]
			}
		}
	}
}

// observeLine handles one complete SSE line (including its newline). Only
// data: lines matter; event names, comments and keep-alives are redundant for
// extraction — OpenAI frames are self-describing, and Anthropic frames carry
// their "type" inside the JSON payload.
func (p *streamParser) observeLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	p.observe(payloadProbe(payload))
}

// probe is the union of the fields this package extracts from either API
// dialect, decoded with the standard (permissive) unmarshaler: unknown fields
// — the actual content! — are skipped.
type probe struct {
	Model   string `json:"model"`
	Type    string `json:"type"`
	Usage   *usage `json:"usage"`
	Message *struct {
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	} `json:"message"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Delta struct {
		Type string `json:"type"`
	} `json:"delta"`
}

type usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func payloadProbe(payload []byte) probe {
	var pr probe
	_ = json.Unmarshal(payload, &pr)
	return pr
}

// observe folds one decoded frame (SSE) or full body (JSON) into the parser
// state.
func (p *streamParser) observe(pr probe) {
	if pr.Message != nil {
		if pr.Message.Model != "" {
			p.model = pr.Message.Model
		}
		if u := pr.Message.Usage; u != nil {
			p.inputTokens = max(p.inputTokens, u.InputTokens)
			p.outputTokens = max(p.outputTokens, u.OutputTokens)
		}
	}
	if pr.Model != "" {
		p.model = pr.Model
	}
	if u := pr.Usage; u != nil {
		p.inputTokens = max(p.inputTokens, u.InputTokens, u.PromptTokens)
		p.outputTokens = max(p.outputTokens, u.OutputTokens, u.CompletionTokens)
	}
	switch {
	case len(pr.Choices) > 0 &&
		(pr.Choices[0].Delta.Content != "" || pr.Choices[0].Delta.ReasoningContent != ""):
		// OpenAI streaming content (reasoning_content: vLLM reasoning models).
		p.sawToken = true
	case pr.Type == "content_block_delta" &&
		(pr.Delta.Type == "text_delta" || pr.Delta.Type == "thinking_delta"):
		// Anthropic streaming content.
		p.sawToken = true
	case p.kind == parserJSON && p.outputTokens > 0:
		// Non-streaming bodies arrive whole: completion equals first token for
		// our purposes, but first-token timing stays 0 (undefined) so tps math
		// falls back to total latency.
		p.sawToken = true
	}
}
