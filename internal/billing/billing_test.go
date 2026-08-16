package billing

import (
	"errors"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestCostRoundingAndOverflow(t *testing.T) {
	// One base unit is the floor of any positive product.
	c, err := Cost(1, 0, 0, 1, 1, 1)
	if err != nil || c != 1 {
		t.Fatalf("Cost(1,0,0,1,1,1) = %d,%v; want 1,nil", c, err)
	}
	// Ceiling rounds up: (1_000_001 base) / 1M -> 2.
	c, err = Cost(1_000_001, 0, 0, 1, 0, 0)
	if err != nil || c != 2 {
		t.Fatalf("ceil: got %d,%v; want 2,nil", c, err)
	}
	// Exact division.
	c, err = Cost(1_000_000, 0, 0, 3, 0, 0)
	if err != nil || c != 3 {
		t.Fatalf("exact: got %d,%v; want 3,nil", c, err)
	}
	// Three tiers combine and round the SUM up, not each term.
	// regular=2000000 @1500 = 3000 base; cached=500000 @400 = 200 base;
	// output=300000 @3600 = 1080 base; sum=4280 -> already whole.
	c, err = Cost(2_000_000, 500_000, 300_000, 1500, 400, 3600)
	if err != nil || c != 4280 {
		t.Fatalf("three-tier: got %d,%v; want 4280,nil", c, err)
	}
	// A sum with a remainder rounds up across the combined total.
	// regular=1 @1500 -> 1500 base; output=1 @3600 -> 3600 base; sum=5100;
	// 5100/1e6 = 0.0051 -> ceil 1.
	c, err = Cost(1, 0, 1, 1500, 0, 3600)
	if err != nil || c != 1 {
		t.Fatalf("combined-ceil: got %d,%v; want 1,nil", c, err)
	}
}

func TestCostZero(t *testing.T) {
	// Zero tokens and unpriced peers both yield a zero charge (release path).
	for _, tc := range []struct {
		name                 string
		regular, cached, out int
		in, cin, outr        int64
	}{
		{"zero tokens", 0, 0, 0, 1500, 300, 3600},
		{"unpriced peer", 100, 0, 10, 0, 0, 0},
		{"priced but zero tokens", 0, 0, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Cost(tc.regular, tc.cached, tc.out, tc.in, tc.cin, tc.outr)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if c != 0 {
				t.Fatalf("got %d; want 0 (release)", c)
			}
		})
	}
}

func TestCostNegativeRejected(t *testing.T) {
	if _, err := Cost(-1, 0, 0, 1, 0, 0); err == nil {
		t.Fatal("negative tokens accepted")
	}
	if _, err := Cost(1, 0, 0, -1, 0, 0); err == nil {
		t.Fatal("negative rate accepted")
	}
}

func TestCostOverflow(t *testing.T) {
	// rate*token overflows int64: a rate near MaxBaseRate times a huge token
	// count must surface ErrOverflow rather than wrap silently.
	huge := int64(1) << 62
	if _, err := Cost(int(huge), 0, 0, MaxBaseRate, 0, 0); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected ErrOverflow, got %v", err)
	}
	// A bound-respecting rate and token count is fine: MaxBaseRate * 1e6 tokens
	// = 1e18 < max int64, divided by 1e6 = 1e12 base units.
	c, err := Cost(1_000_000, 0, 0, MaxBaseRate, 0, 0)
	if err != nil || c != MaxBaseRate {
		t.Fatalf("bound-respecting: got %d,%v; want %d,nil", c, err, MaxBaseRate)
	}
}

func TestFee(t *testing.T) {
	// Zero bps is pure P2P.
	fee, seller, err := Fee(1000, 0)
	if err != nil || fee != 0 || seller != 1000 {
		t.Fatalf("zero bps: fee=%d seller=%d err=%v", fee, seller, err)
	}
	// 100 bps = 1%: floor(1000 * 100 / 10000) = 10, seller = 990.
	fee, seller, err = Fee(1000, 100)
	if err != nil || fee != 10 || seller != 990 {
		t.Fatalf("100bps: fee=%d seller=%d err=%v", fee, seller, err)
	}
	// Rounding is floor: 9999 base units @ 1 bps -> floor(9999/10000) = 0.
	fee, seller, err = Fee(9999, 1)
	if err != nil || fee != 0 || seller != 9999 {
		t.Fatalf("floor fee: fee=%d seller=%d err=%v", fee, seller, err)
	}
	// 10000 bps = 100%: seller gets nothing.
	fee, seller, err = Fee(1000, 10000)
	if err != nil || fee != 1000 || seller != 0 {
		t.Fatalf("100pct: fee=%d seller=%d err=%v", fee, seller, err)
	}
	// bps out of range are clamped.
	fee, seller, err = Fee(1000, 99999)
	if err != nil || fee != 1000 || seller != 0 {
		t.Fatalf("clamp high: fee=%d seller=%d err=%v", fee, seller, err)
	}
}

func TestAffordable(t *testing.T) {
	q := EligiblePeerQuote{InputPerMillion: 1200, CachedInputPerMillion: 300, OutputPerMillion: 3600}
	// No caps -> any quote affordable.
	if !Affordable(q, Caps{}) {
		t.Fatal("empty caps rejected affordable quote")
	}
	// All three within caps.
	if !Affordable(q, Caps{InputPerMillion: ptr(int64(1200)), CachedInputPerMillion: ptr(int64(300)), OutputPerMillion: ptr(int64(3600))}) {
		t.Fatal("exact caps rejected")
	}
	// One dimension over cap rejects the whole peer.
	if Affordable(q, Caps{InputPerMillion: ptr(int64(1199))}) {
		t.Fatal("input over cap accepted")
	}
	if Affordable(q, Caps{CachedInputPerMillion: ptr(int64(299))}) {
		t.Fatal("cached over cap accepted")
	}
	if Affordable(q, Caps{OutputPerMillion: ptr(int64(3599))}) {
		t.Fatal("output over cap accepted")
	}
	// Unpriced peer is affordable under any (including zero) caps.
	unpriced := EligiblePeerQuote{}
	if !Affordable(unpriced, Caps{InputPerMillion: ptr(int64(0)), CachedInputPerMillion: ptr(int64(0)), OutputPerMillion: ptr(int64(0))}) {
		t.Fatal("unpriced peer rejected under zero caps")
	}
}

func TestReserveAmountWorstCase(t *testing.T) {
	quotes := []EligiblePeerQuote{
		{InputPerMillion: 1200, CachedInputPerMillion: 300, OutputPerMillion: 3600}, // in=dearer
		{InputPerMillion: 800, CachedInputPerMillion: 2000, OutputPerMillion: 4000}, // cached=dearer
	}
	// inputCeil=1e6, outputCeil=1e6.
	// quote 0: max(1200,300)*1e6 + 3600*1e6 = 1200 + 3600 = 4800 (per 1M; already in base/1M units since 1e6 tokens * rate/1e6 = rate).
	// quote 1: max(800,2000)*1e6 + 4000*1e6 = 2000 + 4000 = 6000.
	// reserve = max(4800, 6000) = 6000.
	got, err := ReserveAmount(quotes, 1_000_000, 1_000_000)
	if err != nil || got != 6000 {
		t.Fatalf("ReserveAmount = %d,%v; want 6000,nil", got, err)
	}
	// Smaller ceilings reduce the reserve proportionally.
	got, err = ReserveAmount(quotes, 500_000, 1_000_000)
	// quote 1: max(800,2000)*500000 + 4000*1e6 = 2000*0.5 + 4000 = 1000+4000=5000
	if err != nil || got != 5000 {
		t.Fatalf("ReserveAmount(half in) = %d,%v; want 5000,nil", got, err)
	}
}

func TestReserveAmountUnpriced(t *testing.T) {
	// All-unpriced quotes reserve zero (the request is recorded but free).
	got, err := ReserveAmount([]EligiblePeerQuote{{}, {}}, 1_000_000, 1_000_000)
	if err != nil || got != 0 {
		t.Fatalf("unpriced reserve = %d,%v; want 0,nil", got, err)
	}
	// A mix reserves the priced peer's worst case.
	got, err = ReserveAmount([]EligiblePeerQuote{{}, {InputPerMillion: 1500, OutputPerMillion: 3000}}, 1_000_000, 0)
	if err != nil || got != 1500 {
		t.Fatalf("mixed reserve = %d,%v; want 1500,nil", got, err)
	}
}

func TestMergeCaps(t *testing.T) {
	account := Caps{InputPerMillion: ptr(int64(1200)), CachedInputPerMillion: ptr(int64(300)), OutputPerMillion: ptr(int64(3600))}
	// Per-request caps can only tighten the account default.
	got := MergeCaps(account, Caps{OutputPerMillion: ptr(int64(5000))})
	if *got.InputPerMillion != 1200 || *got.CachedInputPerMillion != 300 || *got.OutputPerMillion != 3600 {
		t.Fatalf("looser request cap should not widen account caps: %+v", got)
	}
	got = MergeCaps(account, Caps{OutputPerMillion: ptr(int64(1800))})
	if *got.OutputPerMillion != 1800 {
		t.Fatalf("tighter output cap lost: %+v", got)
	}
	// A nil request dimension that the account also left nil means unlimited.
	accountNoIn := Caps{CachedInputPerMillion: ptr(int64(300)), OutputPerMillion: ptr(int64(3600))}
	got = MergeCaps(accountNoIn, Caps{})
	if got.InputPerMillion != nil || *got.CachedInputPerMillion != 300 || *got.OutputPerMillion != 3600 {
		t.Fatalf("nil override: %+v", got)
	}
	// Explicit nil in request does NOT clear an account default.
	got = MergeCaps(account, Caps{InputPerMillion: nil})
	if got.InputPerMillion == nil || *got.InputPerMillion != 1200 {
		t.Fatalf("nil request should keep account default: %+v", got)
	}
	// A request cap still applies when the account left the dimension unlimited.
	got = MergeCaps(Caps{}, Caps{CachedInputPerMillion: ptr(int64(250))})
	if got.CachedInputPerMillion == nil || *got.CachedInputPerMillion != 250 {
		t.Fatalf("request cap should apply to unlimited account caps: %+v", got)
	}
}
