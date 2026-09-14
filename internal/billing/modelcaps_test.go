package billing

import (
	"testing"
)

func intPtr(v int64) *int64 { return &v }

func TestEffectiveModelOverridesTierByTier(t *testing.T) {
	flat := Caps{InputPerMillion: intPtr(100), CachedInputPerMillion: intPtr(30), OutputPerMillion: intPtr(300)}
	rows := []ModelCaps{
		{Service: "llm", Model: "m1", OutputPerMillion: intPtr(999)},
		{Service: "llm", Model: "m2", InputPerMillion: intPtr(500), CachedInputPerMillion: intPtr(0)},
		{Service: "img", Model: "m1", InputPerMillion: intPtr(1)},
	}

	// m1: only the output tier is overridden; input/cached inherit the flat caps.
	got := Effective(flat, rows, "llm", "m1")
	if got.InputPerMillion != flat.InputPerMillion || got.CachedInputPerMillion != flat.CachedInputPerMillion {
		t.Fatalf("m1 inherited tiers must pass through: %+v", got)
	}
	if got.OutputPerMillion == nil || *got.OutputPerMillion != 999 {
		t.Fatalf("m1 output tier must be overridden, got %+v", got)
	}

	// m2: input overridden, cached zeroed (free-tier peers only for cached),
	// output inherits the flat cap.
	got = Effective(flat, rows, "llm", "m2")
	if got.InputPerMillion == nil || *got.InputPerMillion != 500 {
		t.Fatalf("m2 input must be 500, got %+v", got)
	}
	if got.CachedInputPerMillion == nil || *got.CachedInputPerMillion != 0 {
		t.Fatalf("m2 cached must be explicit zero, got %+v", got)
	}
	if got.OutputPerMillion != flat.OutputPerMillion {
		t.Fatalf("m2 output must inherit flat cap, got %+v", got)
	}

	// A different service's row must not leak into an llm quote.
	got = Effective(flat, rows, "llm", "other")
	if got.InputPerMillion != flat.InputPerMillion || got.OutputPerMillion != flat.OutputPerMillion {
		t.Fatalf("unmatched model must use flat caps, got %+v", got)
	}

	// No rows at all: flat caps unchanged.
	got = Effective(flat, nil, "llm", "m1")
	if got.InputPerMillion != flat.InputPerMillion || got.OutputPerMillion != flat.OutputPerMillion {
		t.Fatalf("no rows must yield flat caps, got %+v", got)
	}
}

func TestEffectiveAllowsExpensiveModelWhenRowSets(t *testing.T) {
	// The point of per-model caps: a flat 100 cap on m1 plus a 5000 row on
	// the big model must let m1's quotes through and reject m2's — the
	// opposite of a single flat number.
	flat := Caps{InputPerMillion: intPtr(100)}
	rows := []ModelCaps{{Service: "llm", Model: "m2", InputPerMillion: intPtr(5000)}}

	if !Affordable(EligiblePeerQuote{InputPerMillion: 90}, Effective(flat, rows, "llm", "m1")) {
		t.Fatal("m1 quote within flat cap must be affordable")
	}
	if Affordable(EligiblePeerQuote{InputPerMillion: 4000}, Effective(flat, rows, "llm", "m1")) {
		t.Fatal("m1 quote of 4000 must be rejected: flat cap 100 with no model row")
	}
	if !Affordable(EligiblePeerQuote{InputPerMillion: 4000}, Effective(flat, rows, "llm", "m2")) {
		t.Fatal("m2 quote of 4000 must be affordable under the 5000 model row")
	}
}
