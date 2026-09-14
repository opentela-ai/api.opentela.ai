package delegations

import (
	"context"
	"fmt"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// Reconciler implements design §11.5.5: the periodic audit that (a) commits
// the account's ledger to a deterministic Merkle root users can verify
// independently, (b) cross-checks the allowance registry against what the
// chain actually reads (the poller mirrors; this audits the mirror), and
// (c) reports residual exposure — settled charges whose on-chain collection
// terminally failed (restored batches, §16 row 14).
//
// The continuous counterpart is the poller (§11.3): it corrects drift; this
// measures and publishes it. A cron or the console can drive Report on any
// cadence — every call is a pure read plus one RPC round trip per account.
type Reconciler struct {
	store reconcileStore
	rpc   chainReader
	// authority is the settlement authority's base58 pubkey — the delegate
	// buyers approve. Reads of delegations to other delegates are reported
	// as observed=0 for that account's rows.
	authority    string
	mint         string
	tokenProgram string
	logf         func(format string, args ...any)
}

// reconcileStore is the audit surface the Reconciler needs.
type reconcileStore interface {
	// ReconcileAccount is the §5 ledger-integrity check (credit vs SUM,
	// reserved vs open reservations).
	ReconcileAccount(ctx context.Context, accountID string, now time.Time) (billing.Reconciliation, error)
	// AccountMerkleSummary commits the ledger: deterministic root + count.
	AccountMerkleSummary(ctx context.Context, accountID string) (billing.MerkleSummary, error)
	// AccountAllowances lists the registry rows to audit.
	AccountAllowances(ctx context.Context, accountID string) ([]billing.Allowance, error)
	// InFlightSettlements: consumed-but-unfinalized amounts per delegate —
	// the gap between registry and chain that is EXPECTED, not divergence.
	InFlightSettlements(ctx context.Context, accountID string) (map[string]int64, error)
	// SettlementExposure: terminally failed (restored) settlement totals.
	SettlementExposure(ctx context.Context, accountID string) (billing.ExposureSummary, error)
	// PrimaryWalletForAccount resolves the ATA holder to read on-chain.
	PrimaryWalletForAccount(ctx context.Context, accountID string) (string, bool, error)
}

// NewReconciler builds the §11.5.5 auditor. The authority/mint/tokenProgram
// triple matches the poller's configuration.
func NewReconciler(store reconcileStore, rpc chainReader, authority, mint, tokenProgram string) (*Reconciler, error) {
	if authority == "" {
		return nil, fmt.Errorf("delegations: reconciler requires the settlement authority")
	}
	if mint == "" || tokenProgram == "" {
		return nil, fmt.Errorf("delegations: reconciler requires mint and token program")
	}
	return &Reconciler{
		store:        store,
		rpc:          rpc,
		authority:    authority,
		mint:         mint,
		tokenProgram: tokenProgram,
	}, nil
}

// SetLogger installs a printf-style logger.
func (r *Reconciler) SetLogger(fn func(format string, args ...any)) {
	r.logf = fn
}

func (r *Reconciler) logfOrDefault(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// Report assembles the full audit for one account. A chain read failure
// degrades only the Observed/Divergence fields (nil = unobserved) — the
// ledger commitment and exposure never depend on the RPC.
func (r *Reconciler) Report(ctx context.Context, accountID string) (billing.ReconciliationReport, error) {
	report := billing.ReconciliationReport{AccountID: accountID}

	ledger, err := r.store.ReconcileAccount(ctx, accountID, time.Time{})
	if err != nil {
		return report, fmt.Errorf("delegations: reconcile ledger: %w", err)
	}
	report.Ledger = ledger

	merkle, err := r.store.AccountMerkleSummary(ctx, accountID)
	if err != nil {
		return report, fmt.Errorf("delegations: reconcile merkle: %w", err)
	}
	report.Merkle = merkle

	allowances, err := r.store.AccountAllowances(ctx, accountID)
	if err != nil {
		return report, fmt.Errorf("delegations: reconcile allowances: %w", err)
	}
	inFlight, err := r.store.InFlightSettlements(ctx, accountID)
	if err != nil {
		return report, fmt.Errorf("delegations: reconcile in-flight: %w", err)
	}
	exposure, err := r.store.SettlementExposure(ctx, accountID)
	if err != nil {
		return report, fmt.Errorf("delegations: reconcile exposure: %w", err)
	}
	report.Exposure = exposure

	// One chain observation per account (one wallet, one ATA, one live
	// delegate at a time under SPL approve). When the read fails, the
	// cross-check rows carry Observed=nil and the report stays truthful
	// about what it could not see.
	observedRaw, observedDelegate, chainErr := r.observeChain(ctx, accountID)
	if chainErr != nil {
		r.logfOrDefault("reconcile account %s: chain read failed: %v", accountID, chainErr)
	}

	// Always non-nil: the manage endpoint reports chain_audited=true when
	// the reconciler ran, even for accounts with no registry rows.
	report.Allowances = []billing.AllowanceCrossCheck{}
	for _, a := range allowances {
		row := billing.AllowanceCrossCheck{
			Delegate:             a.Delegate,
			Active:               a.RevokedAt == nil,
			RegistryAllowanceRaw: a.AllowanceRaw,
			InFlightRaw:          inFlight[a.Delegate],
		}
		row.ExpectedChainRaw = row.RegistryAllowanceRaw - row.InFlightRaw
		if chainErr == nil {
			var observed int64
			if observedDelegate == a.Delegate {
				observed = int64(observedRaw)
			}
			row.ObservedChainRaw = &observed
			div := observed - row.ExpectedChainRaw
			row.DivergenceRaw = &div
		}
		report.Allowances = append(report.Allowances, row)
	}
	return report, nil
}

// observeChain reads the account's wallet ATA delegation. Returns
// (delegatedAmount, delegatePubkey, err); err is nil even when the account
// has no wallet or the ATA does not exist (both mean "no delegation").
func (r *Reconciler) observeChain(ctx context.Context, accountID string) (uint64, string, error) {
	wallet, ok, err := r.store.PrimaryWalletForAccount(ctx, accountID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve wallet: %w", err)
	}
	if !ok || wallet == "" {
		return 0, "", nil
	}
	walletBytes, err := solana.DecodeBase58(wallet, solana.PublicKeyBytes)
	if err != nil {
		return 0, "", fmt.Errorf("wallet %s is not valid base58: %w", wallet, err)
	}
	mintBytes, err := solana.DecodeBase58(r.mint, solana.PublicKeyBytes)
	if err != nil {
		return 0, "", fmt.Errorf("mint is not valid base58: %w", err)
	}
	tpBytes, err := solana.DecodeBase58(r.tokenProgram, solana.PublicKeyBytes)
	if err != nil {
		return 0, "", fmt.Errorf("token program is not valid base58: %w", err)
	}
	ataBytes, err := solana.AssociatedTokenAddress(walletBytes, mintBytes, tpBytes)
	if err != nil {
		return 0, "", fmt.Errorf("derive ATA: %w", err)
	}
	td, exists, err := r.rpc.TokenDelegation(ctx, solana.EncodeBase58(ataBytes))
	if err != nil {
		return 0, "", fmt.Errorf("read token delegation: %w", err)
	}
	if !exists {
		return 0, "", nil // no ATA → no delegation to anyone
	}
	return td.DelegatedAmountRaw, td.Delegate, nil
}
