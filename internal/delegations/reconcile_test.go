package delegations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// fakeReconcileStore is the Reconciler's canned audit surface.
type fakeReconcileStore struct {
	ledger    billing.Reconciliation
	merkle    billing.MerkleSummary
	allowance []billing.Allowance
	inFlight  map[string]int64
	exposure  billing.ExposureSummary
	wallet    string
	hasWallet bool

	ledgerErr  error
	merkleErr  error
	allowErr   error
	inFlightEr error
	exposeErr  error
}

func (f *fakeReconcileStore) ReconcileAccount(context.Context, string, time.Time) (billing.Reconciliation, error) {
	if f.ledgerErr != nil {
		return billing.Reconciliation{}, f.ledgerErr
	}
	return f.ledger, nil
}
func (f *fakeReconcileStore) AccountMerkleSummary(context.Context, string) (billing.MerkleSummary, error) {
	if f.merkleErr != nil {
		return billing.MerkleSummary{}, f.merkleErr
	}
	return f.merkle, nil
}
func (f *fakeReconcileStore) AccountAllowances(context.Context, string) ([]billing.Allowance, error) {
	if f.allowErr != nil {
		return nil, f.allowErr
	}
	return f.allowance, nil
}
func (f *fakeReconcileStore) InFlightSettlements(context.Context, string) (map[string]int64, error) {
	if f.inFlightEr != nil {
		return nil, f.inFlightEr
	}
	return f.inFlight, nil
}
func (f *fakeReconcileStore) SettlementExposure(context.Context, string) (billing.ExposureSummary, error) {
	if f.exposeErr != nil {
		return billing.ExposureSummary{}, f.exposeErr
	}
	return f.exposure, nil
}
func (f *fakeReconcileStore) PrimaryWalletForAccount(context.Context, string) (string, bool, error) {
	return f.wallet, f.hasWallet, nil
}

// §11.5.5: the Reconciler assembles ledger integrity + Merkle commitment +
// registry-vs-chain cross-check (expected = allowance − in-flight) + residual
// exposure, degrading gracefully when the RPC is unavailable.
func TestReconcilerReportCleanAccount(t *testing.T) {
	ctx := context.Background()
	store := &fakeReconcileStore{
		ledger: billing.Reconciliation{AccountID: "acct", CreditRaw: 45_000, ExpectedCredit: 45_000, InvariantHeld: true},
		merkle: billing.MerkleSummary{Root: []byte("0123456789abcdef0123456789abcdef"), LeafCount: 7},
		allowance: []billing.Allowance{
			{AccountID: "acct", Delegate: testAuthority, AllowanceRaw: 50_000},
		},
		inFlight:  map[string]int64{testAuthority: 5_000},
		wallet:    wallet1,
		hasWallet: true,
	}
	rpc := &fakeRPC{exists: map[string]bool{ataFor(wallet1): true}}
	rpc.byATA = map[string]solana.TokenDelegate{
		ataFor(wallet1): {Delegate: testAuthority, DelegatedAmountRaw: 45_000, Slot: 42},
	}
	rec, err := NewReconciler(store, rpc, testAuthority, testMint, testTokenProg)
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}
	rep, err := rec.Report(ctx, "acct")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !rep.Ledger.InvariantHeld || rep.Merkle.LeafCount != 7 {
		t.Fatalf("ledger/merkle wrong: %+v %+v", rep.Ledger, rep.Merkle)
	}
	if len(rep.Allowances) != 1 {
		t.Fatalf("allowance rows = %d", len(rep.Allowances))
	}
	row := rep.Allowances[0]
	if row.RegistryAllowanceRaw != 50_000 || row.InFlightRaw != 5_000 || row.ExpectedChainRaw != 45_000 {
		t.Fatalf("cross-check inputs wrong: %+v", row)
	}
	if row.ObservedChainRaw == nil || *row.ObservedChainRaw != 45_000 {
		t.Fatalf("observed = %v, want 45000", row.ObservedChainRaw)
	}
	if row.DivergenceRaw == nil || *row.DivergenceRaw != 0 {
		t.Fatalf("divergence = %v, want 0", row.DivergenceRaw)
	}
	if rep.Exposure.RestoredCount != 0 {
		t.Fatalf("exposure = %+v", rep.Exposure)
	}
}

func TestReconcilerReportDrainedATA(t *testing.T) {
	// Buyer drained the ATA under a live delegation: chain reads 0 while the
	// registry still holds 50_000 (no in-flight) → divergence −50_000.
	store := &fakeReconcileStore{
		allowance: []billing.Allowance{
			{AccountID: "acct", Delegate: testAuthority, AllowanceRaw: 50_000},
		},
		inFlight:  map[string]int64{},
		wallet:    wallet1,
		hasWallet: true,
	}
	rpc := &fakeRPC{exists: map[string]bool{ataFor(wallet1): true}}
	rpc.byATA = map[string]solana.TokenDelegate{
		ataFor(wallet1): {Delegate: testAuthority, DelegatedAmountRaw: 0, Slot: 99},
	}
	rec, _ := NewReconciler(store, rpc, testAuthority, testMint, testTokenProg)
	rep, err := rec.Report(context.Background(), "acct")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	row := rep.Allowances[0]
	if row.DivergenceRaw == nil || *row.DivergenceRaw != -50_000 {
		t.Fatalf("divergence = %v, want -50000", row.DivergenceRaw)
	}
}

func TestReconcilerReportExposureAndUnobserved(t *testing.T) {
	store := &fakeReconcileStore{
		allowance: []billing.Allowance{
			{AccountID: "acct", Delegate: testAuthority, AllowanceRaw: 10_000},
		},
		inFlight: map[string]int64{},
		exposure: billing.ExposureSummary{RestoredCount: 2, RestoredRaw: 7_000},
		wallet:   wallet1, hasWallet: true,
	}
	// RPC broken: the report must still carry ledger + exposure, with the
	// cross-check explicitly unobserved (nil), not zero.
	rpc := &fakeRPC{err: errors.New("rpc down")}
	rec, _ := NewReconciler(store, rpc, testAuthority, testMint, testTokenProg)
	rep, err := rec.Report(context.Background(), "acct")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	row := rep.Allowances[0]
	if row.ObservedChainRaw != nil || row.DivergenceRaw != nil {
		t.Fatalf("failed RPC must leave observation nil: %+v", row)
	}
	if rep.Exposure.RestoredCount != 2 || rep.Exposure.RestoredRaw != 7_000 {
		t.Fatalf("exposure lost: %+v", rep.Exposure)
	}
}

func TestReconcilerReportNoWallet(t *testing.T) {
	store := &fakeReconcileStore{
		allowance: []billing.Allowance{
			{AccountID: "acct", Delegate: testAuthority, AllowanceRaw: 0},
		},
		inFlight: map[string]int64{},
	}
	rpc := &fakeRPC{}
	rec, _ := NewReconciler(store, rpc, testAuthority, testMint, testTokenProg)
	rep, err := rec.Report(context.Background(), "acct")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	row := rep.Allowances[0]
	// No wallet → no ATA → no delegation: observed 0, divergence −0.
	if row.ObservedChainRaw == nil || *row.ObservedChainRaw != 0 {
		t.Fatalf("observed = %v, want 0", row.ObservedChainRaw)
	}
	if rpc.calls != 0 {
		t.Fatalf("RPC called %d times with no wallet", rpc.calls)
	}
}

func TestReconcilerRequiresConfig(t *testing.T) {
	store := &fakeReconcileStore{}
	if _, err := NewReconciler(store, &fakeRPC{}, "", testMint, testTokenProg); err == nil {
		t.Fatal("authority required")
	}
	if _, err := NewReconciler(store, &fakeRPC{}, testAuthority, "", testTokenProg); err == nil {
		t.Fatal("mint required")
	}
}
