package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

// grantAllowance is a helper wrapping UpsertAllowance for tests.
func grantAllowance(t *testing.T, p *Postgres, accountID, delegate string, raw int64, ref string) billing.AllowanceChangeResult {
	t.Helper()
	res, err := p.UpsertAllowance(context.Background(), billing.AllowanceChange{
		AccountID:    accountID,
		Delegate:     delegate,
		AllowanceRaw: raw,
		ObservedAt:   time.Now().UTC(),
		Ref:          ref,
	})
	if err != nil {
		t.Fatalf("UpsertAllowance(%s, %d): %v", delegate, raw, err)
	}
	return res
}

func creditOf(t *testing.T, p *Postgres, accountID string) billing.AccountCredit {
	t.Helper()
	c, err := p.AccountCredit(context.Background(), accountID)
	if err != nil {
		t.Fatalf("AccountCredit: %v", err)
	}
	return c
}

func TestUpsertAllowanceGrantMirrorsCredit(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	res := grantAllowance(t, p, "acct-1", "SettleAuth1", 1_000_000, "obs-sig-1")
	if res.PrevAllowanceRaw != 0 || res.AppliedRaw != 1_000_000 || res.ShortfallRaw != 0 {
		t.Fatalf("first grant result = %+v", res)
	}
	if got := creditOf(t, p, "acct-1").CreditRaw; got != 1_000_000 {
		t.Fatalf("credit after grant = %d, want 1000000", got)
	}

	// Higher re-approval: delta = +500k.
	res = grantAllowance(t, p, "acct-1", "SettleAuth1", 1_500_000, "obs-sig-2")
	if res.AppliedRaw != 500_000 {
		t.Fatalf("raise result = %+v", res)
	}
	if got := creditOf(t, p, "acct-1").CreditRaw; got != 1_500_000 {
		t.Fatalf("credit after raise = %d", got)
	}

	// Exactly-once: replaying the same observation ref changes nothing.
	res = grantAllowance(t, p, "acct-1", "SettleAuth1", 1_500_000, "obs-sig-2")
	if res.AppliedRaw != 0 {
		t.Fatalf("replay result = %+v", res)
	}
	if got := creditOf(t, p, "acct-1").CreditRaw; got != 1_500_000 {
		t.Fatalf("credit after replay = %d", got)
	}

	// One ledger grant leg per ref, source 'delegation'.
	var legs int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM credit_ledger WHERE account_id='acct-1' AND source='delegation'`).Scan(&legs); err != nil {
		t.Fatal(err)
	}
	if legs != 2 {
		t.Fatalf("delegation legs = %d, want 2", legs)
	}
}

func TestUpsertAllowanceRevokeClampsAtReserved(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	grantAllowance(t, p, "acct-2", "SettleAuth1", 1_000_000, "obs-r1")

	// Open reservation keeps its backing: reserve 400k.
	_, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID:      "req-rvk-1",
		BuyerAccountID: "acct-2",
		Service:        "svc",
		Model:          "model",
		ReservedAt:     time.Now().UTC(),
		Quotes: []billing.EligiblePeerQuote{{
			PeerID: "peer-1", SellerAccountID: "seller-1",
			InputPerMillion: 1_000, OutputPerMillion: 1_000,
		}},
		InputCeil: 100, OutputCeil: 100,
	})
	if err != nil {
		t.Fatalf("ReserveBilling: %v", err)
	}

	// Revoke to 0 with credit 1,000,000 / reserved R (the reservation's
	// worst-case cost): the debit clamps at the reservation; shortfall = R.
	res := grantAllowance(t, p, "acct-2", "SettleAuth1", 0, "obs-r2")
	if res.AppliedRaw != -1_000_000 {
		t.Fatalf("revoke applied = %d", res.AppliedRaw)
	}
	req, err := p.BillingRequest(ctx, "req-rvk-1")
	if err != nil {
		t.Fatalf("BillingRequest: %v", err)
	}
	if res.ShortfallRaw != req.ReservedRaw {
		t.Fatalf("shortfall = %d, want %d (the open reservation)", res.ShortfallRaw, req.ReservedRaw)
	}
	c := creditOf(t, p, "acct-2")
	if c.CreditRaw != req.ReservedRaw || c.ReservedRaw != req.ReservedRaw {
		t.Fatalf("credit after revoke = %+v", c)
	}

	// Registry shows revoked; the active list is empty.
	alls, err := p.AccountAllowances(ctx, "acct-2")
	if err != nil || len(alls) != 1 {
		t.Fatalf("AccountAllowances = %v, %v", alls, err)
	}
	if alls[0].Active() {
		t.Fatal("allowance must be revoked")
	}
	if dels, err := p.DelegateAllowances(ctx, "SettleAuth1"); err != nil || len(dels) != 0 {
		t.Fatalf("DelegateAllowances after revoke = %v, %v", dels, err)
	}
}

func TestReserveBillingBoundedByDelegation(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	// Seller peer must exist for settlement resolution later; only the
	// reserve path matters here, but keep quotes structurally valid.
	grantAllowance(t, p, "acct-3", "SettleAuth1", 500_000, "obs-b1")
	grantAllowance(t, p, "acct-3", "SettleAuth2", 300_000, "obs-b2")

	quote := billing.EligiblePeerQuote{
		PeerID: "peer-1", SellerAccountID: "seller-1",
		InputPerMillion: 1_000, OutputPerMillion: 1_000,
	}
	reserveReq := func(id string) billing.Reservation {
		return billing.Reservation{
			RequestID: id, BuyerAccountID: "acct-3", Service: "svc", Model: "model",
			ReservedAt: time.Now().UTC(),
			Quotes:     []billing.EligiblePeerQuote{quote},
			InputCeil:  100, OutputCeil: 100,
		}
	}

	// Σ allowance = 800k while credit = 800k (mirrored). A reserve whose
	// worst case exceeds the allowance must fail even though plain credit
	// would allow it — simulate by granting only 500k worth of backing…
	// (credit is 800k here, so this checks the min() bite directly.)
	if _, err := p.ReserveBilling(ctx, reserveReq("req-b1")); err != nil {
		t.Fatalf("reserve within allowance: %v", err)
	}

	// Second account: credit inflated beyond the allowance (drift
	// simulation) — the allowance must bind. Grant 100k, then hand-inflate
	// credit by a fake deposit of 900k.
	grantAllowance(t, p, "acct-4", "SettleAuth1", 100_000, "obs-b3")
	if _, err := p.InsertDepositEvent(ctx, billing.DepositEvent{
		TransactionSignature: "sig-dep-4", InstructionIndex: 0,
		FromWallet: "W4", AmountRaw: 900_000, SeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyDeposit(ctx, "sig-dep-4", 0, "acct-4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// credit = 1,000,000, Σ active allowance = 100,000 → the bound bites.
	// Rates of 1e9/million with 100-input/200-output ceilings give a
	// worst-case reserve of 300,000 raw — far above the 100,000 allowance.
	_, err := p.ReserveBilling(ctx, billing.Reservation{
		RequestID: "req-b2", BuyerAccountID: "acct-4", Service: "svc", Model: "model",
		ReservedAt: time.Now().UTC(),
		Quotes: []billing.EligiblePeerQuote{{
			PeerID: "peer-1", SellerAccountID: "seller-1",
			InputPerMillion: 1_000_000_000, OutputPerMillion: 1_000_000_000,
		}},
		InputCeil: 100, OutputCeil: 200,
	})
	if !errors.Is(err, billing.ErrInsufficientCredit) {
		t.Fatalf("reserve above allowance: err = %v, want ErrInsufficientCredit", err)
	}
}

func TestUpsertAllowanceRevivesRegistry(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	grantAllowance(t, p, "acct-5", "SettleAuth1", 0, "obs-v0")
	alls, err := p.AccountAllowances(ctx, "acct-5")
	if err != nil || len(alls) != 1 || alls[0].Active() {
		t.Fatalf("post-zero registry = %+v, %v", alls, err)
	}

	// A later grant revives the row: revoked_at clears.
	grantAllowance(t, p, "acct-5", "SettleAuth1", 250_000, "obs-v1")
	alls, err = p.AccountAllowances(ctx, "acct-5")
	if err != nil || len(alls) != 1 || !alls[0].Active() {
		t.Fatalf("post-revive registry = %+v, %v", alls, err)
	}
	if dels, err := p.DelegateAllowances(ctx, "SettleAuth1"); err != nil || len(dels) != 1 {
		t.Fatalf("DelegateAllowances after revive = %v, %v", dels, err)
	}
}

func TestUpsertAllowanceValidation(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	base := billing.AllowanceChange{
		AccountID: "acct-6", Delegate: "d", AllowanceRaw: 10,
		ObservedAt: time.Now().UTC(), Ref: "obs-x",
	}
	if _, err := p.UpsertAllowance(ctx, billing.AllowanceChange{Delegate: "d", AllowanceRaw: 1, Ref: "r"}); err == nil {
		t.Fatal("missing account must fail")
	}
	if _, err := p.UpsertAllowance(ctx, billing.AllowanceChange{AccountID: "a", AllowanceRaw: 1, Ref: "r"}); err == nil {
		t.Fatal("missing delegate must fail")
	}
	neg := base
	neg.AllowanceRaw = -1
	if _, err := p.UpsertAllowance(ctx, neg); err == nil {
		t.Fatal("negative allowance must fail")
	}
	noRef := base
	noRef.Ref = ""
	if _, err := p.UpsertAllowance(ctx, noRef); err == nil {
		t.Fatal("missing ref must fail")
	}
	_ = ctx
}
