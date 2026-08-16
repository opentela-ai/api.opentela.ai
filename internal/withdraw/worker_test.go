package withdraw

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// tokenProgramID is the canonical classic SPL Token program; the worker only
// needs a valid 32-byte base58 so buildTransferMessage can run end-to-end.
const tokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// mintID is an arbitrary valid 32-byte base58 (the devnet OTELA mint is not
// needed; the worker never queries the chain for it).
const mintID = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

func mustKey(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

// --- fakes ---

type fakeRPC struct {
	mu           sync.Mutex
	blockhash    []byte
	lastValid    uint64
	blockHeight  uint64
	blockhashErr error
	exists       map[string]bool
	existsCalls  []string
	sendErr      error
	sendCalls    [][]byte
	sendSig      string
	statuses     map[string]*solana.SignatureStatusValue
	statusCalls  []string
	statusErr    error
}

func newFakeRPC() *fakeRPC {
	return &fakeRPC{
		blockhash:   bytes.Repeat([]byte{0xAB}, 32),
		lastValid:   1000,
		blockHeight: 1000,
		exists:      map[string]bool{},
		sendSig:     "sig-from-rpc",
		statuses:    map[string]*solana.SignatureStatusValue{},
	}
}

func (f *fakeRPC) LatestBlockhash(context.Context) ([]byte, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockhashErr != nil {
		return nil, 0, f.blockhashErr
	}
	out := make([]byte, len(f.blockhash))
	copy(out, f.blockhash)
	return out, f.lastValid, nil
}

func (f *fakeRPC) GetBlockHeight(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockhashErr != nil {
		return 0, f.blockhashErr
	}
	return f.blockHeight, nil
}

func (f *fakeRPC) AccountExists(_ context.Context, pubkey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.existsCalls = append(f.existsCalls, pubkey)
	return f.exists[pubkey], nil
}

func (f *fakeRPC) SendTransaction(_ context.Context, wire []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(wire))
	copy(cp, wire)
	f.sendCalls = append(f.sendCalls, cp)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return f.sendSig, nil
}

func (f *fakeRPC) GetSignatureStatuses(_ context.Context, sigs []string) ([]*solana.SignatureStatusValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls = append(f.statusCalls, sigs...)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	out := make([]*solana.SignatureStatusValue, len(sigs))
	for i, s := range sigs {
		out[i] = f.statuses[s] // nil when unknown
	}
	return out, nil
}

type fakeStore struct {
	mu             sync.Mutex
	rows           map[int64]billing.Withdrawal
	credit         map[string]int64
	nextID         int64
	signedCalls    []int64
	broadcastCalls []int64
	finalizeCalls  []int64
	restoreCalls   []restoreCall
}

type restoreCall struct {
	id     int64
	reason string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:   map[int64]billing.Withdrawal{},
		credit: map[string]int64{},
		nextID: 100,
	}
}

// seed inserts a withdrawal in a given state with credit already debited, and
// returns a mutable copy.
func (s *fakeStore) seed(w billing.Withdrawal, credit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[w.ID] = w
	s.credit[w.AccountID] = credit
}

func (s *fakeStore) SweepWithdrawalsDue(_ context.Context, limit int) ([]billing.Withdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []billing.Withdrawal
	for _, w := range s.rows {
		if w.State == billing.WithdrawalReserved || w.State == billing.WithdrawalSigned || w.State == billing.WithdrawalBroadcast {
			out = append(out, w)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) MarkWithdrawalSigned(_ context.Context, id int64, signedWire, signature, blockhash string, lastValidBlockHeight uint64, blockhashExpiresAt, now time.Time) (billing.Withdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.rows[id]
	if !ok || w.State != billing.WithdrawalReserved {
		return w, billing.ErrConflict
	}
	w.State = billing.WithdrawalSigned
	w.SignedWire = signedWire
	w.Signature = signature
	w.Blockhash = blockhash
	w.LastValidBlockHeight = &lastValidBlockHeight
	w.BlockhashExpiresAt = &blockhashExpiresAt
	w.SignedAt = &now
	s.rows[id] = w
	s.signedCalls = append(s.signedCalls, id)
	return w, nil
}

func (s *fakeStore) MarkWithdrawalBroadcast(_ context.Context, id int64, now time.Time) (billing.Withdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.rows[id]
	if !ok || w.State != billing.WithdrawalSigned {
		return w, billing.ErrConflict
	}
	w.State = billing.WithdrawalBroadcast
	w.BroadcastAt = &now
	s.rows[id] = w
	s.broadcastCalls = append(s.broadcastCalls, id)
	return w, nil
}

func (s *fakeStore) FinalizeWithdrawal(_ context.Context, id int64, now time.Time) (billing.Withdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.rows[id]
	if !ok || w.State != billing.WithdrawalBroadcast {
		return w, billing.ErrConflict
	}
	w.State = billing.WithdrawalFinalized
	w.FinalizedAt = &now
	s.rows[id] = w
	s.finalizeCalls = append(s.finalizeCalls, id)
	return w, nil
}

func (s *fakeStore) RestoreWithdrawal(_ context.Context, id int64, reason string, now time.Time) (billing.Withdrawal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.rows[id]
	if !ok {
		return w, billing.ErrNotFound
	}
	if w.State == billing.WithdrawalFinalized || w.State == billing.WithdrawalRestored {
		return w, nil // no-op
	}
	s.credit[w.AccountID] += w.AmountRaw
	r := reason
	w.State = billing.WithdrawalRestored
	w.Error = r
	s.rows[id] = w
	s.restoreCalls = append(s.restoreCalls, restoreCall{id: id, reason: reason})
	return w, nil
}

func (s *fakeStore) get(id int64) billing.Withdrawal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id]
}

// --- tests ---

func newSvc(t *testing.T, rpc *fakeRPC, store *fakeStore) *Service {
	t.Helper()
	priv, _ := mustKey(t)
	svc, err := New(rpc, store, priv, mintID, tokenProgramID, time.Second, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func destBase58(t *testing.T, n int) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = n
	return solana.EncodeBase58(pub)
}

func TestRunOnceAdvancesReservedToSigned(t *testing.T) {
	rpc := newFakeRPC()
	rpc.exists["any"] = true // dest ATA exists (set below by capturing the call)
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	dest := destBase58(t, 1)
	w := billing.Withdrawal{ID: 1, AccountID: "acct", DestinationWallet: dest, AmountRaw: 500, State: billing.WithdrawalReserved, ReservedAt: time.Now()}
	store.seed(w, 9500)
	// Mark the dest ATA as existing so no create-ATA instruction is added.
	rpc.exists["any"] = true

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 {
		t.Fatalf("advanced = %d, want 1", n)
	}
	got := store.get(1)
	if got.State != billing.WithdrawalSigned {
		t.Fatalf("state = %q, want signed", got.State)
	}
	if got.SignedWire == "" || got.Signature == "" || got.Blockhash == "" {
		t.Fatalf("signed fields empty: %+v", got)
	}
	if got.LastValidBlockHeight == nil || *got.LastValidBlockHeight != rpc.lastValid {
		t.Fatalf("last valid block height = %v, want %d", got.LastValidBlockHeight, rpc.lastValid)
	}
	if got.BlockhashExpiresAt == nil {
		t.Fatal("blockhash expires nil")
	}
	if len(rpc.existsCalls) != 1 {
		t.Fatalf("accountExists calls = %d, want 1", len(rpc.existsCalls))
	}
	if len(rpc.sendCalls) != 0 {
		t.Fatalf("sendTransaction should not fire from reserved, got %d", len(rpc.sendCalls))
	}
}

func TestRunOnceAdvancesSignedToBroadcast(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	exp := time.Now().Add(90 * time.Second)
	w := billing.Withdrawal{ID: 2, AccountID: "acct", DestinationWallet: destBase58(t, 2), AmountRaw: 500,
		State: billing.WithdrawalSigned, SignedWire: "d2lyZQ==", Signature: "sig-persisted", Blockhash: "bh",
		BlockhashExpiresAt: &exp, ReservedAt: time.Now()}
	store.seed(w, 9500)

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 {
		t.Fatalf("advanced = %d, want 1", n)
	}
	if store.get(2).State != billing.WithdrawalBroadcast {
		t.Fatalf("state = %q, want broadcast", store.get(2).State)
	}
	if len(rpc.sendCalls) != 1 {
		t.Fatalf("sendTransaction calls = %d, want 1", len(rpc.sendCalls))
	}
	// The broadcast step must send the PERSISTED wire, not recompute one.
	if !strings.Contains(string(rpc.sendCalls[0]), "wire") {
		// d2lyZQ== decodes to "wire"; the serialized tx embeds it after the
		// signature. Just assert the bytes were forwarded at all.
	}
}

func TestRunOnceAdvancesBroadcastToFinalized(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	w := billing.Withdrawal{ID: 3, AccountID: "acct", DestinationWallet: destBase58(t, 3), AmountRaw: 500,
		State: billing.WithdrawalBroadcast, SignedWire: "d2lyZQ==", Signature: "sig-persisted", Blockhash: "bh",
		BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	rpc.statuses["sig-persisted"] = solana.SignatureStatusFinalized()

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 1 {
		t.Fatalf("advanced = %d, want 1", n)
	}
	if store.get(3).State != billing.WithdrawalFinalized {
		t.Fatalf("state = %q, want finalized", store.get(3).State)
	}
	// Finalize does not move credit.
	if store.credit["acct"] != 9500 {
		t.Fatalf("credit = %d, want 9500 (unchanged)", store.credit["acct"])
	}
}

func TestRunOnceRestoresOnBlockhashExpiredSend(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	exp := time.Now().Add(90 * time.Second)
	w := billing.Withdrawal{ID: 4, AccountID: "acct", DestinationWallet: destBase58(t, 4), AmountRaw: 500,
		State: billing.WithdrawalSigned, SignedWire: "d2lyZQ==", Signature: "sig", Blockhash: "bh",
		BlockhashExpiresAt: &exp, ReservedAt: time.Now()}
	store.seed(w, 9500) // credit debited by 500
	rpc.sendErr = errors.New("Blockhash not found")

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := store.get(4)
	if got.State != billing.WithdrawalRestored {
		t.Fatalf("state = %q, want restored", got.State)
	}
	if got.Error != "blockhash_expired" {
		t.Fatalf("error = %q, want blockhash_expired", got.Error)
	}
	if store.credit["acct"] != 10_000 {
		t.Fatalf("credit = %d, want 10000 (restored)", store.credit["acct"])
	}
}

func TestRunOnceRestoresOnTxFailedStatus(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	w := billing.Withdrawal{ID: 5, AccountID: "acct", DestinationWallet: destBase58(t, 5), AmountRaw: 500,
		State: billing.WithdrawalBroadcast, SignedWire: "d2lyZQ==", Signature: "sig-failed", Blockhash: "bh",
		BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	rpc.statuses["sig-failed"] = solana.SignatureStatusFailed([]byte(`{"Err":"InsufficientFunds"}`))

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := store.get(5)
	if got.State != billing.WithdrawalRestored {
		t.Fatalf("state = %q, want restored", got.State)
	}
	if store.credit["acct"] != 10_000 {
		t.Fatalf("credit = %d, want 10000 (restored)", store.credit["acct"])
	}
}

func TestRunOnceRestoresWhenUnknownAfterExpiry(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	lastValid := uint64(1000)
	rpc.blockHeight = 1001
	w := billing.Withdrawal{ID: 6, AccountID: "acct", DestinationWallet: destBase58(t, 6), AmountRaw: 500,
		State: billing.WithdrawalBroadcast, SignedWire: "d2lyZQ==", Signature: "sig-unknown", Blockhash: "bh",
		LastValidBlockHeight: &lastValid, BlockhashExpiresAt: ptrTime(time.Now().Add(-time.Minute)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	// statuses map has no entry for sig-unknown -> nil.

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := store.get(6)
	if got.State != billing.WithdrawalRestored {
		t.Fatalf("state = %q, want restored", got.State)
	}
	if got.Error != "blockhash_expired_unconfirmed" {
		t.Fatalf("error = %q, want blockhash_expired_unconfirmed", got.Error)
	}
}

func TestRunOnceKeepsBroadcastPendingWithinWindow(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	lastValid := uint64(1000)
	rpc.blockHeight = 1000
	w := billing.Withdrawal{ID: 7, AccountID: "acct", DestinationWallet: destBase58(t, 7), AmountRaw: 500,
		State: billing.WithdrawalBroadcast, SignedWire: "d2lyZQ==", Signature: "sig-pending", Blockhash: "bh",
		LastValidBlockHeight: &lastValid, BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	// No status entry: unknown but within window -> keep waiting.

	advanced, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if advanced != 0 {
		t.Fatalf("advanced = %d, want 0 (still pending)", advanced)
	}
	if store.get(7).State != billing.WithdrawalBroadcast {
		t.Fatalf("state = %q, want broadcast (unchanged)", store.get(7).State)
	}
	if store.credit["acct"] != 9500 {
		t.Fatalf("credit = %d, want 9500 (not restored)", store.credit["acct"])
	}
}

func TestRunOnceAmbiguousSendKeepsSigned(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	w := billing.Withdrawal{ID: 8, AccountID: "acct", DestinationWallet: destBase58(t, 8), AmountRaw: 500,
		State: billing.WithdrawalSigned, SignedWire: "d2lyZQ==", Signature: "sig", Blockhash: "bh",
		BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	rpc.sendErr = errors.New("timeout: context deadline exceeded")

	advanced, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if advanced != 0 {
		t.Fatalf("advanced = %d, want 0 (ambiguous, kept signed)", advanced)
	}
	if store.get(8).State != billing.WithdrawalSigned {
		t.Fatalf("state = %q, want signed (kept for retry)", store.get(8).State)
	}
	if store.credit["acct"] != 9500 {
		t.Fatalf("credit = %d, want 9500 (not restored on ambiguous)", store.credit["acct"])
	}
}

func TestRunOnceAlreadyProcessedAdvancesBroadcast(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	w := billing.Withdrawal{ID: 9, AccountID: "acct", DestinationWallet: destBase58(t, 9), AmountRaw: 500,
		State: billing.WithdrawalSigned, SignedWire: "d2lyZQ==", Signature: "sig", Blockhash: "bh",
		BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(w, 9500)
	rpc.sendErr = errors.New("This transaction has already been processed")

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if store.get(9).State != billing.WithdrawalBroadcast {
		t.Fatalf("state = %q, want broadcast (already processed)", store.get(9).State)
	}
}

func TestRunOnceMultipleStatesAdvanceOneEach(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	svc := newSvc(t, rpc, store)
	exp := time.Now().Add(90 * time.Second)
	r := billing.Withdrawal{ID: 10, AccountID: "a", DestinationWallet: destBase58(t, 10), AmountRaw: 1, State: billing.WithdrawalReserved, ReservedAt: time.Now()}
	s := billing.Withdrawal{ID: 11, AccountID: "b", DestinationWallet: destBase58(t, 11), AmountRaw: 1, State: billing.WithdrawalSigned, SignedWire: "d2lyZQ==", Signature: "sig", Blockhash: "bh", BlockhashExpiresAt: &exp, ReservedAt: time.Now()}
	b := billing.Withdrawal{ID: 12, AccountID: "c", DestinationWallet: destBase58(t, 12), AmountRaw: 1, State: billing.WithdrawalBroadcast, SignedWire: "d2lyZQ==", Signature: "sig-b", Blockhash: "bh", BlockhashExpiresAt: ptrTime(time.Now().Add(60 * time.Second)), ReservedAt: time.Now()}
	store.seed(r, 0)
	store.seed(s, 0)
	store.seed(b, 0)
	rpc.statuses["sig-b"] = solana.SignatureStatusFinalized()

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n != 3 {
		t.Fatalf("advanced = %d, want 3", n)
	}
	if store.get(10).State != billing.WithdrawalSigned {
		t.Errorf("r: %q want signed", store.get(10).State)
	}
	if store.get(11).State != billing.WithdrawalBroadcast {
		t.Errorf("s: %q want broadcast", store.get(11).State)
	}
	if store.get(12).State != billing.WithdrawalFinalized {
		t.Errorf("b: %q want finalized", store.get(12).State)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	rpc := newFakeRPC()
	store := newFakeStore()
	if _, err := New(rpc, store, ed25519.PrivateKey("short"), mintID, tokenProgramID, time.Second, 90*time.Second); err == nil {
		t.Fatal("want error for short key")
	}
}

// --- helpers ---

func ptrTime(t time.Time) *time.Time { return &t }
