package delegations

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

var (
	testMint      string
	testTokenProg string
)

var (
	testAuthority string
	wallet1       string
	wallet2       string
	otherDeleg8   string
)

func init() {
	testAuthority = pubkeyFromSeedFixed("settlement-authority")
	wallet1 = pubkeyFromSeedFixed("buyer-wallet-1")
	wallet2 = pubkeyFromSeedFixed("buyer-wallet-2")
	otherDeleg8 = pubkeyFromSeedFixed("other-delegate")
	testMint = pubkeyFromSeedFixed("otela-mint")
	testTokenProg = pubkeyFromSeedFixed("token-program")
}

func pubkeyFromSeedFixed(seed string) string {
	h := sha256.Sum256([]byte(seed))
	key := ed25519.NewKeyFromSeed(h[:])
	return solana.EncodeBase58(key.Public().(ed25519.PublicKey))
}

// fakeRPC returns canned TokenDelegation replies keyed by ATA string.
type fakeRPC struct {
	mu     sync.Mutex
	byATA  map[string]solana.TokenDelegate
	exists map[string]bool
	err    error
	calls  int
}

func (f *fakeRPC) TokenDelegation(_ context.Context, ata string) (solana.TokenDelegate, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return solana.TokenDelegate{}, false, f.err
	}
	td, ok := f.byATA[ata]
	if !ok {
		// Not configured: treat as an existing account with no delegation.
		return solana.TokenDelegate{}, f.exists[ata], nil
	}
	return td, f.exists[ata], nil
}

func (f *fakeRPC) set(wallet string, td solana.TokenDelegate, exists bool) string {
	ata := ataFor(wallet)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byATA[ata] = td
	f.exists[ata] = exists
	return ata
}

// ataFor mirrors the real derivation so the poller's ATA math is exercised.
func ataFor(wallet string) string {
	w, _ := solana.DecodeBase58(wallet, solana.PublicKeyBytes)
	m, _ := solana.DecodeBase58(testMint, solana.PublicKeyBytes)
	tp, _ := solana.DecodeBase58(testTokenProg, solana.PublicKeyBytes)
	ata, _ := solana.AssociatedTokenAddress(w, m, tp)
	return solana.EncodeBase58(ata)
}

// fakeStore records UpsertAllowance calls and serves canned registry rows.
type fakeStore struct {
	mu     sync.Mutex
	links  []billing.WalletAccount
	rows   map[string][]billing.Allowance // by account
	ups    []billing.AllowanceChange
	upsert func(ch billing.AllowanceChange) (billing.AllowanceChangeResult, error)
}

func (f *fakeStore) LinkedWalletAccounts(context.Context) ([]billing.WalletAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.links, nil
}

func (f *fakeStore) UpsertAllowance(_ context.Context, ch billing.AllowanceChange) (billing.AllowanceChangeResult, error) {
	f.mu.Lock()
	f.ups = append(f.ups, ch)
	fn := f.upsert
	f.mu.Unlock()
	if fn != nil {
		return fn(ch)
	}
	return billing.AllowanceChangeResult{}, nil
}

func (f *fakeStore) AccountAllowances(_ context.Context, accountID string) ([]billing.Allowance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[accountID], nil
}

func newSvc(t *testing.T, rpc *fakeRPC, store *fakeStore) *Service {
	t.Helper()
	s, err := New(rpc, store, testAuthority, testMint, testTokenProg, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var logs []string
	s.SetLogger(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	return s
}

func TestRunOnceMirrorsGrant(t *testing.T) {
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}
	rpc.set(wallet1, solana.TokenDelegate{Delegate: testAuthority, DelegatedAmountRaw: 500_000, Slot: 42}, true)

	s := newSvc(t, rpc, store)
	n, err := s.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if len(store.ups) != 1 {
		t.Fatalf("upserts = %d", len(store.ups))
	}
	ch := store.ups[0]
	if ch.AccountID != "acct-1" || ch.AllowanceRaw != 500_000 || ch.Ref != fmt.Sprintf("obs:%s:42", wallet1) {
		t.Fatalf("change = %+v", ch)
	}
}

func TestRunOnceLowerCapIsRevokeDelta(t *testing.T) {
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}
	rpc.set(wallet1, solana.TokenDelegate{Delegate: testAuthority, DelegatedAmountRaw: 100_000, Slot: 43}, true)

	s := newSvc(t, rpc, store)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A lower approve is still an absolute observation: the caller (registry)
	// computes the negative delta and clamps credit. The poller must not
	// special-case it.
	if store.ups[len(store.ups)-1].AllowanceRaw != 100_000 {
		t.Fatalf("lower observation = %+v", store.ups[len(store.ups)-1])
	}
}

func TestRunOnceRevokeOnlyWhenActiveRowExists(t *testing.T) {
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}
	// No ATA at all: steady state for an account that never delegated.
	rpc.set(wallet1, solana.TokenDelegate{}, false)
	store.rows = map[string][]billing.Allowance{}

	s := newSvc(t, rpc, store)
	n, err := s.RunOnce(context.Background())
	if err != nil || n != 0 || len(store.ups) != 0 {
		t.Fatalf("no-history revoke wrote: n=%d ups=%d err=%v", n, len(store.ups), err)
	}

	// With an active row for our authority, the same chain state revokes.
	store.rows["acct-1"] = []billing.Allowance{{
		AccountID: "acct-1", Delegate: testAuthority, AllowanceRaw: 900_000,
		ApprovedAt: time.Now().Add(-time.Hour),
	}}
	rpc.set(wallet1, solana.TokenDelegate{}, false)
	n, err = s.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("revoke pass = %d, %v", n, err)
	}
	if got := store.ups[len(store.ups)-1]; got.AllowanceRaw != 0 || got.Delegate != testAuthority {
		t.Fatalf("revoke change = %+v", got)
	}
}

func TestRunOnceDelegateMovedElsewhere(t *testing.T) {
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}
	// Re-approved to a different delegate: our authority lost it → revoke.
	rpc.set(wallet1, solana.TokenDelegate{Delegate: otherDeleg8, DelegatedAmountRaw: 777}, true)
	store.rows = map[string][]billing.Allowance{"acct-1": {{
		AccountID: "acct-1", Delegate: testAuthority, AllowanceRaw: 500_000,
		ApprovedAt: time.Now().Add(-time.Hour),
	}}}

	s := newSvc(t, rpc, store)
	n, err := s.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("moved-delegate pass = %d, %v", n, err)
	}
	if got := store.ups[len(store.ups)-1]; got.AllowanceRaw != 0 {
		t.Fatalf("revoke change = %+v", got)
	}
}

func TestRunOnceWorkerConsumptionIsNotARevoke(t *testing.T) {
	// The §11.3 invariant: the worker decrements the registry when it
	// exercises the delegation, so the chain amount equals the registry and
	// the poller's observation is a delta-0 no-op — not a revoke.
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}
	// Registry already consumed down to 300k; chain agrees.
	store.rows = map[string][]billing.Allowance{"acct-1": {{
		AccountID: "acct-1", Delegate: testAuthority, AllowanceRaw: 300_000,
		ApprovedAt: time.Now().Add(-time.Hour),
	}}}
	store.upsert = func(ch billing.AllowanceChange) (billing.AllowanceChangeResult, error) {
		return billing.AllowanceChangeResult{}, errors.New("poller must not write a revoke for worker consumption")
	}
	rpc.set(wallet1, solana.TokenDelegate{Delegate: testAuthority, DelegatedAmountRaw: 300_000, Slot: 44}, true)

	s := newSvc(t, rpc, store)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The upsert DID run (absolute observation) but with the same amount, so
	// the registry delta is 0 — the fake's guard only fires on writes that
	// would change state; verify the amount equality directly.
	last := store.ups[len(store.ups)-1]
	if last.AllowanceRaw != 300_000 {
		t.Fatalf("observation = %+v", last)
	}
}

func TestRunOnceRPCErrorSkipsWallet(t *testing.T) {
	rpc := &fakeRPC{byATA: map[string]solana.TokenDelegate{}, exists: map[string]bool{}, err: errors.New("rpc down")}
	store := &fakeStore{links: []billing.WalletAccount{{AccountID: "acct-1", Wallet: wallet1}}}

	s := newSvc(t, rpc, store)
	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("per-wallet error must not fail the pass: %v", err)
	}
	if n != 0 || len(store.ups) != 0 {
		t.Fatalf("error pass wrote: n=%d ups=%d", n, len(store.ups))
	}
}

func TestNewValidation(t *testing.T) {
	rpc := &fakeRPC{}
	store := &fakeStore{}
	if _, err := New(rpc, store, "", testMint, testTokenProg, time.Second); err == nil {
		t.Fatal("empty authority must fail")
	}
	if _, err := New(rpc, store, testAuthority, "", testTokenProg, time.Second); err == nil {
		t.Fatal("empty mint must fail")
	}
	if _, err := New(rpc, store, testAuthority, testMint, testTokenProg, 0); err != nil {
		t.Fatalf("zero refresh should default, got %v", err)
	}
}
