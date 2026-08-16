package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

// walletForSeed builds a base58 wallet string of the right length for
// deposit_events.from_wallet (the column is TEXT; only non-empty is required).
func walletForSeed(n int) string { return "wallet-" + strconv.Itoa(n) }

func TestInsertDepositEventIdempotent(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	ev := billing.DepositEvent{
		TransactionSignature: "sig-idem-1",
		InstructionIndex:     0,
		Slot:                 10,
		FromWallet:           walletForSeed(1),
		AmountRaw:            1_000_000,
	}
	inserted, err := p.InsertDepositEvent(ctx, ev)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	inserted2, err := p.InsertDepositEvent(ctx, ev)
	if err != nil {
		t.Fatalf("second insert err: %v", err)
	}
	if inserted2 {
		t.Fatal("second insert should be a no-op (inserted=false)")
	}
}

func TestInsertDepositEventValidation(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	cases := []billing.DepositEvent{
		{InstructionIndex: 0, FromWallet: "w", AmountRaw: 1},          // empty signature
		{TransactionSignature: "sig", FromWallet: "w", AmountRaw: 0},  // zero amount
		{TransactionSignature: "sig", FromWallet: "w", AmountRaw: -1}, // negative amount
		{TransactionSignature: "sig", AmountRaw: 1},                   // empty wallet
	}
	for i, c := range cases {
		if _, err := p.InsertDepositEvent(ctx, c); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("case %d: err = %v, want ErrInvalid", i, err)
		}
	}
}

func TestDepositCursorCompareAndSet(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const ata = "ata-test-1"

	got, err := p.DepositCursor(ctx, ata)
	if err != nil || got != "" {
		t.Fatalf("initial cursor = %q err=%v, want empty,nil", got, err)
	}
	advanced, err := p.AdvanceDepositCursor(ctx, ata, "", "sig-1")
	if err != nil || !advanced {
		t.Fatalf("initial advance = (%v,%v), want true,nil", advanced, err)
	}
	if got, err = p.DepositCursor(ctx, ata); err != nil || got != "sig-1" {
		t.Fatalf("cursor after first advance = %q err=%v, want sig-1,nil", got, err)
	}
	if advanced, err = p.AdvanceDepositCursor(ctx, ata, "", "sig-2"); err != nil || advanced {
		t.Fatalf("stale empty advance = (%v,%v), want false,nil", advanced, err)
	}
	if advanced, err = p.AdvanceDepositCursor(ctx, ata, "sig-x", "sig-2"); err != nil || advanced {
		t.Fatalf("stale compare advance = (%v,%v), want false,nil", advanced, err)
	}
	if advanced, err = p.AdvanceDepositCursor(ctx, ata, "sig-1", "sig-2"); err != nil || !advanced {
		t.Fatalf("matching compare advance = (%v,%v), want true,nil", advanced, err)
	}
	if got, err = p.DepositCursor(ctx, ata); err != nil || got != "sig-2" {
		t.Fatalf("cursor after second advance = %q err=%v, want sig-2,nil", got, err)
	}
}

func TestApplyDepositCreditsAccountAndLedger(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-apply-1"
	const wallet = "wallet-apply"
	const amount int64 = 2_500_000

	ev := billing.DepositEvent{
		TransactionSignature: "sig-apply-1",
		InstructionIndex:     1,
		Slot:                 42,
		FromWallet:           wallet,
		AmountRaw:            amount,
	}
	if _, err := p.InsertDepositEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	out, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, account, time.Now())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.AssignmentState != "assigned" || out.AssignedAccountID == nil || *out.AssignedAccountID != account || out.CreditedAt == nil {
		t.Fatalf("apply result = %+v", out)
	}
	credit, err := p.AccountCredit(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if credit.CreditRaw != amount {
		t.Fatalf("credit_raw = %d, want %d", credit.CreditRaw, amount)
	}

	// Reconciliation: SUM(ledger.delta_raw) for the account must equal credit.
	var sum int64
	if err := p.pool.QueryRow(ctx, `SELECT COALESCE(SUM(delta_raw),0) FROM credit_ledger WHERE account_id = $1`, account).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != amount {
		t.Fatalf("ledger sum = %d, want %d", sum, amount)
	}
}

func TestApplyDepositIsIdempotent(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-idem-2"
	ev := billing.DepositEvent{
		TransactionSignature: "sig-idem-2",
		InstructionIndex:     0,
		Slot:                 7,
		FromWallet:           "wallet-idem-2",
		AmountRaw:            1_000,
	}
	if _, err := p.InsertDepositEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, account, time.Now()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, account, time.Now()); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	credit, _ := p.AccountCredit(ctx, account)
	if credit.CreditRaw != 1_000 {
		t.Fatalf("credit_raw = %d after re-apply, want 1000 (no double-credit)", credit.CreditRaw)
	}
}

func TestApplyDepositUnknownReturnsNotFound(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	_, err := p.ApplyDeposit(ctx, "sig-missing", 0, "acct-x", time.Now())
	if !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestApplyDepositSkippedIsNoOp(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-skip"
	ev := billing.DepositEvent{
		TransactionSignature: "sig-skip-1",
		InstructionIndex:     0,
		Slot:                 1,
		FromWallet:           "wallet-skip",
		AmountRaw:            500,
	}
	if _, err := p.InsertDepositEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE deposit_events SET assignment_state='skipped' WHERE transaction_signature=$1`, ev.TransactionSignature); err != nil {
		t.Fatal(err)
	}
	out, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, account, time.Now())
	if err != nil {
		t.Fatalf("apply skipped: %v", err)
	}
	if out.AssignmentState != "skipped" {
		t.Fatalf("state = %q, want skipped", out.AssignmentState)
	}
	credit, _ := p.AccountCredit(ctx, account)
	if credit.CreditRaw != 0 {
		t.Fatalf("credit_raw = %d, skipped deposit must not credit", credit.CreditRaw)
	}
}

func TestApplyDepositConflictOnOtherAccount(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	ev := billing.DepositEvent{
		TransactionSignature: "sig-conflict-1",
		InstructionIndex:     0,
		Slot:                 1,
		FromWallet:           "wallet-conflict",
		AmountRaw:            800,
	}
	if _, err := p.InsertDepositEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, "acct-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := p.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, "acct-b", time.Now())
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict (already credited to another account)", err)
	}
}

func TestReconcileDepositsForWallet(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const wallet = "wallet-recon"
	const account = "acct-recon"
	// Three unassigned deposits from the wallet, two already-assigned (to
	// another account) that must be ignored, and one from a different wallet.
	deposits := []billing.DepositEvent{
		{TransactionSignature: "sig-r1", InstructionIndex: 0, Slot: 1, FromWallet: wallet, AmountRaw: 100},
		{TransactionSignature: "sig-r2", InstructionIndex: 0, Slot: 2, FromWallet: wallet, AmountRaw: 200},
		{TransactionSignature: "sig-r3", InstructionIndex: 1, Slot: 3, FromWallet: wallet, AmountRaw: 300},
		{TransactionSignature: "sig-r4", InstructionIndex: 0, Slot: 4, FromWallet: "other-wallet", AmountRaw: 999},
	}
	for _, d := range deposits {
		if _, err := p.InsertDepositEvent(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	n, err := p.ReconcileDepositsForWallet(ctx, wallet, account, time.Now())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 3 {
		t.Fatalf("credited %d, want 3", n)
	}
	credit, _ := p.AccountCredit(ctx, account)
	if credit.CreditRaw != 600 {
		t.Fatalf("credit_raw = %d, want 600", credit.CreditRaw)
	}

	// Re-running is a no-op (no unassigned deposits remain for the wallet).
	n2, err := p.ReconcileDepositsForWallet(ctx, wallet, account, time.Now())
	if err != nil {
		t.Fatalf("reconcile re-run: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("re-run credited %d, want 0 (idempotent)", n2)
	}

	// The other-wallet deposit is still unassigned.
	var state string
	if err := p.pool.QueryRow(ctx, `SELECT assignment_state FROM deposit_events WHERE transaction_signature=$1`, "sig-r4").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unassigned" {
		t.Fatalf("other-wallet deposit state = %q, want unassigned", state)
	}
}

func TestListDepositEventsPagination(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-list"
	const wallet = "wallet-list"
	const n = 5
	// Insert deposits with distinct, increasing seen_at so ordering is stable
	// regardless of the column's DEFAULT now().
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		sig := "sig-list-" + strconv.Itoa(i)
		if _, err := p.pool.Exec(ctx, `
			INSERT INTO deposit_events (transaction_signature, instruction_index, slot, from_wallet, amount_raw, assignment_state, assigned_account_id, credited_at, seen_at)
			VALUES ($1, 0, $2, $3, $4, 'assigned', $5, $6, $7)`,
			sig, i, wallet, int64(100+i), account, base.Add(time.Duration(i)*time.Second), base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	page, cursor, err := p.ListDepositEvents(ctx, account, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].TransactionSignature != "sig-list-4" || page[1].TransactionSignature != "sig-list-3" {
		t.Fatalf("page 1 = %+v", page)
	}
	if cursor == nil {
		t.Fatal("cursor should be non-nil when more rows remain")
	}
	page2, cursor2, err := p.ListDepositEvents(ctx, account, cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].TransactionSignature != "sig-list-2" || page2[1].TransactionSignature != "sig-list-1" {
		t.Fatalf("page 2 = %+v", page2)
	}
	page3, cursor3, err := p.ListDepositEvents(ctx, account, cursor2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 || page3[0].TransactionSignature != "sig-list-0" || cursor3 != nil {
		t.Fatalf("page 3 = %+v cursor=%v", page3, cursor3)
	}
}

func TestAccountForWallet(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	const account = "acct-afw"
	const wallet = "wallet-afw-1"
	if got, ok, err := p.AccountForWallet(ctx, wallet); err != nil || ok {
		t.Fatalf("before link: got=%q ok=%v err=%v, want ok=false", got, ok, err)
	}
	if _, err := p.LinkWallet(ctx, account, wallet); err != nil {
		t.Fatal(err)
	}
	got, ok, err := p.AccountForWallet(ctx, wallet)
	if err != nil || !ok || got != account {
		t.Fatalf("after link: got=%q ok=%v err=%v, want %s", got, ok, err, account)
	}
	if _, ok, err := p.AccountForWallet(ctx, ""); err != nil || ok {
		t.Fatalf("empty wallet: ok=%v err=%v, want ok=false", ok, err)
	}
}
