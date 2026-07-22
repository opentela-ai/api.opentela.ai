package store

import "testing"

func TestHashKeyKnownVector(t *testing.T) {
	// SHA-256("test") is a well-known digest.
	const want = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if got := HashKey("test"); got != want {
		t.Fatalf("HashKey(\"test\") = %q, want %q", got, want)
	}
}

func TestHashKeyDeterministicAndDistinct(t *testing.T) {
	if HashKey("a") != HashKey("a") {
		t.Error("HashKey not deterministic")
	}
	if HashKey("a") == HashKey("b") {
		t.Error("HashKey collided on distinct inputs")
	}
}
