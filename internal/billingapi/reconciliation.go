package billingapi

// §11.5.5 reconciliation endpoints: the account's audit report (ledger
// integrity, Merkle commitment, registry-vs-chain cross-check, residual
// exposure) and row-level Merkle inclusion proofs so a user can verify a
// ledger row against the published root without trusting this API.

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/httputil"
)

type reconciliationResponse struct {
	AccountID    string               `json:"account_id"`
	GeneratedAt  time.Time            `json:"generated_at"`
	Ledger       ledgerIntegrityJSON  `json:"ledger"`
	Merkle       merkleJSON           `json:"merkle"`
	Allowances   []allowanceCheckJSON `json:"allowances"`
	Exposure     exposureJSON         `json:"exposure"`
	ChainAudited bool                 `json:"chain_audited"`
}

type ledgerIntegrityJSON struct {
	CreditRaw        rawInt64 `json:"credit_raw"`
	ExpectedCredit   rawInt64 `json:"expected_credit_raw"`
	DriftCredit      rawInt64 `json:"drift_credit_raw"`
	ReservedRaw      rawInt64 `json:"reserved_raw"`
	ExpectedReserved rawInt64 `json:"expected_reserved_raw"`
	DriftReserved    rawInt64 `json:"drift_reserved_raw"`
	LedgerRows       int64    `json:"ledger_rows"`
	OpenReservations int64    `json:"open_reservations"`
	InvariantHeld    bool     `json:"invariant_held"`
}

type merkleJSON struct {
	// Root is hex(SHA-256); recompute per the documented rules in
	// internal/billing/merkle.go (leaf/node/pad domains) from the ledger
	// the /manage/billing/ledger endpoint serves.
	Root      string `json:"root"`
	LeafCount int64  `json:"leaf_count"`
}

type allowanceCheckJSON struct {
	Delegate             string   `json:"delegate"`
	Active               bool     `json:"active"`
	RegistryAllowanceRaw rawInt64 `json:"registry_allowance_raw"`
	InFlightRaw          rawInt64 `json:"in_flight_raw"`
	ExpectedChainRaw     rawInt64 `json:"expected_chain_raw"`
	// ObservedChainRaw/DivergenceRaw are null when the RPC read failed —
	// "not audited this pass", not zero.
	ObservedChainRaw *rawInt64 `json:"observed_chain_raw"`
	DivergenceRaw    *rawInt64 `json:"divergence_raw"`
}

type exposureJSON struct {
	RestoredCount int64    `json:"restored_count"`
	RestoredRaw   rawInt64 `json:"restored_raw"`
}

func reconciliationResponseFrom(rep billing.ReconciliationReport, generatedAt time.Time, chainAudited bool) reconciliationResponse {
	resp := reconciliationResponse{
		AccountID:   rep.AccountID,
		GeneratedAt: generatedAt,
		Ledger: ledgerIntegrityJSON{
			CreditRaw:        rawInt64(rep.Ledger.CreditRaw),
			ExpectedCredit:   rawInt64(rep.Ledger.ExpectedCredit),
			DriftCredit:      rawInt64(rep.Ledger.DriftCredit),
			ReservedRaw:      rawInt64(rep.Ledger.ReservedRaw),
			ExpectedReserved: rawInt64(rep.Ledger.ExpectedReserved),
			DriftReserved:    rawInt64(rep.Ledger.DriftReserved),
			LedgerRows:       rep.Ledger.LedgerRows,
			OpenReservations: rep.Ledger.OpenReservations,
			InvariantHeld:    rep.Ledger.InvariantHeld,
		},
		Merkle: merkleJSON{Root: hex.EncodeToString(rep.Merkle.Root), LeafCount: rep.Merkle.LeafCount},
		Exposure: exposureJSON{
			RestoredCount: rep.Exposure.RestoredCount,
			RestoredRaw:   rawInt64(rep.Exposure.RestoredRaw),
		},
		ChainAudited: chainAudited,
	}
	for _, a := range rep.Allowances {
		row := allowanceCheckJSON{
			Delegate:             a.Delegate,
			Active:               a.Active,
			RegistryAllowanceRaw: rawInt64(a.RegistryAllowanceRaw),
			InFlightRaw:          rawInt64(a.InFlightRaw),
			ExpectedChainRaw:     rawInt64(a.ExpectedChainRaw),
		}
		if a.ObservedChainRaw != nil {
			obs := rawInt64(*a.ObservedChainRaw)
			row.ObservedChainRaw = &obs
		}
		if a.DivergenceRaw != nil {
			div := rawInt64(*a.DivergenceRaw)
			row.DivergenceRaw = &div
		}
		resp.Allowances = append(resp.Allowances, row)
	}
	return resp
}

// handleReconciliation serves GET /manage/billing/reconciliation. The
// ledger-only portion (integrity + Merkle root) always comes from the
// store; the chain cross-check and exposure require the reconciler (wired
// when a settlement authority is configured).
func (s *Service) handleReconciliation(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	report := billing.ReconciliationReport{AccountID: accountID}
	ledger, err := s.store.ReconcileAccount(ctx, accountID, time.Time{})
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	report.Ledger = ledger
	merkle, err := s.store.AccountMerkleSummary(ctx, accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	report.Merkle = merkle
	if s.reconciler != nil {
		full, err := s.reconciler.Report(ctx, accountID)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		report = full
	}
	httputil.WriteJSON(w, http.StatusOK, reconciliationResponseFrom(report, s.now().UTC(), report.Allowances != nil))
}

type merkleProofResponse struct {
	AccountID string   `json:"account_id"`
	Root      string   `json:"root"`
	LeafCount int      `json:"leaf_count"`
	Index     int      `json:"index"`
	Leaf      string   `json:"leaf"`
	Path      []string `json:"path"`
}

// handleMerkleProof serves GET /manage/billing/merkle-proof?leaf_id=N — the
// inclusion proof for one ledger row: the leaf hash, the audit path (all
// hex), the root, the leaf count, and the row's 0-based index. The client
// verifies per billing.VerifyMerklePath's documented rules.
func (s *Service) handleMerkleProof(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	leafID, err := strconv.ParseInt(r.URL.Query().Get("leaf_id"), 10, 64)
	if err != nil || leafID <= 0 {
		http.Error(w, "leaf_id must be a positive ledger row id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	leaf, path, root, leafCount, err := s.store.MerkleProofForLedgerRow(ctx, accountID, leafID)
	if err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			http.Error(w, "no such ledger row", http.StatusNotFound)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	index, _, err := s.store.LedgerLeafIndex(ctx, accountID, leafID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	resp := merkleProofResponse{
		AccountID: accountID,
		Root:      hex.EncodeToString(root),
		LeafCount: leafCount,
		Index:     index,
		Leaf:      hex.EncodeToString(leaf),
		Path:      make([]string, 0, len(path)),
	}
	for _, p := range path {
		resp.Path = append(resp.Path, hex.EncodeToString(p))
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}
