// Package thinkfix translates leaked reasoning-section markers into API-native
// thinking content for the Anthropic Messages API.
//
// Some inference backends (notably sglang when its reasoning parser does not
// match the served model) decode the model's reasoning-section special tokens
// into visible text, e.g. Kimi-K3 emitting
//
//	<|open|>think<|sep|<|sep|><reasoning><|close|>think<|sep|<|sep|><answer>
//
// inside ordinary text deltas. Clients then display the raw markers. This
// package re-classifies such text into thinking/text segments so the proxy can
// emit proper Anthropic thinking blocks instead.
//
// Two token shapes are handled at two levels:
//
//   - Section markers <|open|>think and <|close|>think switch the attribution
//     of what follows (text vs thinking). The trailing separator is NOT part
//     of the marker: model output has been observed both with and without it
//     (compaction-style turns emit "<|close|>think the answer" directly).
//   - The separator token, rendered by the model in two forms — bare
//     "<|sep|" (no closing bracket) and full "<|sep|>" — which regularly
//     appears doubled after a section marker and occasionally alone at the
//     start of a block emitted by a half-working backend split. It is stripped
//     wherever it opens a section, but left untouched mid-content, where it
//     could — in principle — be literal model text.
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

// Marker strings, exactly as rendered by the misconfigured backend. The
// separator is handled separately from the markers and in both observed
// renderings: leaks around markers appear with the separator absent,
// bare ("<|sep|"), full ("<|sep|>"), or doubled in mixed form.
const (
	thinkOpenMarker  = "<|open|>think"
	thinkCloseMarker = "<|close|>think"
	sepBareToken     = "<|sep|"
	sepFullToken     = "<|sep|>"
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
// Additionally, at the start of every section (stream start, upstream block
// boundary, or just after a consumed marker) it strips any number of leading
// separator tokens, holding back bytes while a partial separator could still
// complete in a later chunk.
type Scanner struct {
	inThink bool
	pending string
	flipped bool // a marker has been consumed at least once
	atStart bool // at a section start: strip leading separator tokens
}

func NewScanner() *Scanner { return &Scanner{} }

// Flipped reports whether any marker has been consumed. Callers use it for
// fast-path passthrough: as long as Flipped is false, the concatenation of all
// emitted segments equals the concatenation of all Feed inputs.
func (s *Scanner) Flipped() bool { return s.flipped }

// Seed resets the scanner's expectation to match a new content block: upstream
// block boundaries redefine the context, so any marker fragment straddling the
// boundary is discarded and the next marker to match is open (text block) or
// close (thinking block). The new block's first bytes go through separator
// stripping, mirroring what happens after a consumed marker.
func (s *Scanner) Seed(inThink bool) {
	s.inThink = inThink
	s.pending = ""
	s.atStart = true
}

func (s *Scanner) marker() string {
	if s.inThink {
		return thinkCloseMarker
	}
	return thinkOpenMarker
}

// Feed consumes a chunk of text and returns the fully classified segments
// (possibly empty, while a potential marker or separator is held back).
func (s *Scanner) Feed(chunk string) []Segment {
	data := s.pending + chunk
	s.pending = ""
	var out []Segment
	for len(data) > 0 {
		if s.atStart {
			rest, decided := stripLeadingSeps(data)
			if !decided {
				// Only separators so far (or a trailing fragment that might
				// complete one): keep holding until more input resolves it.
				s.pending = data
				return out
			}
			s.atStart = false
			data = rest
			if len(data) == 0 {
				break
			}
		}
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
		s.atStart = true // the section after a marker opens with separators
		data = data[idx+len(m):]
	}
	return out
}

// stripLeadingSeps removes leading separator tokens (both renderings, any
// count) from d. decided is false when everything in d is either consumed
// separators or a remainder that is a strict prefix of one — i.e. more input
// could still extend it — in which case d is returned unchanged and should be
// held. A complete bare "<|sep|" is always strippable on its own: whether or
// not a ">" follows, the result is identical.
func stripLeadingSeps(d string) (rest string, decided bool) {
	orig := d
	for {
		switch {
		case strings.HasPrefix(d, sepFullToken):
			d = d[len(sepFullToken):]
		case strings.HasPrefix(d, sepBareToken):
			d = d[len(sepBareToken):]
		default:
			if d == "" || strings.HasPrefix(sepBareToken, d) {
				return orig, false
			}
			return d, true
		}
	}
}

// Flush drops any held-back partial marker or undecided separator bytes.
// Trailing fragments are truncation artifacts (generation stopped mid-token),
// never legitimate content, so they are discarded. Flush reports whether
// anything was dropped.
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
