// Package thinkfix translates leaked reasoning-section markers into API-native
// thinking content for the Anthropic Messages API.
//
// Some inference backends (notably sglang when its reasoning parser does not
// match the served model) decode the model's reasoning-section special tokens
// into visible text, e.g. Kimi-K3 emitting
//
//	<|open|>think<|sep|<reasoning><|close|>think<|sep|<answer>
//
// inside ordinary text deltas. Clients then display the raw markers. This
// package re-classifies such text into thinking/text segments so the proxy can
// emit proper Anthropic thinking blocks instead.
//
// The translation is marker-triggered and inert by design: a response that
// contains no markers (vLLM with its token-ID reasoning parser, or a correctly
// configured sglang) passes through unchanged, at the cost of a string scan.
//
// Known limitation of any text-level approach: if a model deliberately emits
// the marker strings as content (e.g. explaining this very feature), that text
// is misclassified as a thinking boundary. Token-ID parsers (vLLM) do not have
// this problem, but theirs is the only level where markers are distinguishable
// from literal text. The marker strings are special-token renderings that
// essentially never occur in legitimate output, so the trade-off favors
// translating.
package thinkfix

import "strings"

// Marker strings, exactly as rendered by the misconfigured backend.
const (
	thinkOpenMarker  = "<|open|>think<|sep|"
	thinkCloseMarker = "<|close|>think<|sep|"
)

// Segment is a piece of response text attributed to either the thinking
// section or the visible answer.
type Segment struct {
	Thinking bool
	Text     string
}

// Scanner incrementally classifies a text stream into thinking/text segments.
// It holds back at most len(marker)-1 bytes — a suffix that might be the start
// of a marker split across Feed calls — so latency impact is negligible.
type Scanner struct {
	inThink bool
	pending string
	flipped bool // a marker has been consumed at least once
}

func NewScanner() *Scanner { return &Scanner{} }

// Flipped reports whether any marker has been consumed. Callers use it for
// fast-path passthrough: as long as Flipped is false, the concatenation of all
// emitted segments equals the concatenation of all Feed inputs.
func (s *Scanner) Flipped() bool { return s.flipped }

// Seed resets the scanner's expectation to match a new content block: upstream
// block boundaries redefine the context, so any marker fragment straddling the
// boundary is discarded and the next marker to match is open (text block) or
// close (thinking block).
func (s *Scanner) Seed(inThink bool) {
	s.inThink = inThink
	s.pending = ""
}

func (s *Scanner) marker() string {
	if s.inThink {
		return thinkCloseMarker
	}
	return thinkOpenMarker
}

// Feed consumes a chunk of text and returns the fully classified segments
// (possibly empty, while a potential marker is held back).
func (s *Scanner) Feed(chunk string) []Segment {
	data := s.pending + chunk
	s.pending = ""
	var out []Segment
	for len(data) > 0 {
		m := s.marker()
		idx := strings.Index(data, m)
		if idx < 0 {
			keep := markerPrefixOverlap(data, m)
			if emit := data[:len(data)-keep]; emit != "" {
				out = append(out, Segment{Thinking: s.inThink, Text: emit})
			}
			s.pending = data[len(data)-keep:]
			break
		}
		if idx > 0 {
			out = append(out, Segment{Thinking: s.inThink, Text: data[:idx]})
		}
		s.inThink = !s.inThink
		s.flipped = true
		data = data[idx+len(m):]
	}
	return out
}

// Flush drops any held-back partial marker. Trailing fragments are truncation
// artifacts (generation stopped mid-marker), never legitimate content, so they
// are discarded. Flush reports whether anything was dropped.
func (s *Scanner) Flush() (dropped bool) {
	dropped = s.pending != ""
	s.pending = ""
	return dropped
}

// markerPrefixOverlap returns the length of the longest suffix of data that is
// a proper prefix of m. data is known NOT to contain m, so the overlap is
// always shorter than len(m).
func markerPrefixOverlap(data, m string) int {
	max := len(m) - 1
	if len(data) < max {
		max = len(data)
	}
	for k := max; k > 0; k-- {
		if data[len(data)-k:] == m[:k] {
			return k
		}
	}
	return 0
}
