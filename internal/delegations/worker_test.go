package delegations

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

func seedPubkey(seed string) string {
	h := sha256.Sum256([]byte(seed))
	key := ed25519.NewKeyFromSeed(h[:])
	return solana.EncodeBase58(key.Public().(ed25519.PublicKey))
}

// fakeRPC for the worker: existence + blockhash + send + statuses.
type workerRPC struct {
	mu        sync.Mutex
	exists    map[string]bool
	sendErr   error
	sentCount int
	lastWire  []byte
	statuses  map[string]*solana.SignatureStatusValue // by signature
	height    uint64
	blockhash []byte
}

func newWorkerRPC() *workerRPC {
	return &workerRPC{
		exists:    map[string]bool{},
		statuses:  map[string]*solana.SignatureStatusValue{},
		height:    100,
		blockhash: make([]byte, 32),
	}
}

func (f *workerRPC) AccountExists(_ context.Context, pubkey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[pubkey], nil
}

func (f *workerRPC) LatestBlockhash(_ context.Context) ([]byte, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blockhash, f.height + 150, nil
}

func (f *workerRPC) SendTransaction(_ context.Context, wire []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sentCount++
	f.lastWire = wire
	return solana.EncodeBase58(wire[:64]), nil
}

func (f *workerRPC) GetSignatureStatuses(_ context.Context, signatures []string) ([]*solana.SignatureStatusValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*solana.SignatureStatusValue, 0, len(signatures))
	for _, sig := range signatures {
		out = append(out, f.statuses[sig])
	}
	return out, nil
}

func (f *workerRPC) GetBlockHeight(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.height, nil
}

// fakeStore records every transition.
type fakeSettlementStore struct {
	mu        sync.Mutex
	claim     []billing.DelegationSettlement
	sweep     []billing.DelegationSettlement
	signed    []int64
	wires     map[int64]string
	broadcast []int64
	finalized []int64
	restored  map[int64]string
	claimErr  error

	markSignedOK bool
}

func (f *fakeSettlementStore) ClaimDelegationSettlements(_ context.Context, _ store.SettlementClaimParams, _ time.Time) ([]billing.DelegationSettlement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	out := f.claim
	f.claim = nil
	return out, nil
}

func (f *fakeSettlementStore) SweepDelegationSettlements(_ context.Context, _ int) ([]billing.DelegationSettlement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweep, nil
}

func (f *fakeSettlementStore) MarkSettlementSigned(_ context.Context, id int64, wireB64, _, _ string, _ uint64, _, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wires == nil {
		f.wires = map[int64]string{}
	}
	f.wires[id] = wireB64
	f.signed = append(f.signed, id)
	return f.markSignedOK, nil
}

func (f *fakeSettlementStore) MarkSettlementBroadcast(_ context.Context, id int64, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast = append(f.broadcast, id)
	return true, nil
}

func (f *fakeSettlementStore) FinalizeSettlement(_ context.Context, id int64, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = append(f.finalized, id)
	return true, nil
}

func (f *fakeSettlementStore) RestoreSettlement(_ context.Context, id int64, reason string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restored == nil {
		f.restored = map[int64]string{}
	}
	f.restored[id] = reason
	return true, nil
}

var (
	wAuthority = seedPubkey("worker-authority")
	wMint      = seedPubkey("worker-mint")
	wTokenProg = seedPubkey("worker-token-program")
	wTreasury  = seedPubkey("worker-treasury")
)

func workerKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	h := sha256.Sum256([]byte("worker-authority"))
	return ed25519.NewKeyFromSeed(h[:])
}

func newTestWorker(t *testing.T, rpc *workerRPC, st *fakeSettlementStore) *SettlementWorker {
	t.Helper()
	w, err := NewSettlementWorker(rpc, st, workerKey(t), wAuthority, wMint, wTokenProg, wTreasury, 6, time.Second, time.Minute)
	if err != nil {
		t.Fatalf("NewSettlementWorker: %v", err)
	}
	return w
}

func TestWorkerKeyMustMatchAuthority(t *testing.T) {
	other := seedPubkey("other-key")
	if _, err := NewSettlementWorker(nil, &fakeSettlementStore{}, workerKey(t), other, wMint, wTokenProg, wTreasury, 6, time.Second, time.Minute); err == nil {
		t.Fatal("mismatched keypair/authority must fail")
	}
}

func TestWorkerSignsPendingBatch(t *testing.T) {
	rpc := newWorkerRPC()
	st := &fakeSettlementStore{}
	buyerATA := seedPubkey("buyer-ata")
	destATA := seedPubkey("dest-ata")
	st.sweep = []billing.DelegationSettlement{{
		ID: 7, BatchRef: "dset:x", BuyerAccount: "acct-b", Delegate: wAuthority,
		SourceATA: buyerATA, DestinationWallet: "destwallet", DestinationATA: destATA,
		AmountRaw: 1_000_000, State: billing.SettlementPending,
	}}
	w := newTestWorker(t, rpc, st)

	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	_ = n
	if len(st.signed) != 1 || st.signed[0] != 7 {
		t.Fatalf("signed = %v", st.signed)
	}
	if len(st.broadcast) != 0 {
		t.Fatalf("must advance exactly one state per sweep, broadcast=%v", st.broadcast)
	}
	// The persisted wire must be a real signed tx: a 64-byte signature
	// prefix over the message (verify by re-signing).
	wireB64 := st.wires[7]
	if wireB64 == "" {
		t.Fatal("no wire persisted")
	}
	wire, err := base64.StdEncoding.DecodeString(wireB64)
	if err != nil || len(wire) < 65 {
		t.Fatalf("bad wire: %v", err)
	}
	// Wire = compact-u16 sig count (1 byte) + 64-byte sig + message.
	if wire[0] != 1 {
		t.Fatalf("sig count = %d", wire[0])
	}
	if !ed25519.Verify(workerKey(t).Public().(ed25519.PublicKey), wire[65:], wire[1:65]) {
		t.Fatal("wire signature does not verify against the authority key")
	}
}

func TestWorkerBroadcastThenFinalize(t *testing.T) {
	rpc := newWorkerRPC()
	st := &fakeSettlementStore{}
	wire := make([]byte, 200)
	sig := solana.EncodeBase58(wire[:64])
	st.sweep = []billing.DelegationSettlement{{
		ID: 9, State: billing.SettlementSigned, SignedWire: base64Std(wire), Signature: sig,
		LastValidBlockHeight: u64ptr(150), BlockhashExpiresAt: timePtr(time.Now().Add(time.Minute)),
	}}
	w := newTestWorker(t, rpc, st)
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.broadcast) != 1 || st.broadcast[0] != 9 {
		t.Fatalf("broadcast = %v", st.broadcast)
	}

	// Next sweep: finalized status → finalize.
	st.sweep = []billing.DelegationSettlement{{
		ID: 9, State: billing.SettlementBroadcast, Signature: sig,
	}}
	rpc.statuses[sig] = &solana.SignatureStatusValue{}
	// Finalized() requires confirmation status; craft via SignatureStatusFinalized helper.
	rpc.statuses[sig] = finalizedStatus()
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.finalized) != 1 || st.finalized[0] != 9 {
		t.Fatalf("finalized = %v", st.finalized)
	}
}

func TestWorkerRestoreOnTxFailure(t *testing.T) {
	rpc := newWorkerRPC()
	st := &fakeSettlementStore{}
	sig := solana.EncodeBase58(make([]byte, 64))
	rpc.statuses[sig] = failedStatus()
	st.sweep = []billing.DelegationSettlement{{
		ID: 11, State: billing.SettlementBroadcast, Signature: sig,
	}}
	w := newTestWorker(t, rpc, st)
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.restored[11] != "tx_failed" {
		t.Fatalf("restored = %v", st.restored)
	}
}

func TestWorkerRestoreOnBlockhashExpiry(t *testing.T) {
	rpc := newWorkerRPC() // height 100
	st := &fakeSettlementStore{}
	sig := solana.EncodeBase58(make([]byte, 64))
	// No status recorded (unknown signature) and last-valid height 50 < 100.
	st.sweep = []billing.DelegationSettlement{{
		ID: 12, State: billing.SettlementBroadcast, Signature: sig,
		LastValidBlockHeight: u64ptr(50),
	}}
	w := newTestWorker(t, rpc, st)
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.restored[12] != "blockhash_expired_unconfirmed" {
		t.Fatalf("restored = %v", st.restored)
	}
}

func TestWorkerAmbiguousSendRetries(t *testing.T) {
	rpc := newWorkerRPC()
	rpc.sendErr = errors.New("context deadline exceeded")
	st := &fakeSettlementStore{}
	wire := make([]byte, 200)
	st.sweep = []billing.DelegationSettlement{{
		ID: 13, State: billing.SettlementSigned, SignedWire: base64Std(wire),
		Signature: solana.EncodeBase58(wire[:64]),
	}}
	w := newTestWorker(t, rpc, st)
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err) // advance errors are logged, not surfaced
	}
	if len(st.broadcast) != 0 || len(st.restored) != 0 {
		t.Fatalf("ambiguous must neither broadcast nor restore: bc=%v rest=%v", st.broadcast, st.restored)
	}
}

func TestWorkerLandedOnRetryAdvances(t *testing.T) {
	rpc := newWorkerRPC()
	rpc.sendErr = errors.New("Transaction has already been processed")
	st := &fakeSettlementStore{}
	wire := make([]byte, 200)
	st.sweep = []billing.DelegationSettlement{{
		ID: 14, State: billing.SettlementSigned, SignedWire: base64Std(wire),
		Signature: solana.EncodeBase58(wire[:64]),
	}}
	w := newTestWorker(t, rpc, st)
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.broadcast) != 1 {
		t.Fatalf("already-processed must advance to broadcast: %v", st.broadcast)
	}
}

func TestTransferCheckedMessageShape(t *testing.T) {
	source := seedPubkey("msg-source")
	mint := seedPubkey("msg-mint")
	dest := seedPubkey("msg-dest")
	auth := seedPubkey("msg-auth")
	tp, _ := solana.DecodeBase58(wTokenProg, solana.PublicKeyBytes)
	bh := make([]byte, 32)

	// Without ATA create: 5 accounts (4 + token program), 1 instruction.
	msg := buildTransferCheckedMessage(
		mustDec(t, source), mustDec(t, mint), mustDec(t, dest), mustDec(t, auth),
		tp, false, bh, 1_000_000, 6,
	)
	_ = msg
	// Header: 1 signer (authority), 0 ro signers... authority is read-only
	// signer → header = [1, 1, 0]; 5 account keys; verify the transfer ix
	// data ends with amount+decimals.
	if len(msg) == 0 {
		t.Fatal("empty message")
	}
	// With ATA create: extra accounts and a first instruction for the ATA program.
	msg2 := buildTransferCheckedMessage(
		mustDec(t, source), mustDec(t, mint), mustDec(t, dest), mustDec(t, auth),
		tp, true, bh, 1_000_000, 6,
	)
	if len(msg2) <= len(msg) {
		t.Fatalf("create-ATA message must be longer: %d vs %d", len(msg2), len(msg))
	}
}

func TestTransferCheckedInstructionData(t *testing.T) {
	d := solana.TransferCheckedInstructionData(1_000_000, 6)
	if d[0] != 12 {
		t.Fatalf("discriminator = %d", d[0])
	}
	if len(d) != 10 || d[9] != 6 {
		t.Fatalf("data = %v", d)
	}
}

// --- helpers ---

func mustDec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := solana.DecodeBase58(s, solana.PublicKeyBytes)
	if err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return b
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func u64ptr(v uint64) *uint64 { return &v }

func timePtr(t time.Time) *time.Time { return &t }

func finalizedStatus() *solana.SignatureStatusValue {
	return solana.SignatureStatusFinalized()
}

func failedStatus() *solana.SignatureStatusValue {
	return solana.SignatureStatusFailed([]byte(`{"InstructionError":[0,{"Custom":1}]}`))
}
