package thinkfix

import "encoding/json"

// MaxJSONRewriteBody caps the buffered non-streaming response body eligible
// for rewriting. Larger bodies pass through untouched rather than growing
// gateway memory.
const MaxJSONRewriteBody = 8 << 20 // 8 MiB

// RewriteAnthropicMessage rewrites a non-streaming Anthropic Messages
// response body: text blocks containing leaked reasoning markers are split
// into thinking/text blocks, and separator remnants at the start of text or
// thinking blocks (emitted by backends that half-apply the split themselves)
// are stripped. It reports changed=false (and returns the input bytes
// unchanged) when neither was found — the common case — so callers can return
// the original body verbatim.
func RewriteAnthropicMessage(body []byte) (out []byte, changed bool, err error) {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, false, err
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return body, false, nil
	}

	sc := NewScanner()
	newContent := make([]any, 0, len(content))
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			newContent = append(newContent, item)
			continue
		}
		kind, _ := block["type"].(string)
		scannable := kind == "text" || kind == "thinking"
		if !scannable {
			newContent = append(newContent, item)
			continue
		}
		sc.Seed(kind == "thinking")
		field := "text"
		if kind == "thinking" {
			field = "thinking"
		}
		text, _ := block[field].(string)
		segs := sc.Feed(text)
		if !sc.Flipped() {
			// No section markers: keep the block unless section-start
			// separator tokens or a truncation tail were stripped.
			joined := ""
			for _, s := range segs {
				joined += s.Text
			}
			if joined == text {
				newContent = append(newContent, item)
				continue
			}
			block[field] = joined
			newContent = append(newContent, block)
			changed = true
			continue
		}
		for _, s := range mergeSegments(segs) {
			if s.Text == "" {
				continue
			}
			if s.Thinking {
				newContent = append(newContent, map[string]any{
					"type": "thinking", "thinking": s.Text,
				})
			} else {
				newContent = append(newContent, map[string]any{
					"type": "text", "text": s.Text,
				})
			}
		}
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	msg["content"] = newContent
	out, err = json.Marshal(msg)
	return out, true, err
}

func mergeSegments(segs []Segment) []Segment {
	merged := segs[:0]
	for _, s := range segs {
		if n := len(merged); n > 0 && merged[n-1].Thinking == s.Thinking {
			merged[n-1].Text += s.Text
		} else {
			merged = append(merged, s)
		}
	}
	return merged
}
