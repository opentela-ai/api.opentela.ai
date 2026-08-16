package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

func destWallet(n int) string {
	if n < 0 {
		n = -n
	}
	return "dest-" + randHex(8)
}

func TestReserveWithdrawalDebitsCreditAndWritesLeg(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-1"
	seedCredit(t, p, account, 1_000_000)

	now := time.Now().UTC()
	w, err := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID:         account,
		IdempotencyKey:    "idem-w-1",
		DestinationWallet: destWallet(1),
		AmountRaw:         250_000,
	}, now)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if w.State != billing.WithdrawalReserved || w.AmountRaw != 250_000 {
		t.Fatalf("withdrawal = %+v", w)
	}
	ac, err := p.AccountCredit(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if ac.CreditRaw != 750_000 {
		t.Fatalf("credit_raw = %d, want 750000", ac.CreditRaw)
	}
	if ac.ReservedRaw != 0 {
		t.Fatalf("reserved_raw = %d, want 0 (withdrawals do not bump reserved_raw)", ac.ReservedRaw)
	}
	// Exactly one `withdraw` ledger leg under the withdrawal ref.
	var n int64
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM credit_ledger WHERE account_id = $1 AND ref = $2 AND leg = 'withdraw'`,
		account, billing.WithdrawalRef(w.ID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("withdraw leg count = %d, want 1", n)
	}
}

func TestReserveWithdrawalIdempotent(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-2"
	seedCredit(t, p, account, 1_000_000)
	req := billing.WithdrawalRequest{
		AccountID:         account,
		IdempotencyKey:    "idem-w-2",
		DestinationWallet: destWallet(2),
		AmountRaw:         100_000,
	}
	now := time.Now().UTC()
	w1, err := p.ReserveWithdrawal(ctx, req, now)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	w2, err := p.ReserveWithdrawal(ctx, req, now.Add(time.Second))
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if w1.ID != w2.ID {
		t.Fatalf("idempotent reserve returned different ids: %d vs %d", w1.ID, w2.ID)
	}
	// Credit debited exactly once.
	ac, _ := p.AccountCredit(ctx, account)
	if ac.CreditRaw != 900_000 {
		t.Fatalf("credit_raw = %d, want 900000 (debited twice?)", ac.CreditRaw)
	}
}

func TestReserveWithdrawalInsufficientCredit(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-3"
	seedCredit(t, p, account, 100)
	if _, err := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID:         account,
		IdempotencyKey:    "idem-w-3",
		DestinationWallet: destWallet(3),
		AmountRaw:         1_000,
	}, time.Now().UTC()); !errors.Is(err, billing.ErrInsufficientCredit) {
		t.Fatalf("err = %v, want ErrInsufficientCredit", err)
	}
	// No withdrawal row, no ledger leg.
	ac, _ := p.AccountCredit(ctx, account)
	if ac.CreditRaw != 100 {
		t.Fatalf("credit_raw = %d, want 100 (unchanged)", ac.CreditRaw)
	}
}

func TestMarkWithdrawalSignedOnlyFromReserved(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-4"
	seedCredit(t, p, account, 1_000_000)
	now := time.Now().UTC()
	w, err := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID:         account,
		IdempotencyKey:    "idem-w-4",
		DestinationWallet: destWallet(4),
		AmountRaw:         50_000,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(90 * time.Second)
	const lastValidBlockHeight = uint64(12345)
	updated, err := p.MarkWithdrawalSigned(ctx, w.ID, "wire-b64", "sig-b58", "bh-b58", lastValidBlockHeight, exp, now.Add(time.Second))
	if err != nil {
		t.Fatalf("mark signed: %v", err)
	}
	if updated.State != billing.WithdrawalSigned || updated.SignedWire != "wire-b64" || updated.Signature != "sig-b58" {
		t.Fatalf("updated = %+v", updated)
	}
	if updated.LastValidBlockHeight == nil || *updated.LastValidBlockHeight != lastValidBlockHeight {
		t.Fatalf("last_valid_block_height = %v, want %d", updated.LastValidBlockHeight, lastValidBlockHeight)
	}
	if updated.BlockhashExpiresAt == nil {
		t.Fatal("blockhash expires nil")
	}
	// PostgreSQL timestamptz truncates to microseconds; round both sides
	// before comparing so the round-trip does not flake.
	if !updated.BlockhashExpiresAt.Truncate(time.Microsecond).Equal(exp.Truncate(time.Microsecond)) {
		t.Fatalf("blockhash expires = %v, want %v", updated.BlockhashExpiresAt, exp)
	}
	// Second call (already signed) is a clean no-op: no error, state unchanged.
	again, err := p.MarkWithdrawalSigned(ctx, w.ID, "wire-DIFFERENT", "sig-DIFFERENT", "bh-DIFFERENT", lastValidBlockHeight+1, exp.Add(time.Hour), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("second mark signed: %v", err)
	}
	if again.SignedWire != "wire-b64" {
		t.Fatalf("signed wire overwritten: got %q want wire-b64", again.SignedWire)
	}
}

func TestMarkWithdrawalBroadcastOnlyFromSigned(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-5"
	seedCredit(t, p, account, 1_000_000)
	now := time.Now().UTC()
	w, _ := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID: account, IdempotencyKey: "idem-w-5", DestinationWallet: destWallet(5), AmountRaw: 50_000,
	}, now)
	if _, err := p.MarkWithdrawalSigned(ctx, w.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MarkWithdrawalBroadcast(ctx, w.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("mark broadcast: %v", err)
	}
	got, err := p.Withdrawal(ctx, account, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != billing.WithdrawalBroadcast {
		t.Fatalf("state = %q, want broadcast", got.State)
	}
}

func TestFinalizeWithdrawalNoBalanceChange(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-6"
	seedCredit(t, p, account, 1_000_000)
	now := time.Now().UTC()
	w, _ := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID: account, IdempotencyKey: "idem-w-6", DestinationWallet: destWallet(6), AmountRaw: 300_000,
	}, now)
	if _, err := p.MarkWithdrawalSigned(ctx, w.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MarkWithdrawalBroadcast(ctx, w.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FinalizeWithdrawal(ctx, w.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	got, _ := p.Withdrawal(ctx, account, w.ID)
	if got.State != billing.WithdrawalFinalized {
		t.Fatalf("state = %q, want finalized", got.State)
	}
	ac, _ := p.AccountCredit(ctx, account)
	if ac.CreditRaw != 700_000 {
		t.Fatalf("credit_raw = %d, want 700000 (finalize must not move balance)", ac.CreditRaw)
	}
}

func TestRestoreWithdrawalReturnsCredit(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-7"
	seedCredit(t, p, account, 1_000_000)
	now := time.Now().UTC()
	w, _ := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID: account, IdempotencyKey: "idem-w-7", DestinationWallet: destWallet(7), AmountRaw: 400_000,
	}, now)
	if _, err := p.MarkWithdrawalSigned(ctx, w.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RestoreWithdrawal(ctx, w.ID, "blockhash_expired", now.Add(time.Second)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := p.Withdrawal(ctx, account, w.ID)
	if got.State != billing.WithdrawalRestored {
		t.Fatalf("state = %q, want restored", got.State)
	}
	if got.Error != "blockhash_expired" {
		t.Fatalf("error = %q, want blockhash_expired", got.Error)
	}
	ac, _ := p.AccountCredit(ctx, account)
	if ac.CreditRaw != 1_000_000 {
		t.Fatalf("credit_raw = %d, want 1000000 (restored)", ac.CreditRaw)
	}
	// A `restore` ledger leg exists under the same ref.
	var n int64
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM credit_ledger WHERE account_id = $1 AND ref = $2 AND leg = 'restore'`,
		account, billing.WithdrawalRef(w.ID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restore leg count = %d, want 1", n)
	}
}

func TestRestoreWithdrawalAfterFinalizeIsNoOp(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-8"
	seedCredit(t, p, account, 1_000_000)
	now := time.Now().UTC()
	w, _ := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
		AccountID: account, IdempotencyKey: "idem-w-8", DestinationWallet: destWallet(8), AmountRaw: 200_000,
	}, now)
	p.MarkWithdrawalSigned(ctx, w.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now)
	p.MarkWithdrawalBroadcast(ctx, w.ID, now)
	p.FinalizeWithdrawal(ctx, w.ID, now)
	if _, err := p.RestoreWithdrawal(ctx, w.ID, "late", now.Add(time.Hour)); err != nil {
		t.Fatalf("restore after finalize: %v", err)
	}
	ac, _ := p.AccountCredit(ctx, account)
	if ac.CreditRaw != 800_000 {
		t.Fatalf("credit_raw = %d, want 800000 (restore must not double-credit a finalized withdrawal)", ac.CreditRaw)
	}
	got, _ := p.Withdrawal(ctx, account, w.ID)
	if got.State != billing.WithdrawalFinalized {
		t.Fatalf("state = %q, want finalized (restore did not regress)", got.State)
	}
}

func TestSweepWithdrawalsDue(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-9"
	seedCredit(t, p, account, 10_000_000)
	now := time.Now().UTC()
	mk := func(key string, amount int64) billing.Withdrawal {
		w, _ := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
			AccountID: account, IdempotencyKey: key, DestinationWallet: destWallet(0), AmountRaw: amount,
		}, now)
		return w
	}
	r1 := mk("sweep-r1", 100_000)
	r2 := mk("sweep-r2", 100_000)
	s1 := mk("sweep-s1", 100_000)
	p.MarkWithdrawalSigned(ctx, s1.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now)
	b1 := mk("sweep-b1", 100_000)
	p.MarkWithdrawalSigned(ctx, b1.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now)
	p.MarkWithdrawalBroadcast(ctx, b1.ID, now)
	f := mk("sweep-f", 100_000)
	p.MarkWithdrawalSigned(ctx, f.ID, "wire", "sig", "bh", 100, now.Add(90*time.Second), now)
	p.MarkWithdrawalBroadcast(ctx, f.ID, now)
	p.FinalizeWithdrawal(ctx, f.ID, now)

	due, err := p.SweepWithdrawalsDue(ctx, 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	seen := map[int64]string{}
	for _, w := range due {
		seen[w.ID] = string(w.State)
	}
	if seen[r1.ID] != string(billing.WithdrawalReserved) {
		t.Errorf("r1: got %q, want reserved", seen[r1.ID])
	}
	if seen[r2.ID] != string(billing.WithdrawalReserved) {
		t.Errorf("r2: got %q, want reserved", seen[r2.ID])
	}
	if seen[s1.ID] != string(billing.WithdrawalSigned) {
		t.Errorf("s1: got %q, want signed", seen[s1.ID])
	}
	if seen[b1.ID] != string(billing.WithdrawalBroadcast) {
		t.Errorf("b1: got %q, want broadcast", seen[b1.ID])
	}
	if _, ok := seen[f.ID]; ok {
		t.Errorf("finalized withdrawal %d should not be due", f.ID)
	}
	if len(due) != 4 {
		t.Errorf("due count = %d, want 4", len(due))
	}
}

func TestListWithdrawalsPagination(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-w-10"
	seedCredit(t, p, account, 100_000_000)
	now := time.Now().UTC()
	var first int64
	for i := 0; i < 5; i++ {
		w, err := p.ReserveWithdrawal(ctx, billing.WithdrawalRequest{
			AccountID: account, IdempotencyKey: "page-" + randHex(6), DestinationWallet: destWallet(i), AmountRaw: 1_000,
		}, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = w.ID
		}
	}
	// First page (limit 3) — newest first, so the first row is the most recent.
	page1, cursor1, err := p.ListWithdrawals(ctx, account, nil, 3)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	if cursor1 == nil {
		t.Fatal("cursor1 nil")
	}
	page2, cursor2, err := p.ListWithdrawals(ctx, account, cursor1, 3)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if cursor2 != nil {
		t.Fatalf("cursor2 = %v, want nil (end of results)", cursor2)
	}
	// The oldest row (first inserted) is the last of page2.
	if page2[len(page2)-1].ID != first {
		t.Fatalf("oldest id = %d, want %d", page2[len(page2)-1].ID, first)
	}
	// A different account sees none.
	other, _, err := p.ListWithdrawals(ctx, "acct-other", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other account saw %d withdrawals", len(other))
	}
}
