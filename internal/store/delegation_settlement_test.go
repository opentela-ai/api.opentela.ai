package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

func seedPubkey(seed string) string {
	h := sha256.Sum256([]byte(seed))
	key := ed25519.NewKeyFromSeed(h[:])
	return solana.EncodeBase58(key.Public().(ed25519.PublicKey))
}

// settleDelegated is the shared setup: a settled usage charge with the given
// fee bps, returns (requestID, costRaw, sellerRaw, feeRaw).
func settleDelegated(t *testing.T, p *Postgres, reqID, buyer, peer, seller string, feeBps int) (int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := p.ReplaceAsks(ctx, peer, []billing.Ask{
		{Service: "llm", Model: "llama", InputPerMillion: 1200, CachedInputPerMillion: 300, OutputPerMillion: 3600},
	}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	asks, _ := p.LiveAsks(ctx, "llm", "llama", now)
	q := quoteFor(peer, seller, 1200, 300, 3600, asks[0].Revision)
	if _, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: reqID, BuyerAccountID: buyer, Service: "llm", Model: "llama",
		Quotes: []billing.EligiblePeerQuote{q}, InputCeil: 1_000_000, OutputCeil: 1_000_000, ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := p.SettleBilling(ctx, reqID, peer, billing.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 0, OutputTokens: 1_000_000,
	}, feeBps, now)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	return r.CostRaw, r.SellerRaw, r.FeeRaw
}

func TestClaimDelegationSettlementsEndToEnd(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("claim-delegate")
	buyerWallet := seedPubkey("claim-buyer-wallet")
	sellerWallet := seedPubkey("claim-seller-wallet")

	// Accounts with linked wallets: buyer and seller.
	linkWallet(t, p, "buyer", buyerWallet)
	linkWallet(t, p, "seller", sellerWallet)
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)
	grantAllowance(t, p, "buyer", delegate, 100_000, "obs-1")

	// feeBps 500 → cost 4800, seller 4560, fee 240.
	cost, sellerRaw, feeRaw := settleDelegated(t, p, "cr1", "buyer", "p1", "seller", 500)
	if cost != 4800 || feeRaw != 240 {
		t.Fatalf("charge = cost %d fee %d", cost, feeRaw)
	}

	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("claim-mint"),
		TokenProgram: seedPubkey("claim-tp"), TreasuryWallet: seedPubkey("claim-treasury"),
		LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("batches = %d, want 2 (seller + fee): %+v", len(created), created)
	}
	var sellerBatch, feeBatch *billing.DelegationSettlement
	for i := range created {
		if created[i].DestinationWallet == sellerWallet {
			sellerBatch = &created[i]
		} else {
			feeBatch = &created[i]
		}
	}
	if sellerBatch == nil || feeBatch == nil {
		t.Fatalf("destinations wrong: %+v", created)
	}
	if sellerBatch.AmountRaw != sellerRaw || feeBatch.AmountRaw != feeRaw {
		t.Fatalf("amounts: seller %d fee %d; want %d/%d", sellerBatch.AmountRaw, feeBatch.AmountRaw, sellerRaw, feeRaw)
	}
	if sellerBatch.SourceATA == "" || feeBatch.SourceATA == "" {
		t.Fatal("source ATA not stored")
	}

	// Allowance consumed by the full outflow: 4800.
	a := allowanceOf(t, p, "buyer", delegate)
	if a.AllowanceRaw != 100_000-cost {
		t.Fatalf("allowance after claim = %d, want %d", a.AllowanceRaw, 100_000-cost)
	}
	// Credit untouched by consumption: deposit seed + grant mirror − cost.
	// (The grant itself credits — §11.2 — so the pre-claim credit already
	// included it; the claim must not move it again.)
	if got := creditOf(t, p, "buyer").CreditRaw; got != 100_000+100_000-cost {
		t.Fatalf("credit changed on claim: %d", got)
	}

	// Re-claim: nothing new (cursor past the legs).
	again, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil || len(again) != 0 {
		t.Fatalf("re-claim = %+v, %v; want empty", again, err)
	}

	// The state machine: signed → broadcast → finalized with guards.
	if ok, err := p.MarkSettlementSigned(ctx, sellerBatch.ID, "d2lyZQ==", "sig1", "bh", 150, time.Now().Add(time.Minute), time.Now().UTC()); err != nil || !ok {
		t.Fatalf("mark signed: %v %v", ok, err)
	}
	// Re-marking from pending must fail (already signed).
	if ok, err := p.MarkSettlementSigned(ctx, sellerBatch.ID, "x", "y", "z", 1, time.Now(), time.Now().UTC()); err != nil || ok {
		t.Fatalf("double-sign allowed: ok=%v err=%v", ok, err)
	}
	if ok, _ := p.MarkSettlementBroadcast(ctx, sellerBatch.ID, time.Now().UTC()); !ok {
		t.Fatal("broadcast failed")
	}
	if ok, _ := p.FinalizeSettlement(ctx, sellerBatch.ID, time.Now().UTC()); !ok {
		t.Fatal("finalize failed")
	}
	// Terminal states accept no further transitions.
	if ok, _ := p.RestoreSettlement(ctx, sellerBatch.ID, "late", time.Now().UTC()); ok {
		t.Fatal("restore after finalize must no-op")
	}
}

func TestClaimSkipsBuyerWithoutAllowance(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("skip-delegate")
	linkWallet(t, p, "buyer", seedPubkey("skip-buyer-wallet"))
	linkWallet(t, p, "seller", seedPubkey("skip-seller-wallet"))
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)
	// NO allowance granted: deposit-rail buyer.

	settleDelegated(t, p, "sr1", "buyer", "p1", "seller", 0)
	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("skip-mint"), TokenProgram: seedPubkey("skip-tp"),
		TreasuryWallet: seedPubkey("skip-treasury"), LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil || len(created) != 0 {
		t.Fatalf("claim without allowance = %+v, %v; want empty", created, err)
	}
}

func TestClaimInsufficientAllowanceSkipsBuyer(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("insuf-delegate")
	linkWallet(t, p, "buyer", seedPubkey("insuf-buyer-wallet"))
	linkWallet(t, p, "seller", seedPubkey("insuf-seller-wallet"))
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)
	// Grant covers the reserve (4,800) so the gate passes, then a lower
	// observation (the poller mirroring a buyer re-approve) shrinks the
	// authority below the settled charge before the worker claims.
	grantAllowance(t, p, "buyer", delegate, 10_000, "obs-insuf")
	settleDelegated(t, p, "ir1", "buyer", "p1", "seller", 0)
	grantAllowance(t, p, "buyer", delegate, 1_000, "obs-insuf-lower")
	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("insuf-mint"), TokenProgram: seedPubkey("insuf-tp"),
		TreasuryWallet: seedPubkey("insuf-treasury"), LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil || len(created) != 0 {
		t.Fatalf("claim over allowance = %+v, %v; want empty", created, err)
	}
	// Allowance untouched.
	a := allowanceOf(t, p, "buyer", delegate)
	if a.AllowanceRaw != 1_000 {
		t.Fatalf("allowance changed on skipped claim: %d", a.AllowanceRaw)
	}
}

func TestClaimUnlinkedSellerSkipsBuyer(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("unlinked-delegate")
	linkWallet(t, p, "buyer", seedPubkey("unlinked-buyer-wallet"))
	// seller has NO linked wallet.
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)
	grantAllowance(t, p, "buyer", delegate, 100_000, "obs-unlinked")

	settleDelegated(t, p, "ur1", "buyer", "p1", "seller", 0)
	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("unlinked-mint"), TokenProgram: seedPubkey("unlinked-tp"),
		TreasuryWallet: seedPubkey("unlinked-treasury"), LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil || len(created) != 0 {
		t.Fatalf("claim with unlinked seller = %+v, %v; want empty", created, err)
	}
}

func TestRestoreSettlementRefundsAllowance(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("restore-delegate")
	linkWallet(t, p, "buyer", seedPubkey("restore-buyer-wallet"))
	linkWallet(t, p, "seller", seedPubkey("restore-seller-wallet"))
	seedCredit(t, p, "buyer", 100_000)
	seedCredit(t, p, "seller", 0)
	grantAllowance(t, p, "buyer", delegate, 10_000, "obs-restore")

	settleDelegated(t, p, "rr1", "buyer", "p1", "seller", 0)
	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("restore-mint"), TokenProgram: seedPubkey("restore-tp"),
		TreasuryWallet: seedPubkey("restore-treasury"), LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil || len(created) != 1 {
		t.Fatalf("claim = %+v, %v", created, err)
	}
	if ok, _ := p.MarkSettlementSigned(ctx, created[0].ID, "d2lyZQ==", "sig", "bh", 150, time.Now().Add(time.Minute), time.Now().UTC()); !ok {
		t.Fatal("sign failed")
	}
	if ok, err := p.RestoreSettlement(ctx, created[0].ID, "tx_failed", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("restore: %v %v", ok, err)
	}
	// Allowance refunded in full.
	a := allowanceOf(t, p, "buyer", delegate)
	if a.AllowanceRaw != 10_000 {
		t.Fatalf("allowance after restore = %d, want 10000", a.AllowanceRaw)
	}
	// Restored rows are terminal: sweep never returns them.
	due, err := p.SweepDelegationSettlements(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == created[0].ID {
			t.Fatal("restored row must not be swept")
		}
	}
}

func TestSettlementRefIsDeterministic(t *testing.T) {
	delegate := seedPubkey("ref-delegate")
	b1 := settlementBatch{Buyer: "acct", Source: "src", Dest: "dest", Amount: 5, LegIDs: []int64{3, 7, 5}}
	b2 := settlementBatch{Buyer: "acct", Source: "src", Dest: "dest", Amount: 5, LegIDs: []int64{7, 3, 5}}
	if settlementRef(delegate, b1) != settlementRef(delegate, b2) {
		t.Fatal("ref must be independent of leg order")
	}
	bf := settlementBatch{Buyer: "acct", Source: "src", Dest: "dest", Amount: 1, LegIDs: []int64{3}, IsFee: true}
	if settlementRef(delegate, b1) == settlementRef(delegate, bf) {
		t.Fatal("fee and xfer refs must differ")
	}
}

// linkWallet seeds a user_wallets row via the store's link API (used by
// walletsapi); fall back to direct SQL when the store method needs auth.
func linkWallet(t *testing.T, p *Postgres, accountID, wallet string) {
	t.Helper()
	if _, err := p.LinkWallet(context.Background(), accountID, wallet); err != nil {
		t.Fatalf("LinkWallet: %v", err)
	}
}

func allowanceOf(t *testing.T, p *Postgres, accountID, delegate string) *billing.Allowance {
	t.Helper()
	rows, err := p.AccountAllowances(context.Background(), accountID)
	if err != nil {
		t.Fatalf("AccountAllowances: %v", err)
	}
	for _, a := range rows {
		if a.Delegate == delegate {
			return &a
		}
	}
	return nil
}
