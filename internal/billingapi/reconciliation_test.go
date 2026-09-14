package billingapi

// §11.5.5 endpoint tests: the reconciliation report (with and without the
// chain-auditing reconciler) and the Merkle inclusion proof endpoint.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/billing"
)

func TestReconciliationEndpointFull(t *testing.T) {
	root := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	obs := int64(45_000)
	div := int64(0)
	st := &stubStore{
		rec: billing.Reconciliation{
			AccountID: "acct-1", CreditRaw: 45_000, ExpectedCredit: 45_000,
			ReservedRaw: 0, ExpectedReserved: 0, LedgerRows: 7, InvariantHeld: true,
		},
		merkle: billing.MerkleSummary{Root: root, LeafCount: 7},
	}
	rec := &stubReconciler{report: billing.ReconciliationReport{
		AccountID: "acct-1",
		Ledger: billing.Reconciliation{
			AccountID: "acct-1", CreditRaw: 45_000, ExpectedCredit: 45_000,
			LedgerRows: 7, InvariantHeld: true,
		},
		Merkle: billing.MerkleSummary{Root: root, LeafCount: 7},
		Allowances: []billing.AllowanceCrossCheck{{
			Delegate:             "delegate-1",
			Active:               true,
			RegistryAllowanceRaw: 50_000,
			InFlightRaw:          5_000,
			ExpectedChainRaw:     45_000,
			ObservedChainRaw:     &obs,
			DivergenceRaw:        &div,
		}},
		Exposure: billing.ExposureSummary{RestoredCount: 1, RestoredRaw: 4_000},
	}}
	svc := New(st, "enforce", "treasury-ata", "treasury-wallet", "mint", "tp", 6, false, nil)
	svc.SetReconciler(rec)
	h := authed(t, svc, "acct-1")

	req := httptest.NewRequest("GET", "/manage/billing/reconciliation", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, `"root":"`+hex.EncodeToString(root)+`"`) {
		t.Fatalf("root missing/wrong: %s", body)
	}
	if !contains(body, `"leaf_count":7`) || !contains(body, `"invariant_held":true`) {
		t.Fatalf("ledger/merkle fields missing: %s", body)
	}
	if !contains(body, `"registry_allowance_raw":"50000"`) ||
		!contains(body, `"expected_chain_raw":"45000"`) ||
		!contains(body, `"observed_chain_raw":"45000"`) ||
		!contains(body, `"divergence_raw":"0"`) {
		t.Fatalf("cross-check fields missing: %s", body)
	}
	if !contains(body, `"restored_count":1`) || !contains(body, `"restored_raw":"4000"`) {
		t.Fatalf("exposure fields missing: %s", body)
	}
	if !contains(body, `"chain_audited":true`) {
		t.Fatalf("chain_audited missing: %s", body)
	}
}

func TestReconciliationEndpointLedgerOnly(t *testing.T) {
	// No reconciler wired (no settlement authority configured): the ledger
	// audit still serves, allowances empty, chain_audited false.
	st := &stubStore{
		rec:    billing.Reconciliation{AccountID: "acct-1", CreditRaw: 0, ExpectedCredit: 0, InvariantHeld: true},
		merkle: billing.MerkleSummary{Root: []byte(make([]byte, 32)), LeafCount: 0},
	}
	svc := New(st, "enforce", "treasury-ata", "treasury-wallet", "mint", "tp", 6, false, nil)
	h := authed(t, svc, "acct-1")

	req := httptest.NewRequest("GET", "/manage/billing/reconciliation", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, `"chain_audited":false`) || !contains(body, `"allowances":null`) {
		t.Fatalf("ledger-only shape wrong: %s", body)
	}
	if contains(body, `"restored_count":1`) {
		t.Fatal("no reconciler: exposure must be zero")
	}
}

func TestReconciliationEndpointStoreError(t *testing.T) {
	st := &stubStore{recErr: errors.New("db down")}
	svc := New(st, "enforce", "treasury-ata", "treasury-wallet", "mint", "tp", 6, false, nil)
	h := authed(t, svc, "acct-1")
	req := httptest.NewRequest("GET", "/manage/billing/reconciliation", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestMerkleProofEndpoint(t *testing.T) {
	leaf := []byte{0xAA, 0xBB}
	path := [][]byte{{0x01}, {0x02}}
	root := []byte{0xCC}
	st := &stubStore{
		proofLeaf: leaf, proofPath: path, proofRoot: root, proofCount: 3,
		indexOut: 2, indexOK: true,
	}
	svc := New(st, "enforce", "treasury-ata", "treasury-wallet", "mint", "tp", 6, false, nil)
	h := authed(t, svc, "acct-1")

	req := httptest.NewRequest("GET", "/manage/billing/merkle-proof?leaf_id=42", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, `"leaf":"aabb"`) || !contains(body, `"root":"cc"`) ||
		!contains(body, `"leaf_count":3`) || !contains(body, `"index":2`) ||
		!contains(body, `"path":["01","02"]`) {
		t.Fatalf("proof payload wrong: %s", body)
	}

	// Missing leaf_id → 400.
	req = httptest.NewRequest("GET", "/manage/billing/merkle-proof", nil)
	req.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing leaf_id: status = %d", w.Code)
	}

	// Unknown row → 404.
	st.proofErr = billing.ErrNotFound
	req = httptest.NewRequest("GET", "/manage/billing/merkle-proof?leaf_id=999", nil)
	req.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown row: status = %d", w.Code)
	}
}

// The endpoint's proof must satisfy billing.VerifyMerklePath end to end:
// stubs return a real 2-leaf proof; index 0 verifies against the real root.
func TestMerkleProofVerifiesWithBillingRules(t *testing.T) {
	leaves := [][]byte{
		{1, 2, 3, 4}, {5, 6, 7, 8},
	}
	path, err := billing.MerklePath(leaves, 1)
	if err != nil {
		t.Fatal(err)
	}
	root := billing.MerkleRoot(leaves)
	st := &stubStore{
		proofLeaf: leaves[1], proofPath: path, proofRoot: root, proofCount: 2,
		indexOut: 1, indexOK: true,
	}
	svc := New(st, "enforce", "treasury-ata", "treasury-wallet", "mint", "tp", 6, false, nil)
	h := authed(t, svc, "acct-1")
	req := httptest.NewRequest("GET", "/manage/billing/merkle-proof?leaf_id=7", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Decode the JSON payload and run the real verifier against it.
	resp := decodeProof(t, w.Body.String())
	if !billing.VerifyMerklePath(resp.leaf, resp.path, resp.index, resp.leafCount, resp.root) {
		t.Fatal("endpoint proof rejected by the documented verifier")
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// decodeProof parses the merkleProofResponse JSON into verifier inputs.
func decodeProof(t *testing.T, body string) (out struct {
	leaf      []byte
	path      [][]byte
	index     int
	leafCount int
	root      []byte
}) {
	t.Helper()
	var raw struct {
		Leaf      string   `json:"leaf"`
		Path      []string `json:"path"`
		Root      string   `json:"root"`
		LeafCount int      `json:"leaf_count"`
		Index     int      `json:"index"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var err error
	out.leaf, err = hex.DecodeString(raw.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	out.root, err = hex.DecodeString(raw.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range raw.Path {
		b, err := hex.DecodeString(p)
		if err != nil {
			t.Fatal(err)
		}
		out.path = append(out.path, b)
	}
	out.index, out.leafCount = raw.Index, raw.LeafCount
	return out
}
