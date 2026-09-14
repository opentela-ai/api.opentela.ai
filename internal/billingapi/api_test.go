package billingapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/account"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

// --- stubs ---

type stubStore struct {
	ensureErr     error
	credit        billing.AccountCredit
	creditErr     error
	primaryWallet string
	primaryOK     bool
	primaryErr    error
	setCapsIn     billing.Caps
	setCapsOut    billing.AccountCredit
	setCapsErr    error
	ledgerIn      *billing.LedgerCursor
	ledgerOut     []billing.LedgerEntry
	ledgerNext    *billing.LedgerCursor
	ledgerErr     error
	depositIn     *billing.DepositCursor
	depositOut    []billing.DepositEvent
	depositNext   *billing.DepositCursor
	depositErr    error
	reserveIn     billing.WithdrawalRequest
	reserveOut    billing.Withdrawal
	reserveErr    error
	withdrawalID  int64
	withdrawalOut billing.Withdrawal
	withdrawalErr error
	listWCursor   *billing.WithdrawalCursor
	listWOut      []billing.Withdrawal
	listWNext     *billing.WithdrawalCursor
	listWErr      error
	// Seller asks surface.
	instances    []store.InstanceInfo
	instancesErr error
	asksIn       []string
	asksOut      []billing.Ask
	// §11.5.5 reconciliation surface.
	rec        billing.Reconciliation
	recErr     error
	merkle     billing.MerkleSummary
	merkleErr  error
	proofLeaf  []byte
	proofPath  [][]byte
	proofRoot  []byte
	proofCount int
	proofErr   error
	indexOut   int
	indexOK    bool
	indexErr   error
	asksErr    error
	// Console-managed pricing surface (model caps + ask config).
	capsSheet    []billing.ModelCaps
	capsSheetErr error
	savedSheet   []billing.ModelCaps
	capsIn       string // captured accountID
	askCfg       map[string][]billing.Ask
	cfgErr       error
	savedPeer    string
	savedAsks    []billing.Ask
	instByPeer   map[string]store.InstanceInfo
	instErr      error
	published    map[string][]billing.Ask // ReplaceAsks captures
	publishTTL   time.Duration
	publishErr   error
}

func (s *stubStore) ListInstancesByUser(_ context.Context, accountID string) ([]store.InstanceInfo, error) {
	if s.instancesErr != nil {
		return nil, s.instancesErr
	}
	// Only instances owned by the requesting account are visible.
	var out []store.InstanceInfo
	for _, in := range s.instances {
		if in.AccountID == accountID {
			out = append(out, in)
		}
	}
	return out, nil
}

func (s *stubStore) LiveAsksByPeers(_ context.Context, peerIDs []string, _ time.Time) ([]billing.Ask, error) {
	if s.asksErr != nil {
		return nil, s.asksErr
	}
	s.asksIn = peerIDs
	return s.asksOut, nil
}

// stubMesh is the SellerMesh test double.
type stubMesh struct {
	peers map[string]mesh.PeerObservation
	err   error
}

func (m *stubMesh) LookupPeer(_ context.Context, peerID string) (mesh.PeerObservation, error) {
	if m.err != nil {
		return mesh.PeerObservation{}, m.err
	}
	if obs, ok := m.peers[peerID]; ok {
		return obs, nil
	}
	return mesh.PeerObservation{}, errors.New("not found")
}

func (s *stubStore) EnsureAccountCredit(_ context.Context, _ string) error {
	return s.ensureErr
}
func (s *stubStore) AccountCredit(_ context.Context, _ string) (billing.AccountCredit, error) {
	return s.credit, s.creditErr
}
func (s *stubStore) PrimaryWalletForAccount(_ context.Context, _ string) (string, bool, error) {
	return s.primaryWallet, s.primaryOK, s.primaryErr
}
func (s *stubStore) AccountAllowances(_ context.Context, _ string) ([]billing.Allowance, error) {
	return nil, nil
}
func (s *stubStore) SetAccountCaps(_ context.Context, _ string, caps billing.Caps) (billing.AccountCredit, error) {
	s.setCapsIn = caps
	return s.setCapsOut, s.setCapsErr
}
func (s *stubStore) ListLedger(_ context.Context, _ string, cursor *billing.LedgerCursor, _ int) ([]billing.LedgerEntry, *billing.LedgerCursor, error) {
	s.ledgerIn = cursor
	return s.ledgerOut, s.ledgerNext, s.ledgerErr
}
func (s *stubStore) ListDepositEvents(_ context.Context, _ string, cursor *billing.DepositCursor, _ int) ([]billing.DepositEvent, *billing.DepositCursor, error) {
	s.depositIn = cursor
	return s.depositOut, s.depositNext, s.depositErr
}
func (s *stubStore) ReserveWithdrawal(_ context.Context, req billing.WithdrawalRequest, _ time.Time) (billing.Withdrawal, error) {
	s.reserveIn = req
	return s.reserveOut, s.reserveErr
}
func (s *stubStore) Withdrawal(_ context.Context, _ string, id int64) (billing.Withdrawal, error) {
	s.withdrawalID = id
	return s.withdrawalOut, s.withdrawalErr
}
func (s *stubStore) ListWithdrawals(_ context.Context, _ string, cursor *billing.WithdrawalCursor, _ int) ([]billing.Withdrawal, *billing.WithdrawalCursor, error) {
	s.listWCursor = cursor
	return s.listWOut, s.listWNext, s.listWErr
}

// §11.5.5 reconciliation surface (canned).
func (s *stubStore) ReconcileAccount(context.Context, string, time.Time) (billing.Reconciliation, error) {
	return s.rec, s.recErr
}
func (s *stubStore) AccountMerkleSummary(context.Context, string) (billing.MerkleSummary, error) {
	return s.merkle, s.merkleErr
}
func (s *stubStore) MerkleProofForLedgerRow(context.Context, string, int64) ([]byte, [][]byte, []byte, int, error) {
	if s.proofErr != nil {
		return nil, nil, nil, 0, s.proofErr
	}
	return s.proofLeaf, s.proofPath, s.proofRoot, s.proofCount, nil
}
func (s *stubStore) LedgerLeafIndex(context.Context, string, int64) (int, bool, error) {
	return s.indexOut, s.indexOK, s.indexErr
}

// §11.5.5 reconciliation surface (canned).
type stubReconciler struct {
	report billing.ReconciliationReport
	err    error
}

func (r *stubReconciler) Report(context.Context, string) (billing.ReconciliationReport, error) {
	if r.err != nil {
		return billing.ReconciliationReport{}, r.err
	}
	return r.report, nil
}

type verifierStub struct{ sub string }

func (v verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: v.sub, Email: "alice@example.com", EmailVerified: true}, nil
}

// authed routes the Service through the same principal.Middleware the manage
// router applies, with a fixed sub.
func authed(t *testing.T, svc *Service, sub string) http.Handler {
	t.Helper()
	return principal.Middleware(verifierStub{sub: sub}, nil, func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	})(svc.Routes())
}

// authedWithAccount layers account.ID on the request context WITHOUT a JWT
// subject, simulating an API-key request resolved by auth.Middleware. The
// billing Service falls back to account.ID when principal.UserID is absent.
func authedWithAccount(t *testing.T, svc *Service, accountID string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(account.WithID(r.Context(), accountID))
		svc.Routes().ServeHTTP(w, r)
	})
}

func ptr64(v int64) *int64 { return &v }

// --- tests ---

func TestGetStateReturnsBalanceAndDepositInstructions(t *testing.T) {
	primary := validDestWallet(t)
	st := &stubStore{
		credit: billing.AccountCredit{
			AccountID: "acct-1", CreditRaw: 5_000_000, ReservedRaw: 1_000_000,
			MaxInputPerMillion: ptr64(2000), MaxOutputPerMillion: ptr64(6000),
			UpdatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		},
		primaryWallet: primary,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "TreasuryATA", "Mint", "TokenProg", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Balance.CreditRaw != 5_000_000 || got.Balance.ReservedRaw != 1_000_000 || got.Balance.AvailableRaw != 4_000_000 {
		t.Fatalf("balance = %+v", got.Balance)
	}
	if got.Mode != "enforce" {
		t.Fatalf("mode = %q, want enforce", got.Mode)
	}
	if !got.WithdrawalsEnabled {
		t.Fatalf("withdrawals_enabled = false, want true")
	}
	if got.PrimaryLinkedWallet == nil || *got.PrimaryLinkedWallet != primary {
		t.Fatalf("primary_linked_wallet = %v, want %q", got.PrimaryLinkedWallet, primary)
	}
	if !got.Deposits.Enabled || got.Deposits.TreasuryATA != "TreasuryATA" || got.Deposits.Mint != "Mint" || got.Deposits.Decimals != 9 {
		t.Fatalf("deposits = %+v", got.Deposits)
	}
	// MaxCachedInputPerMillion was nil → must render as JSON null.
	if v, ok := got.Caps.InputPerMillion.Int(); !ok || v != 2000 {
		t.Fatalf("input cap = %v, want 2000", got.Caps.InputPerMillion)
	}
	if _, ok := got.Caps.CachedInputPerMillion.Int(); ok {
		t.Fatalf("cached cap = %v, want null(-1)", got.Caps.CachedInputPerMillion)
	}
}

func TestGetStateEncodesRawBalancesAsDecimalStrings(t *testing.T) {
	const raw = int64(9_007_199_254_740_993)
	st := &stubStore{credit: billing.AccountCredit{AccountID: "acct-1", CreditRaw: raw}}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, false, nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Balance map[string]json.RawMessage `json:"balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := string(body.Balance["credit_raw"]); got != `"9007199254740993"` {
		t.Fatalf("credit_raw JSON = %s, want exact decimal string", got)
	}
}

func TestGetStateDepositsDisabledWhenOff(t *testing.T) {
	st := &stubStore{credit: billing.AccountCredit{AccountID: "acct-1"}}
	svc := New(st, config.BillingOff, "TreasuryATA", "Mint", "TokenProg", 9, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	var got stateResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Deposits.Enabled {
		t.Fatalf("deposits must be disabled in off mode, got %+v", got.Deposits)
	}
	if got.WithdrawalsEnabled {
		t.Fatalf("withdrawals_enabled must be false, got %+v", got)
	}
}

// TestGetStateOffModeIsZeroCost verifies that an off-mode GET /manage/billing
// (the management route is always mounted) returns a synthetic 200 payload
// WITHOUT touching the billing store. Sentinel errors on every store method
// prove the store is never consulted, so an off-mode deployment never writes
// an account_credit row just because a user opened the wallet page.
func TestGetStateOffModeIsZeroCost(t *testing.T) {
	st := &stubStore{
		ensureErr:  errors.New("EnsureAccountCredit must not be called in off mode"),
		creditErr:  errors.New("AccountCredit must not be called in off mode"),
		primaryErr: errors.New("PrimaryWalletForAccount must not be called in off mode"),
	}
	svc := New(st, config.BillingOff, "", "", "", 0, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (off-mode GET must not touch the store)", rec.Code, rec.Body.String())
	}
	var got stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != "off" {
		t.Fatalf("mode=%q want off", got.Mode)
	}
	if got.Balance.CreditRaw != 0 || got.Balance.ReservedRaw != 0 || got.Balance.AvailableRaw != 0 {
		t.Fatalf("balance must be zero in off mode, got %+v", got.Balance)
	}
	if got.Deposits.Enabled {
		t.Fatalf("deposits must be disabled in off mode")
	}
	if got.WithdrawalsEnabled {
		t.Fatalf("withdrawals must be disabled in off mode")
	}
	if got.PrimaryLinkedWallet != nil {
		t.Fatalf("primary_linked_wallet must be null in off mode, got %q", *got.PrimaryLinkedWallet)
	}
}

func TestGetStateRequiresAccount(t *testing.T) {
	st := &stubStore{}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	// No principal, no account id → 401.
	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	rec := httptest.NewRecorder()
	svc.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestGetStateUsesAccountIDFallbackForApiKey(t *testing.T) {
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "api-acct", CreditRaw: 7},
	}
	svc := New(st, config.BillingObserve, "ATA", "Mint", "Prog", 9, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer legacy")
	rec := httptest.NewRecorder()
	authedWithAccount(t, svc, "api-acct").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got stateResponse
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Balance.CreditRaw != 7 {
		t.Fatalf("credit = %d, want 7 (API-key fallback)", got.Balance.CreditRaw)
	}
}

func TestPatchPreferencesSetsCaps(t *testing.T) {
	st := &stubStore{
		setCapsOut: billing.AccountCredit{
			AccountID: "acct-1", MaxInputPerMillion: ptr64(1500), MaxCachedInputPerMillion: ptr64(400), MaxOutputPerMillion: ptr64(5000),
		},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"max_input_per_million":1500,"max_cached_input_per_million":400,"max_output_per_million":5000}`
	req := httptest.NewRequest(http.MethodPatch, "/manage/billing/preferences", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.setCapsIn.InputPerMillion == nil || *st.setCapsIn.InputPerMillion != 1500 {
		t.Fatalf("setCaps input = %+v", st.setCapsIn)
	}
	if st.setCapsIn.CachedInputPerMillion == nil || *st.setCapsIn.CachedInputPerMillion != 400 {
		t.Fatalf("setCaps cached = %+v", st.setCapsIn)
	}
	var got capsJSON
	json.Unmarshal(rec.Body.Bytes(), &got)
	if v, ok := got.InputPerMillion.Int(); !ok || v != 1500 {
		t.Fatalf("input cap=%v", v)
	}
	if v, ok := got.CachedInputPerMillion.Int(); !ok || v != 400 {
		t.Fatalf("cached cap=%v", v)
	}
	if v, ok := got.OutputPerMillion.Int(); !ok || v != 5000 {
		t.Fatalf("output cap=%v", v)
	}
}

func TestPatchPreferencesClearsDimensionWithNull(t *testing.T) {
	st := &stubStore{setCapsOut: billing.AccountCredit{AccountID: "acct-1"}}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	// null max_input → clears that dimension to unlimited.
	body := `{"max_input_per_million":null,"max_output_per_million":9000}`
	req := httptest.NewRequest(http.MethodPatch, "/manage/billing/preferences", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.setCapsIn.InputPerMillion != nil {
		t.Fatalf("input cap should be nil (unlimited), got %v", st.setCapsIn.InputPerMillion)
	}
	if st.setCapsIn.OutputPerMillion == nil || *st.setCapsIn.OutputPerMillion != 9000 {
		t.Fatalf("output cap = %+v", st.setCapsIn.OutputPerMillion)
	}
}

func TestPatchPreferencesRejectsNegative(t *testing.T) {
	st := &stubStore{}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"max_input_per_million":-1}`
	req := httptest.NewRequest(http.MethodPatch, "/manage/billing/preferences", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 for negative cap", rec.Code)
	}
	if st.setCapsIn.InputPerMillion != nil {
		t.Fatalf("store must not be called for invalid caps")
	}
}

func TestGetLedgerPaginatesWithOpaqueCursor(t *testing.T) {
	t0 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	next := billing.LedgerCursor{CreatedAt: t0, ID: 42}
	st := &stubStore{
		ledgerOut: []billing.LedgerEntry{
			{ID: 100, DeltaRaw: -5000, Source: "usage", Leg: "usage", Ref: "req-1", InputTokens: ptrInt(120), OutputTokens: ptrInt(80)},
		},
		ledgerNext: &next,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/ledger?limit=10", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page ledgerPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].ID != 100 {
		t.Fatalf("page = %+v", page)
	}
	if v, ok := page.Entries[0].InputTokens.Int(); !ok || v != 120 {
		t.Fatalf("page = %+v", page)
	}
	if page.Next == "" {
		t.Fatal("next cursor missing")
	}

	// Second request carries the opaque cursor; the store must receive the
	// decoded (created_at, id) back.
	st.ledgerIn = nil
	req2 := httptest.NewRequest(http.MethodGet, "/manage/billing/ledger?cursor="+page.Next, nil)
	req2.Header.Set("Authorization", "Bearer token")
	rec2 := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if st.ledgerIn == nil || st.ledgerIn.ID != 42 {
		t.Fatalf("cursor not round-tripped: %+v", st.ledgerIn)
	}
	if !st.ledgerIn.CreatedAt.Equal(t0) {
		t.Fatalf("cursor time = %v, want %v", st.ledgerIn.CreatedAt, t0)
	}
}

func TestGetLedgerRejectsMalformedCursor(t *testing.T) {
	st := &stubStore{}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/billing/ledger?cursor=!!!notbase64!!!", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 for malformed cursor", rec.Code)
	}
}

func TestGetDepositsPaginates(t *testing.T) {
	t0 := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	next := billing.DepositCursor{SeenAt: t0, TransactionSignature: "sig-9", InstructionIndex: 2}
	st := &stubStore{
		depositOut: []billing.DepositEvent{
			{TransactionSignature: "sig-1", InstructionIndex: 0, Slot: 10, FromWallet: "walletA", AmountRaw: 1_000_000, AssignmentState: "assigned", CreditedAt: &t0, SeenAt: t0},
		},
		depositNext: &next,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/deposits", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page depositPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].TransactionSignature != "sig-1" || page.Entries[0].AmountRaw != 1_000_000 {
		t.Fatalf("page = %+v", page)
	}
	if page.Next == "" {
		t.Fatal("next cursor missing")
	}

	// Round-trip the cursor.
	st.depositIn = nil
	req2 := httptest.NewRequest(http.MethodGet, "/manage/billing/deposits?cursor="+page.Next, nil)
	req2.Header.Set("Authorization", "Bearer token")
	rec2 := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if st.depositIn == nil || st.depositIn.TransactionSignature != "sig-9" || st.depositIn.InstructionIndex != 2 {
		t.Fatalf("deposit cursor not round-tripped: %+v", st.depositIn)
	}
	if !st.depositIn.SeenAt.Equal(t0) {
		t.Fatalf("deposit cursor time = %v, want %v", st.depositIn.SeenAt, t0)
	}
}

func ptrInt(v int) *int { return &v }

// TestCapsJSONKeyNames guards the wire contract: the state and PATCH responses
// must serialize caps under input_per_million / cached_input_per_million /
// output_per_million (a prior typo wrote output_output_per_million, which left
// the buyer's output ceiling null to every frontend caller).
func TestCapsJSONKeyNames(t *testing.T) {
	st := &stubStore{
		credit: billing.AccountCredit{
			AccountID: "acct-1", MaxInputPerMillion: ptr64(2000),
			MaxCachedInputPerMillion: ptr64(500), MaxOutputPerMillion: ptr64(6000),
		},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(raw["caps"], &caps); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"input_per_million", "cached_input_per_million", "output_per_million"} {
		if _, ok := caps[key]; !ok {
			t.Fatalf("caps missing JSON key %q (got %v); body=%s", key, keys(caps), rec.Body.String())
		}
	}
	if _, bad := caps["output_output_per_million"]; bad {
		t.Fatalf("caps still serialize under the doubled key output_output_per_million")
	}
	if string(caps["output_per_million"]) != "6000" {
		t.Fatalf("output_per_million=%s, want 6000", caps["output_per_million"])
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// validDestWallet builds a real 32-byte base58 pubkey for the create tests.
func validDestWallet(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return solana.EncodeBase58(pub)
}

func TestCreateWithdrawalReserves(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000, ReservedRaw: 0},
		primaryWallet: dest,
		primaryOK:     true,
		reserveOut:    billing.Withdrawal{ID: 77, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: 4_000, State: billing.WithdrawalReserved, ReservedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-1")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got withdrawalJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 77 || got.State != "reserved" || got.AmountRaw != 4000 || got.DestinationWallet != dest {
		t.Fatalf("withdrawal = %+v", got)
	}
	if st.reserveIn.AccountID != "acct-1" || st.reserveIn.IdempotencyKey != "idem-1" || st.reserveIn.AmountRaw != 4000 {
		t.Fatalf("reserveIn = %+v", st.reserveIn)
	}
	if st.reserveIn.DestinationWallet != dest {
		t.Fatalf("reserve destination = %q, want %q", st.reserveIn.DestinationWallet, dest)
	}
}

func TestCreateWithdrawalAcceptsLosslessDecimalString(t *testing.T) {
	const raw = int64(9_007_199_254_740_993)
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: raw},
		primaryWallet: dest,
		primaryOK:     true,
		reserveOut: billing.Withdrawal{
			ID: 79, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: raw,
			State: billing.WithdrawalReserved, ReservedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	body := `{"amount_raw":"9007199254740993"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-lossless")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.reserveIn.AmountRaw != raw {
		t.Fatalf("reserved amount = %d, want %d", st.reserveIn.AmountRaw, raw)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := string(response["amount_raw"]); got != `"9007199254740993"` {
		t.Fatalf("amount_raw JSON = %s, want exact decimal string", got)
	}
}

func TestCreateWithdrawalInsufficientCreditPreCheck(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 100, ReservedRaw: 0},
		primaryWallet: dest,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000,"idempotency_key":"idem-2"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d, want 402 (available credit pre-check)", rec.Code)
	}
	if st.reserveIn.IdempotencyKey != "" {
		t.Fatal("store should not be called when available credit is short")
	}
}

func TestCreateWithdrawalUsesBodyIdempotencyKeyCompatibility(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
		reserveOut:    billing.Withdrawal{ID: 78, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: 4_000, State: billing.WithdrawalReserved, ReservedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000,"idempotency_key":"idem-body"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.reserveIn.IdempotencyKey != "idem-body" {
		t.Fatalf("idempotency key = %q, want idem-body", st.reserveIn.IdempotencyKey)
	}
}

func TestCreateWithdrawalRejectsIdempotencyMismatch(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000,"idempotency_key":"idem-body"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-header")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 (mismatched idempotency key)", rec.Code)
	}
	if st.reserveIn.IdempotencyKey != "" {
		t.Fatal("store must not be called on idempotency mismatch")
	}
}

func TestCreateWithdrawalDisabled(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, false, nil)

	body := `{"amount_raw":4000}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-disabled")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503 (withdrawals disabled)", rec.Code)
	}
	if st.reserveIn.IdempotencyKey != "" {
		t.Fatal("store must not be called when withdrawals are disabled")
	}
}

func TestCreateWithdrawalRequiresPrimaryWallet(t *testing.T) {
	st := &stubStore{credit: billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000}}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(`{"amount_raw":4000}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-no-wallet")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (missing primary wallet)", rec.Code)
	}
}

func TestCreateWithdrawalRejectsDestinationMismatch(t *testing.T) {
	primary := validDestWallet(t)
	other := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: primary,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + other + `","amount_raw":4000}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-destination")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (destination mismatch)", rec.Code)
	}
}

func TestCreateWithdrawalRejectsInvalidPrimaryWallet(t *testing.T) {
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: "not-a-pubkey",
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(`{"amount_raw":4000}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "idem-invalid-primary")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (invalid primary wallet)", rec.Code)
	}
}

func TestCreateWithdrawalMissingIdempotencyKey(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 (missing idempotency key)", rec.Code)
	}
}

func TestCreateWithdrawalStoreConflict(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
		reserveErr:    billing.ErrConflict,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000,"idempotency_key":"idem-4"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (idempotency key in use)", rec.Code)
	}
}

func TestCreateWithdrawalStoreInsufficientCreditRace(t *testing.T) {
	dest := validDestWallet(t)
	// The pre-check passes (available 10000), but the store loses a
	// concurrent reservation race → ErrInsufficientCredit → 402.
	st := &stubStore{
		credit:        billing.AccountCredit{AccountID: "acct-1", CreditRaw: 10_000},
		primaryWallet: dest,
		primaryOK:     true,
		reserveErr:    billing.ErrInsufficientCredit,
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	body := `{"destination_wallet":"` + dest + `","amount_raw":4000,"idempotency_key":"idem-5"}`
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d, want 402 (store race)", rec.Code)
	}
}

func TestCreateWithdrawalRequiresAccount(t *testing.T) {
	svc := New(&stubStore{}, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	req := httptest.NewRequest(http.MethodPost, "/manage/billing/withdrawals", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	svc.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestGetWithdrawal(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{withdrawalOut: billing.Withdrawal{ID: 55, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: 1000, State: billing.WithdrawalBroadcast}}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/withdrawals/55", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got withdrawalJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 55 || got.State != "broadcast" {
		t.Fatalf("withdrawal = %+v", got)
	}
	// The signed wire is an internal implementation detail and must NOT be
	// exposed over the API (it carries a transaction the operator signed).
	var raw map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, leak := raw["signed_wire"]; leak {
		t.Fatal("response leaked signed_wire")
	}
	if st.withdrawalID != 55 {
		t.Fatalf("store id = %d, want 55", st.withdrawalID)
	}
}

func TestGetWithdrawalNotFound(t *testing.T) {
	st := &stubStore{withdrawalErr: billing.ErrNotFound}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/withdrawals/999", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestGetWithdrawalBadID(t *testing.T) {
	svc := New(&stubStore{}, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/billing/withdrawals/notanint", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestListWithdrawalsPaginates(t *testing.T) {
	dest := validDestWallet(t)
	st := &stubStore{
		listWOut: []billing.Withdrawal{
			{ID: 3, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: 100, State: billing.WithdrawalFinalized, ReservedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)},
			{ID: 2, AccountID: "acct-1", DestinationWallet: dest, AmountRaw: 100, State: billing.WithdrawalBroadcast, ReservedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)},
		},
		listWNext: &billing.WithdrawalCursor{ReservedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC), ID: 2},
	}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/withdrawals?limit=2", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got withdrawalPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].ID != 3 || got.Entries[1].ID != 2 {
		t.Fatalf("entries = %+v", got.Entries)
	}
	if got.Next == "" {
		t.Fatal("next cursor empty")
	}
	// The cursor round-trips: a second request with it must be decoded and
	// forwarded to the store unchanged.
	req2 := httptest.NewRequest(http.MethodGet, "/manage/billing/withdrawals?cursor="+got.Next, nil)
	req2.Header.Set("Authorization", "Bearer token")
	rec2 := httptest.NewRecorder()
	st.listWNext = nil
	authed(t, svc, "acct-1").ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second page code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if st.listWCursor == nil || st.listWCursor.ID != 2 {
		t.Fatalf("decoded cursor = %+v, want id=2", st.listWCursor)
	}
}

// --- GET /manage/billing/asks (seller pricing surface) ---

func TestGetAsksSellerPricingSurface(t *testing.T) {
	exp := time.Now().UTC().Add(3 * time.Minute)
	st := &stubStore{
		instances: []store.InstanceInfo{
			{AccountID: "acct-1", PeerID: "peer-live", Label: "gpu-box", OwnerWallet: "WalletA"},
			{AccountID: "acct-1", PeerID: "peer-dark", Label: "unlinked"},
			{AccountID: "acct-other", PeerID: "peer-foreign", Label: "not mine"},
		},
		asksOut: []billing.Ask{
			{PeerID: "peer-live", Service: "llm", Model: "model-A",
				InputPerMillion: 100, CachedInputPerMillion: 50, OutputPerMillion: 200,
				Revision: 7, ExpiresAt: exp, UpdatedAt: exp},
		},
	}
	m := &stubMesh{peers: map[string]mesh.PeerObservation{
		"peer-live": {
			PeerID: "peer-live", Wallet: "WalletA", Online: true,
			ObservedAt: time.Now().UTC(),
			Services: []mesh.ServiceObservation{
				{Name: "llm", IdentityGroups: []string{"model=model-A", "model=model-B"}},
			},
		},
	}}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, m)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/asks", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got asksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AskTTLSeconds != 300 || got.RepublishSeconds != 120 {
		t.Fatalf("ttl/republish = %d/%d, want 300/120", got.AskTTLSeconds, got.RepublishSeconds)
	}
	if got.PublicationEndpoint != "POST /internal/pricing" {
		t.Fatalf("publication endpoint = %q", got.PublicationEndpoint)
	}
	if len(got.Instances) != 2 {
		t.Fatalf("instances = %d, want 2 (foreign instance must be excluded)", len(got.Instances))
	}
	byPeer := map[string]instancePricing{}
	for _, ip := range got.Instances {
		byPeer[ip.PeerID] = ip
	}
	live := byPeer["peer-live"]
	if !live.Billable || !live.Online || !live.AdvertisementKnown {
		t.Fatalf("peer-live flags = billable=%v online=%v advKnown=%v, want true/true/true", live.Billable, live.Online, live.AdvertisementKnown)
	}
	if live.OwnerWallet != "WalletA" {
		t.Fatalf("owner wallet = %q, want WalletA", live.OwnerWallet)
	}
	if len(live.Asks) != 1 || live.Asks[0].Model != "model-A" || live.Asks[0].Revision != 7 {
		t.Fatalf("asks = %+v", live.Asks)
	}
	if len(live.UnpricedRoutes) != 1 || live.UnpricedRoutes[0].Service != "llm" || live.UnpricedRoutes[0].Model != "model-B" {
		t.Fatalf("unpriced routes = %+v, want [llm/model-B]", live.UnpricedRoutes)
	}
	dark := byPeer["peer-dark"]
	if dark.Billable {
		t.Fatal("instance without owner wallet must not be billable")
	}
	if dark.AdvertisementKnown || len(dark.UnpricedRoutes) != 0 {
		t.Fatalf("failed mesh lookup must mark advertisement unknown, got advKnown=%v unpriced=%v", dark.AdvertisementKnown, dark.UnpricedRoutes)
	}
	if dark.Online {
		t.Fatal("failed mesh lookup must not report online")
	}
	// The store must have been asked for exactly the account's peer IDs.
	if len(st.asksIn) != 2 {
		t.Fatalf("LiveAsksByPeers peerIDs = %v, want the 2 owned peers", st.asksIn)
	}
}

func TestGetAsksOffModeEmptyPayload(t *testing.T) {
	st := &stubStore{
		ensureErr: errors.New("EnsureAccountCredit must not be called in off mode"),
	}
	svc := New(st, config.BillingOff, "", "", "", 0, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/asks", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got asksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Instances) != 0 {
		t.Fatalf("off mode must return an empty instance list, got %d", len(got.Instances))
	}
}

func TestGetAsksStoreErrorIs503(t *testing.T) {
	st := &stubStore{instancesErr: errors.New("db down")}
	svc := New(st, config.BillingEnforce, "ATA", "Mint", "Prog", 9, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/manage/billing/asks", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", rec.Code)
	}
}
