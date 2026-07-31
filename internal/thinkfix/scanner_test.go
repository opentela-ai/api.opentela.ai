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

// Marker strings never appear in real content: a sanity check on their shape
// that guards against future refactors loosening the constants.
func TestMarkerShape(t *testing.T) {
	for _, m := range []string{thinkOpenMarker, thinkCloseMarker} {
		if !strings.HasPrefix(m, "<|") || !strings.HasSuffix(m, "<|sep|") {
			t.Fatalf("marker %q lost its special-token shape", m)
		}
		if !strings.Contains(m, "think") {
			t.Fatalf("marker %q lost its section name", m)
		}
	}
}
