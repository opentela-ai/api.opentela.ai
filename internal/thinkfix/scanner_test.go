package thinkfix

import (
	"strings"
	"testing"
)

func feedAll(sc *Scanner, chunks ...string) []Segment {
	var out []Segment
	for _, c := range chunks {
		out = append(out, sc.Feed(c)...)
	}
	return out
}

func join(segs []Segment, thinking bool) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Thinking == thinking {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

func TestScannerNoMarkers(t *testing.T) {
	sc := NewScanner()
	input := "Hello, world! Markers like <|open|> without the rest are fine; even this: <|ope|"
	segs := feedAll(sc, input)
	if sc.Flipped() {
		t.Fatal("flipped without a full marker")
	}
	if got := join(segs, false); got != input {
		t.Fatalf("identity broken: %q", got)
	}
}

func TestScannerSingleSection(t *testing.T) {
	sc := NewScanner()
	segs := feedAll(sc, "<|open|>think<|sep|let me reason<|close|>think<|sep|the answer")
	if !sc.Flipped() {
		t.Fatal("marker not detected")
	}
	if got := join(segs, true); got != "let me reason" {
		t.Fatalf("thinking = %q", got)
	}
	if got := join(segs, false); got != "the answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestScannerMultipleSections(t *testing.T) {
	sc := NewScanner()
	input := "pre <|open|>think<|sep|t1<|close|>think<|sep|mid <|open|>think<|sep|t2<|close|>think<|sep|post"
	segs := feedAll(sc, input)
	if got, want := join(segs, true), "t1t2"; got != want {
		t.Fatalf("thinking = %q, want %q", got, want)
	}
	if got, want := join(segs, false), "pre mid post"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// Any chunking of the same input must produce the same classification.
func TestScannerSplitInvariant(t *testing.T) {
	input := "start <|open|>think<|sep|deep thoughts <|close|>think<|sep|visible <|open|>think<|sep|more<|close|>think<|sep|end"
	want := NewScanner()
	wantSegs := feedAll(want, input)
	wantThink, wantText := join(wantSegs, true), join(wantSegs, false)

	for i := 0; i < len(input); i++ {
		sc := NewScanner()
		segs := feedAll(sc, input[:i], input[i:])
		if got := join(segs, true); got != wantThink {
			t.Fatalf("split at %d: thinking %q, want %q", i, got, wantThink)
		}
		if got := join(segs, false); got != wantText {
			t.Fatalf("split at %d: text %q, want %q", i, got, wantText)
		}
	}
	// And byte-at-a-time.
	sc := NewScanner()
	var segs []Segment
	for i := 0; i < len(input); i++ {
		segs = append(segs, sc.Feed(input[i:i+1])...)
	}
	if got := join(segs, true); got != wantThink {
		t.Fatalf("bytewise thinking %q, want %q", got, wantThink)
	}
	if got := join(segs, false); got != wantText {
		t.Fatalf("bytewise text %q, want %q", got, wantText)
	}
}

// Generation truncated mid-marker: the fragment is dropped, not delivered.
func TestScannerFlushDropsPartialMarker(t *testing.T) {
	sc := NewScanner()
	sc.Seed(true) // inside a thinking block, awaiting the close marker
	segs := feedAll(sc, "reasoning so far <|close|>")
	if got := join(segs, true); got != "reasoning so far " {
		t.Fatalf("got %q held-back violation", got)
	}
	if !sc.Flush() {
		t.Fatal("expected pending partial marker to be dropped")
	}
	// A chunk ending inside a marker candidate is held back, then delivered
	// once the next chunk proves it was not a marker.
	sc2 := NewScanner()
	segs2 := feedAll(sc2, "plain text with <|open|>")
	if got := join(segs2, false); got != "plain text with " {
		t.Fatalf("expected hold-back of the marker candidate: %q", got)
	}
	segs2 = append(segs2, sc2.Feed(" but no section")...)
	if got := join(segs2, false); got != "plain text with <|open|> but no section" {
		t.Fatalf("held-back text not redelivered: %q", got)
	}
	if sc2.Flush() {
		t.Fatal("nothing should remain pending after redelivery")
	}
}

// Seed (a new upstream block) abandons a straddling marker fragment.
func TestScannerSeedResets(t *testing.T) {
	sc := NewScanner()
	feedAll(sc, "text <|ope")
	sc.Seed(true)
	segs := feedAll(sc, "thinking<|close|>think<|sep|after")
	if got := join(segs, true); got != "thinking" {
		t.Fatalf("thinking = %q", got)
	}
	if got := join(segs, false); got != "after" {
		t.Fatalf("text = %q", got)
	}
}

// Marker and separator strings never appear in real content: a sanity check
// on their shape that guards against future refactors loosening the constants.
func TestMarkerShape(t *testing.T) {
	for _, m := range []string{thinkOpenMarker, thinkCloseMarker} {
		if !strings.HasPrefix(m, "<|") || !strings.HasSuffix(m, "think") {
			t.Fatalf("marker %q lost its special-token shape", m)
		}
	}
	for _, s := range []string{sepBareToken, sepFullToken} {
		if !strings.HasPrefix(s, "<|") || !strings.Contains(s, "sep") {
			t.Fatalf("separator %q lost its special-token shape", s)
		}
	}
	if !strings.HasSuffix(sepFullToken, ">") || sepFullToken[:len(sepBareToken)] != sepBareToken {
		t.Fatalf("separators must differ only by the closing bracket: %q vs %q", sepBareToken, sepFullToken)
	}
}

// The observed double-separator leak form: "<|open|>think" followed by a bare
// then a full separator token real content starts only after both.
func TestScannerDoubleSeparatorLeak(t *testing.T) {
	sc := NewScanner()
	segs := feedAll(sc, "<|open|>think<|sep|<|sep|>Check a couple more.\n<|close|>think<|sep|<|sep|>Here's the summary.")
	if got := join(segs, true); got != "Check a couple more.\n" {
		t.Fatalf("thinking = %q", got)
	}
	if got := join(segs, false); got != "Here's the summary." {
		t.Fatalf("text = %q", got)
	}
}

// Observed compaction-style turn: the close marker is followed by a space and
// the answer directly, without any separator token.
func TestScannerCloseMarkerWithoutSeparator(t *testing.T) {
	sc := NewScanner()
	sc.Seed(true)
	segs := feedAll(sc, "done <|close|>think the user wants forty-two")
	if got := join(segs, true); got != "done " {
		t.Fatalf("thinking = %q", got)
	}
	if got := join(segs, false); got != " the user wants forty-two" {
		t.Fatalf("text = %q", got)
	}
}

// A lone separator at the start of a fresh block (a backend that half-applied
// the split itself): stripped even though no marker ever appears.
func TestScannerBareSeparatorAtBlockStart(t *testing.T) {
	sc := NewScanner()
	sc.Seed(false)
	var segs []Segment
	for _, c := range []string{"<|sep|>Here's a summary "} {
		segs = append(segs, sc.Feed(c)...)
	}
	segs = append(segs, sc.Feed("of your home directory.")...)
	if sc.Flipped() {
		t.Fatal("bare separator must not flip sections")
	}
	if got := join(segs, false); got != "Here's a summary of your home directory." {
		t.Fatalf("text = %q", got)
	}
}

// Separator stripping must survive chunks that split the separator itself,
// and must not eat lookalike text such as "<|self|>".
func TestScannerSeparatorHoldback(t *testing.T) {
	sc := NewScanner()
	sc.Seed(false)
	if segs := sc.Feed("<|se"); len(segs) != 0 {
		t.Fatalf("partial separator emitted: %+v", segs)
	}
	segs := sc.Feed("p|>Hi")
	if got := join(segs, false); got != "Hi" {
		t.Fatalf("text = %q", got)
	}

	sc2 := NewScanner()
	sc2.Seed(false)
	if segs := sc2.Feed("<|se"); len(segs) != 0 {
		t.Fatalf("partial lookalike held: %+v", segs)
	}
	segs2 := sc2.Feed("lf|> is mentioned in this text")
	if got := join(segs2, false); got != "<|self|> is mentioned in this text" {
		t.Fatalf("lookalike was mangled: %q", got)
	}
}

// A separator mid-section is content, not stripped: only section starts
// are cleaned.
func TestScannerSeparatorMidSectionPreserved(t *testing.T) {
	sc := NewScanner()
	sc.Seed(false)
	segs := feedAll(sc, "before <|sep|> after")
	if got := join(segs, false); got != "before <|sep|> after" {
		t.Fatalf("mid-section separator must survive: %q", got)
	}
}

// A thinking block opened via Seed still gets its leading separator stripped
// and the close marker found afterwards.
func TestScannerSeedThenStripThenClose(t *testing.T) {
	sc := NewScanner()
	sc.Seed(true)
	segs := feedAll(sc, "<|sep|reasoning<|close|>think<|sep|>answer")
	if got := join(segs, true); got != "reasoning" {
		t.Fatalf("thinking = %q", got)
	}
	if got := join(segs, false); got != "answer" {
		t.Fatalf("text = %q", got)
	}
}
