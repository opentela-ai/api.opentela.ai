package store

import (
	"context"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

// TestModelCapsRoundTrip: ReplaceModelCaps swaps the whole sheet; tiers are
// stored verbatim (NULL stays NULL, 0 stays 0).
func TestModelCapsRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	if err := p.ReplaceModelCaps(ctx, "acct-caps", []billing.ModelCaps{
		{Service: "llm", Model: "m1", OutputPerMillion: intPtr(999)},
		{Service: "llm", Model: "m2", InputPerMillion: intPtr(500), CachedInputPerMillion: intPtr(0), OutputPerMillion: intPtr(0)},
		{Service: "img", Model: "sd", CachedInputPerMillion: nil},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	rows, err := p.ModelCapsForAccount(ctx, "acct-caps")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	byKey := map[string]billing.ModelCaps{}
	for _, r := range rows {
		byKey[r.Service+"/"+r.Model] = r
	}
	m1 := byKey["llm/m1"]
	if m1.InputPerMillion != nil || m1.CachedInputPerMillion != nil || m1.OutputPerMillion == nil || *m1.OutputPerMillion != 999 {
		t.Fatalf("llm/m1 = %+v", m1)
	}
	m2 := byKey["llm/m2"]
	if m2.InputPerMillion == nil || *m2.InputPerMillion != 500 ||
		m2.CachedInputPerMillion == nil || *m2.CachedInputPerMillion != 0 {
		t.Fatalf("llm/m2 must preserve explicit zeros and values: %+v", m2)
	}

	// Second replace is a full swap, not a merge.
	if err := p.ReplaceModelCaps(ctx, "acct-caps", []billing.ModelCaps{
		{Service: "llm", Model: "m3", InputPerMillion: intPtr(7)},
	}); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	rows, err = p.ModelCapsForAccount(ctx, "acct-caps")
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "m3" || *rows[0].InputPerMillion != 7 {
		t.Fatalf("second replace must swap the sheet: %+v", rows)
	}

	// Empty sheet clears; read returns empty, not an error.
	if err := p.ReplaceModelCaps(ctx, "acct-caps", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	rows, err = p.ModelCapsForAccount(ctx, "acct-caps")
	if err != nil || len(rows) != 0 {
		t.Fatalf("cleared sheet: rows=%d err=%v", len(rows), err)
	}
}

func intPtr(v int64) *int64 { return &v }

// TestAskConfigLifecycle: ReplaceAskConfig is per-peer full replacement and
// AllAskConfigs/AskConfigForPeers see exactly what was saved.
func TestAskConfigLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)

	asks := []billing.Ask{
		{Service: "llm", Model: "m1", InputPerMillion: 100, CachedInputPerMillion: 30, OutputPerMillion: 300},
		{Service: "llm", Model: "m2", InputPerMillion: 500, CachedInputPerMillion: 100, OutputPerMillion: 900},
	}
	if err := p.ReplaceAskConfig(ctx, "peer-cfg-1", asks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	cfg, err := p.AskConfigForPeers(ctx, []string{"peer-cfg-1", "peer-missing"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(cfg["peer-cfg-1"]) != 2 || cfg["peer-cfg-1"][0].Model != "m1" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, ok := cfg["peer-missing"]; ok {
		t.Fatal("missing peer must be absent")
	}

	all, err := p.AllAskConfigs(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all["peer-cfg-1"]) != 2 {
		t.Fatalf("all = %+v", all)
	}

	// Republishing into the market table works from the same shapes the
	// refresher uses: ReplaceAsks must accept the config rows untouched
	// (Ask embeds PeerID; config rows carry it empty, which is how the
	// refresher passes them). It returns the new revision; liveness is
	// verified by reading the market table back.
	rev, err := p.ReplaceAsks(ctx, "peer-cfg-1", cfg["peer-cfg-1"], 5*time.Minute)
	if err != nil || rev == 0 {
		t.Fatalf("ReplaceAsks from config: rev=%d err=%v", rev, err)
	}
	for _, m := range []string{"m1", "m2"} {
		live, err := p.LiveAsks(ctx, "llm", m, time.Now())
		if err != nil {
			t.Fatalf("live %s: %v", m, err)
		}
		if len(live) != 1 || live[0].PeerID != "peer-cfg-1" {
			t.Fatalf("live %s = %+v", m, live)
		}
	}

	// Clearing one peer leaves others untouched.
	if err := p.ReplaceAskConfig(ctx, "peer-cfg-2", []billing.Ask{{Service: "llm", Model: "x", InputPerMillion: 1, CachedInputPerMillion: 1, OutputPerMillion: 1}}); err != nil {
		t.Fatalf("second peer: %v", err)
	}
	if err := p.ReplaceAskConfig(ctx, "peer-cfg-1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	all, err = p.AllAskConfigs(ctx)
	if err != nil {
		t.Fatalf("all 2: %v", err)
	}
	if _, ok := all["peer-cfg-1"]; ok {
		t.Fatal("cleared peer must be absent")
	}
	if len(all["peer-cfg-2"]) != 1 {
		t.Fatalf("other peer must survive: %+v", all)
	}
}
