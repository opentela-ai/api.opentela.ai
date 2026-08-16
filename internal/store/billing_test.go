package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

// seedCredit credits accountID by amount as a ledger-backed deposit so the
// balance reconciles against SUM(delta_raw). A unique ref keeps deposits
// distinct under credit_ledger's (ref, leg) uniqueness.
func seedCredit(t *testing.T, p *Postgres, accountID string, amount int64) {
	t.Helper()
	ctx := context.Background()
	if err := p.EnsureAccountCredit(ctx, accountID); err != nil {
		t.Fatalf("ensure %s: %v", accountID, err)
	}
	ref := "seed:" + randHex(8)
	if _, err := p.pool.Exec(ctx, `
		UPDATE account_credits SET credit_raw = credit_raw + $2, updated_at = now()
		WHERE account_id = $1`, accountID, amount); err != nil {
		t.Fatalf("seed credit %s: %v", accountID, err)
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
		VALUES ($1, $2, 'deposit', 'deposit', $3, $4, now())`,
		accountID, amount, "seed-wallet", ref); err != nil {
		t.Fatalf("seed ledger %s: %v", accountID, err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// quoteFor builds an eligible-peer quote snapshot entry priced at the given
// rates, attributed to seller.
func quoteFor(peer, seller string, in, cin, out, rev int64) billing.EligiblePeerQuote {
	return billing.EligiblePeerQuote{
		PeerID: peer, SellerAccountID: seller, OwnerWallet: seller + "-wallet",
		Revision: rev, InputPerMillion: in, CachedInputPerMillion: cin, OutputPerMillion: out,
	}
}

func TestReplaceAsksLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()

	if rev, err := p.ReplaceAsks(ctx, "p1", []billing.Ask{
		{Service: "llm", Model: "llama", InputPerMillion: 1200, CachedInputPerMillion: 300, OutputPerMillion: 3600},
	}, 5*time.Minute); err != nil {
		t.Fatalf("replace: %v", err)
	} else if rev == 0 {
		t.Fatalf("replace returned revision 0")
	}
	asks, err := p.LiveAsks(ctx, "llm", "llama", now)
	if err != nil || len(asks) != 1 || asks[0].InputPerMillion != 1200 {
		t.Fatalf("live asks: %+v %v", asks, err)
	}

	// Full replacement: the old (service, model) is dropped, a fresh revision
	// is assigned across all rows.
	revBefore := asks[0].Revision
	if rev, err := p.ReplaceAsks(ctx, "p1", []billing.Ask{
		{Service: "llm", Model: "llama", InputPerMillion: 2000, CachedInputPerMillion: 500, OutputPerMillion: 6000},
		{Service: "llm", Model: "qwen", InputPerMillion: 800, CachedInputPerMillion: 200, OutputPerMillion: 2400},
	}, 5*time.Minute); err != nil {
		t.Fatalf("replace2: %v", err)
	} else if rev <= revBefore {
		t.Fatalf("revision %d not > %d", rev, revBefore)
	}
	asks, _ = p.LiveAsks(ctx, "llm", "llama", now)
	if len(asks) != 1 || asks[0].InputPerMillion != 2000 || asks[0].Revision <= revBefore {
		t.Fatalf("replacement not applied / revision not bumped: %+v", asks)
	}
	// Empty set clears the peer.
	if rev, err := p.ReplaceAsks(ctx, "p1", nil, 5*time.Minute); err != nil {
		t.Fatalf("clear: %v", err)
	} else if rev != 0 {
		t.Fatalf("clear returned revision %d, want 0", rev)
	}
	if asks, _ := p.LiveAsks(ctx, "llm", "llama", now); len(asks) != 0 {
		t.Fatalf("clear left %d asks", len(asks))
	}
}

func TestReplaceAsksValidation(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	for _, tc := range []struct {
		name string
		asks []billing.Ask
	}{
		{"too many", make([]billing.Ask, 257)},
		{"duplicate", []billing.Ask{{Service: "llm", Model: "x"}, {Service: "llm", Model: "x"}}},
		{"negative", []billing.Ask{{Service: "llm", Model: "x", InputPerMillion: -1}}},
		{"over bound", []billing.Ask{{Service: "llm", Model: "x", InputPerMillion: billing.MaxBaseRate}}},
		{"empty model", []billing.Ask{{Service: "llm"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.ReplaceAsks(ctx, "pv", tc.asks, 5*time.Minute); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("got %v; want %v", err, billing.ErrConflict)
			}
		})
	}
}

func TestReserveBillingInsufficientAndConservative(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	seedCredit(t, p, "buyer", 10) // only 10 base units available

	// Reserve against two peers; the expensive one sets the conservative max.
	// inputCeil=1e6, outputCeil=1e6 -> reserve = max(1200+3600, 8000+4000)=12000.
	quotes := []billing.EligiblePeerQuote{
		quoteFor("cheap", "sellerA", 1200, 300, 3600, 1),
		quoteFor("dear", "sellerB", 8000, 1000, 4000, 1),
	}
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "buyer", Service: "llm", Model: "llama",
		Quotes: quotes, InputCeil: 1_000_000, OutputCeil: 1_000_000,
	}); !errors.Is(err, billing.ErrInsufficientCredit) {
		t.Fatalf("reserve should be insufficient (need 12000, have 10): %v", err)
	}

	// With enough credit, the reserve is the conservative max.
	seedCredit(t, p, "buyer2", 100_000)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r2", BuyerAccountID: "buyer2", Service: "llm", Model: "llama",
		Quotes: quotes, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	r, err := p.BillingRequest(ctx, "r2")
	if err != nil || r.ReservedRaw != 12000 {
		t.Fatalf("reserved_raw = %d (err %v); want 12000", r.ReservedRaw, err)
	}
	c, _ := p.AccountCredit(ctx, "buyer2")
	if c.ReservedRaw != 12000 || c.Available() != 100_000-12000 {
		t.Fatalf("projection after reserve: %+v", c)
	}
}

func TestConcurrentReservationsNeverOverdraft(t *testing.T) {
	// Many goroutines reserve against one account with limited credit. Each
	// reservation is exactly 1 base unit. Exactly `credit` may succeed; the
	// rest get ErrInsufficientCredit, and reserved_raw never exceeds credit_raw.
	ctx := context.Background()
	p := newTestStore(t)
	const credit = 50
	const goroutines = 500
	seedCredit(t, p, "acct", credit)

	// A quote priced at 1 base unit per 1M tokens over a 1M-token request.
	q := quoteFor("p", "seller", 1, 0, 0, 1)

	var wg sync.WaitGroup
	var ok, denied int64
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := p.ReserveBilling(ctx, billing.Reservation{
				RequestID:      rid(i),
				BuyerAccountID: "acct",
				Service:        "llm",
				Model:          "llama",
				Quotes:         []billing.EligiblePeerQuote{q},
				InputCeil:      1_000_000,
				OutputCeil:     0,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, billing.ErrInsufficientCredit):
				denied++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if ok != credit {
		t.Fatalf("successful reservations = %d; want %d", ok, credit)
	}
	if int(ok+denied) != goroutines {
		t.Fatalf("ok+denied = %d; want %d", ok+denied, goroutines)
	}
	c, err := p.AccountCredit(ctx, "acct")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if c.ReservedRaw != credit {
		t.Fatalf("reserved_raw = %d; want %d", c.ReservedRaw, credit)
	}
	if c.ReservedRaw > c.CreditRaw {
		t.Fatalf("invariant violated: reserved %d > credit %d", c.ReservedRaw, c.CreditRaw)
	}
	rec, err := p.ReconcileAccount(ctx, "acct", time.Now().UTC())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.DriftReserved != 0 {
		t.Fatalf("reserved drift = %d; want 0", rec.DriftReserved)
	}
	if !rec.InvariantHeld {
		t.Fatalf("invariant not held: %+v", rec)
	}
}

func rid(i int) string {
	return "req-" + hex.EncodeToString([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
}

func TestSettleBillingChargesSnapshotAndIdempotent(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)

	// Peer publishes an ask; the gate snapshots it.
	if _, err := p.ReplaceAsks(ctx, "p1", []billing.Ask{
		{Service: "llm", Model: "llama", InputPerMillion: 1200, CachedInputPerMillion: 300, OutputPerMillion: 3600},
	}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	asks, _ := p.LiveAsks(ctx, "llm", "llama", now)
	rev := asks[0].Revision

	// Snapshot the peer into the reservation (the gate does this live).
	q := quoteFor("p1", "seller", 1200, 300, 3600, rev)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "buyer", Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// The ask changes between gate and settlement; the charge must use the
	// snapshot, never the fresh rate.
	if _, err := p.ReplaceAsks(ctx, "p1", []billing.Ask{
		{Service: "llm", Model: "llama", InputPerMillion: 9999, CachedInputPerMillion: 9999, OutputPerMillion: 9999},
	}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	r, err := p.SettleBilling(ctx, "r1", "p1", billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, 0, now)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// Snapshot rate: in 1200 + out 3600 = 4800 (no fee).
	if r.CostRaw != 4800 || r.FeeRaw != 0 || r.SellerRaw != 4800 {
		t.Fatalf("charge = cost %d fee %d seller %d; want 4800,0,4800", r.CostRaw, r.FeeRaw, r.SellerRaw)
	}
	if r.ServedInputPerMillion != 1200 {
		t.Fatalf("served rate = %d; want snapshot 1200", r.ServedInputPerMillion)
	}
	buyer, _ := p.AccountCredit(ctx, "buyer")
	seller, _ := p.AccountCredit(ctx, "seller")
	if buyer.CreditRaw != 100_000-4800 {
		t.Fatalf("buyer credit = %d; want %d", buyer.CreditRaw, 100_000-4800)
	}
	if buyer.ReservedRaw != 0 {
		t.Fatalf("buyer reserved = %d; want 0", buyer.ReservedRaw)
	}
	if seller.CreditRaw != 4800 {
		t.Fatalf("seller credit = %d; want 4800", seller.CreditRaw)
	}

	// Repeated settlement is a no-op: balances and ledger unchanged.
	r2, err := p.SettleBilling(ctx, "r1", "p1", billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, 0, now)
	if err != nil || r2.State != billing.StateSettled {
		t.Fatalf("idempotent settle: %+v %v", r2, err)
	}
	seller2, _ := p.AccountCredit(ctx, "seller")
	if seller2.CreditRaw != 4800 {
		t.Fatalf("seller changed on replay: %d", seller2.CreditRaw)
	}
}

func TestSettleBillingUnknownPeerReleases(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	seedCredit(t, p, "buyer", 100_000)
	q := quoteFor("p1", "seller", 1200, 300, 3600, 1)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "buyer", Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// X-Computing-Node names a peer NOT in the snapshot: release, never charge.
	r, err := p.SettleBilling(ctx, "r1", "stranger", billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, 0, now)
	if err != nil || r.State != billing.StateReleased {
		t.Fatalf("settle unknown: %+v %v", r, err)
	}
	buyer, _ := p.AccountCredit(ctx, "buyer")
	if buyer.CreditRaw != 100_000 || buyer.ReservedRaw != 0 {
		t.Fatalf("buyer after unknown-peer release: %+v", buyer)
	}
}

func TestSettleBillingZeroCostReleases(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	seedCredit(t, p, "buyer", 100_000)
	// Unpriced peer (all rates zero) is eligible but free.
	q := quoteFor("p1", "seller", 0, 0, 0, 1)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "buyer", Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := p.SettleBilling(ctx, "r1", "p1", billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, 0, now)
	if err != nil || r.State != billing.StateReleased || r.CostRaw != 0 {
		t.Fatalf("zero-cost: %+v %v", r, err)
	}
	buyer, _ := p.AccountCredit(ctx, "buyer")
	if buyer.CreditRaw != 100_000 || buyer.ReservedRaw != 0 {
		t.Fatalf("buyer after zero-cost: %+v", buyer)
	}
}

func TestSettleBillingBuyerEqualsSellerBalanced(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	const start = 1_000_000
	seedCredit(t, p, "alice", start)
	seedCredit(t, p, billing.TreasuryAccountID, 0)

	q := quoteFor("p1", "alice", 1200, 300, 3600, 1)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "alice", Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := p.SettleBilling(ctx, "r1", "p1", billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, 100, now) // 1% fee
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// cost 4800, fee floor(4800*100/10000)=48, seller 4752.
	// alice nets -4800 (usage) + 4752 (earn) = -48; treasury +48.
	if r.CostRaw != 4800 || r.FeeRaw != 48 || r.SellerRaw != 4752 {
		t.Fatalf("charge cost=%d fee=%d seller=%d; want 4800,48,4752", r.CostRaw, r.FeeRaw, r.SellerRaw)
	}
	alice, _ := p.AccountCredit(ctx, "alice")
	treasury, _ := p.AccountCredit(ctx, billing.TreasuryAccountID)
	if alice.CreditRaw != start-48 {
		t.Fatalf("alice credit = %d; want %d", alice.CreditRaw, start-48)
	}
	if treasury.CreditRaw != 48 {
		t.Fatalf("treasury credit = %d; want 48", treasury.CreditRaw)
	}
	// Conservation: total ledger delta across alice + treasury == 0.
	recA, _ := p.ReconcileAccount(ctx, "alice", now)
	recT, _ := p.ReconcileAccount(ctx, billing.TreasuryAccountID, now)
	if recA.DriftCredit != 0 || recT.DriftCredit != 0 {
		t.Fatalf("drift alice=%d treasury=%d; want 0,0", recA.DriftCredit, recT.DriftCredit)
	}
}

func TestReleaseBillingIdempotent(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()
	seedCredit(t, p, "buyer", 100_000)
	q := quoteFor("p1", "seller", 1200, 300, 3600, 1)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "r1", BuyerAccountID: "buyer", Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReleaseBilling(ctx, "r1", "client_abort", now); err != nil {
		t.Fatalf("release: %v", err)
	}
	buyer, _ := p.AccountCredit(ctx, "buyer")
	if buyer.CreditRaw != 100_000 || buyer.ReservedRaw != 0 {
		t.Fatalf("buyer after release: %+v", buyer)
	}
	// Second release is a no-op.
	if _, err := p.ReleaseBilling(ctx, "r1", "client_abort", now); err != nil {
		t.Fatalf("second release: %v", err)
	}
	buyer2, _ := p.AccountCredit(ctx, "buyer")
	if buyer2 != buyer {
		t.Fatalf("second release changed balance: %+v", buyer2)
	}
}

func TestReconcileAccountDrift(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	// Credit inserted WITHOUT a ledger row -> reconciliation must report drift.
	if _, err := p.pool.Exec(ctx,
		`INSERT INTO account_credits (account_id, credit_raw) VALUES ('drifter', 9999)`); err != nil {
		t.Fatal(err)
	}
	rec, err := p.ReconcileAccount(ctx, "drifter", time.Now().UTC())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.DriftCredit != 9999 {
		t.Fatalf("drift = %d; want 9999", rec.DriftCredit)
	}
	if !rec.InvariantHeld {
		t.Fatalf("invariant should hold for isolated account: %+v", rec)
	}
}

func TestListLedgerCursor(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	for i := 0; i < 5; i++ {
		seedCredit(t, p, "acct", 1)
	}
	page1, cur, err := p.ListLedger(ctx, "acct", nil, 3)
	if err != nil || len(page1) != 3 || cur == nil {
		t.Fatalf("page1: %d items, cur=%v, err=%v", len(page1), cur, err)
	}
	page2, cur, err := p.ListLedger(ctx, "acct", cur, 3)
	if err != nil || len(page2) != 2 || cur != nil {
		t.Fatalf("page2: %d items, cur=%v, err=%v", len(page2), cur, err)
	}
	// Newest-first ordering within page1.
	if !page1[0].CreatedAt.After(page1[2].CreatedAt) && !page1[0].CreatedAt.Equal(page1[2].CreatedAt) {
		t.Fatalf("page1 not newest-first: %+v", page1)
	}
}

func TestSweepStaleReservationsReleasesAndPreservesSettled(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	now := time.Now().UTC()

	// Two peers; settle one against the cheap peer, leave one reserved and
	// stale. A third settled request must be untouched by the sweep.
	quotes := []billing.EligiblePeerQuote{quoteFor("cheap", "sellerA", 1000, 0, 2000, 1)}
	seedCredit(t, p, "buyer", 1_000_000)

	for _, id := range []string{"r-settle", "r-stale"} {
		if _, err := p.ReserveBilling(ctx, billing.Reservation{
			RequestID: id, BuyerAccountID: "buyer", Service: "llm", Model: "m",
			Quotes: quotes, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
		}); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}

	// Settle r-settle normally.
	if _, err := p.SettleBilling(ctx, "r-settle", "cheap",
		billing.Usage{InputTokens: 1000, OutputTokens: 500}, 0, now); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Backdate the stale reservation so it's older than the sweep threshold.
	if _, err := p.pool.Exec(ctx,
		`UPDATE billing_requests SET reserved_at = $2 WHERE request_id = $1`,
		"r-stale", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	cBefore, _ := p.AccountCredit(ctx, "buyer")
	if cBefore.ReservedRaw == 0 {
		t.Fatalf("expected a reserved balance before sweep, got %+v", cBefore)
	}

	released, err := p.SweepStaleReservations(ctx, now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(released) != 1 || released[0] != "r-stale" {
		t.Fatalf("released = %v, want [r-stale]", released)
	}

	r, err := p.BillingRequest(ctx, "r-stale")
	if err != nil || r.State != billing.StateReleased || r.ReleaseReason != "swept_stale" {
		t.Fatalf("stale after sweep: %+v err=%v", r, err)
	}
	// The settled request is untouched.
	rs, _ := p.BillingRequest(ctx, "r-settle")
	if rs.State != billing.StateSettled {
		t.Fatalf("settled request was swept: %+v", rs)
	}
	// reserved_raw dropped back to zero.
	cAfter, _ := p.AccountCredit(ctx, "buyer")
	if cAfter.ReservedRaw != 0 {
		t.Fatalf("reserved_raw after sweep = %d, want 0", cAfter.ReservedRaw)
	}
}

func TestSweepStaleReservationsEmptyIsSafe(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	released, err := p.SweepStaleReservations(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("sweep empty: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want []", released)
	}
}
