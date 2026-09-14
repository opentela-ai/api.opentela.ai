package deposits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// kb returns the base58 of a fixed 32-byte key (first 31 bytes b, last byte
// last). These are off-curve with overwhelming probability, so the derived
// treasury ATA never lands on the ed25519 curve.
func kb(b, last byte) string {
	out := make([]byte, 32)
	for i := 0; i < 31; i++ {
		out[i] = b
	}
	out[31] = last
	return solana.EncodeBase58(out)
}

var (
	treasuryWallet = kb(0x42, 0x01)
	mint           = kb(0x43, 0x02)
	tokenProgram   = kb(0x44, 0x03)
)

// buildTx builds a ConfirmedTransaction with one SPL Token transfer from src
// to dst, attributing senderOwner as the owner of the source token account
// via preTokenBalances (account index 1).
func buildTx(slot int64, src, dst, senderOwner string, amount int64) *solana.ConfirmedTransaction {
	info, _ := json.Marshal(map[string]string{
		"source":      src,
		"destination": dst,
		"authority":   senderOwner,
		"amount":      itoa64(amount),
	})
	return &solana.ConfirmedTransaction{
		Slot: slot,
		Transaction: solana.ParsedMessage{
			AccountKeys: []string{senderOwner, src, dst, tokenProgram},
			Instructions: []solana.ParsedIx{{
				ProgramID: tokenProgram,
				Parsed:    &solana.ParsedInfo{Type: "transfer", Info: info},
			}},
		},
		Meta: &solana.ParsedMeta{
			PreTokenBalances: []solana.TokenBalance{
				{AccountIndex: 1, Mint: mint, Owner: senderOwner},
			},
		},
	}
}

// buildCheckedTx builds a ConfirmedTransaction whose only token instruction
// is an SPL Token transferChecked (the shape wallets and dApps actually send:
// create-ATA followed by transferChecked). The mint rides on the instruction
// itself; attribution uses preTokenBalances exactly like a plain transfer.
func buildCheckedTx(slot int64, src, dst, senderOwner string, amount int64) *solana.ConfirmedTransaction {
	info, _ := json.Marshal(map[string]any{
		"source":      src,
		"destination": dst,
		"authority":   senderOwner,
		"mint":        mint,
		"tokenAmount": map[string]any{"amount": itoa64(amount), "decimals": 6},
	})
	return &solana.ConfirmedTransaction{
		Slot: slot,
		Transaction: solana.ParsedMessage{
			AccountKeys: []string{senderOwner, src, dst, tokenProgram},
			Instructions: []solana.ParsedIx{{
				ProgramID: tokenProgram,
				Parsed:    &solana.ParsedInfo{Type: "transferChecked", Info: info},
			}},
		},
		Meta: &solana.ParsedMeta{
			PreTokenBalances: []solana.TokenBalance{
				{AccountIndex: 1, Mint: mint, Owner: senderOwner},
			},
		},
	}
}

func TestRunOnceCreditsTransferChecked(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	// Regression: the mainnet deposit 5x8geeD8… used transferChecked (wallet-
	// built create-ATA + transferChecked), which the parser silently skipped —
	// the cursor advanced past it and the deposit was never persisted.
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-checked", Slot: 100, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-checked": buildCheckedTx(100, "srcATA", ata, "walletA", 200_000_000),
	}

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("examined = %d, want 1", n)
	}
	if len(st.inserts) != 1 || st.inserts[0].AmountRaw != 200_000_000 || st.inserts[0].FromWallet != "walletA" {
		t.Fatalf("transferChecked deposit must persist: inserts = %+v", st.inserts)
	}
	if len(st.applies) != 1 || st.applies[0].account != "acct-1" {
		t.Fatalf("transferChecked deposit must credit: applies = %+v", st.applies)
	}
	if svc.lastSig != "sig-checked" || st.cursor != "sig-checked" {
		t.Fatalf("cursor = %q/%q, want sig-checked", svc.lastSig, st.cursor)
	}
}

func TestRunOnceTransferCheckedWrongMintSkipped(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	// A transferChecked for a different mint must not be persisted — the mint
	// rides on the instruction and the watcher filters on it directly.
	offMintTxInfo, _ := json.Marshal(map[string]any{
		"source": "srcATA", "destination": ata, "authority": "walletA",
		"mint":        "SomeOtherMint11111111111111111111111111111111",
		"tokenAmount": map[string]any{"amount": "200000000", "decimals": 6},
	})
	offMint := buildCheckedTx(100, "srcATA", ata, "walletA", 200_000_000)
	offMint.Transaction.Instructions[0].Parsed = &solana.ParsedInfo{Type: "transferChecked", Info: offMintTxInfo}
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-off-mint", Slot: 100, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{"sig-off-mint": offMint}

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 0 || len(st.applies) != 0 {
		t.Fatalf("off-mint transferChecked must be skipped: inserts=%d applies=%d", len(st.inserts), len(st.applies))
	}
}

func TestRunOnceCreditsInnerTransfer(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	// Regression for recovery flows: token transfers executed as CPIs inside
	// an outer instruction (e.g. the ATA program's recover_nested) must be
	// parsed from meta.innerInstructions and credited.
	info, _ := json.Marshal(map[string]string{
		"source": "nestedATA", "destination": ata,
		"authority": "walletA", "amount": "200000000",
	})
	tx := &solana.ConfirmedTransaction{
		Slot: 100,
		Transaction: solana.ParsedMessage{
			AccountKeys: []string{"walletA", "nestedATA", ata, tokenProgram},
			Instructions: []solana.ParsedIx{{
				ProgramID: "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL",
				// recover_nested outer call has no parsed transfer payload.
			}},
		},
		Meta: &solana.ParsedMeta{
			PreTokenBalances: []solana.TokenBalance{
				{AccountIndex: 1, Mint: mint, Owner: "walletA"},
			},
			InnerInstructions: []solana.InnerInstructionGroup{{
				Index:        0,
				Instructions: []solana.ParsedIx{{ProgramID: tokenProgram, Parsed: &solana.ParsedInfo{Type: "transfer", Info: info}}},
			}},
		},
	}
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-inner", Slot: 100, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{"sig-inner": tx}

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 1 {
		t.Fatalf("inner transfer must be persisted: inserts = %+v", st.inserts)
	}
	// Synthetic index: outer 0, first inner instruction -> 0<<32|1.
	if st.inserts[0].InstructionIndex != 1 {
		t.Fatalf("inner transfer index = %d, want 1", st.inserts[0].InstructionIndex)
	}
	if st.inserts[0].AmountRaw != 200_000_000 {
		t.Fatalf("inner transfer amount = %d", st.inserts[0].AmountRaw)
	}
	if len(st.applies) != 1 || st.applies[0].account != "acct-1" {
		t.Fatalf("inner transfer must credit: applies = %+v", st.applies)
	}
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b [21]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// rawJSON is a small helper for building *json.RawMessage literals in tests.
func rawJSON(s string) *json.RawMessage { j := json.RawMessage(s); return &j }

// --- fakes ---

type fakeRPC struct {
	mu     sync.Mutex
	sigs   []solana.SignatureInfo
	txs    map[string]*solana.ConfirmedTransaction
	getErr error
	txErr  error
	txErrs map[string]error
	gotSig []sigCall
	gotTx  []string
}

type sigCall struct {
	address, before, commitment string
	limit                       int
}

func (f *fakeRPC) GetSignaturesForAddress(_ context.Context, address string, limit int, before, commitment string) ([]solana.SignatureInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotSig = append(f.gotSig, sigCall{address, before, commitment, limit})
	if f.getErr != nil {
		return nil, f.getErr
	}
	if before == "" {
		return f.sigs, nil
	}
	out := make([]solana.SignatureInfo, 0)
	skip := true
	for _, s := range f.sigs {
		if skip {
			if s.Signature == before {
				skip = false
			}
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeRPC) GetTransaction(_ context.Context, signature string) (*solana.ConfirmedTransaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotTx = append(f.gotTx, signature)
	if err := f.txErrs[signature]; err != nil {
		return nil, err
	}
	if f.txErr != nil {
		return nil, f.txErr
	}
	return f.txs[signature], nil
}

type fakeStore struct {
	mu         sync.Mutex
	inserts    []billing.DepositEvent
	applies    []applyCall
	advances   []advanceCall
	insertErr  error
	applyErr   error
	advanceErr error
	alreadyHas map[string]bool
	cursor     string
}

type applyCall struct {
	signature string
	index     int
	account   string
}

type advanceCall struct {
	from string
	to   string
}

func newFakeStore() *fakeStore { return &fakeStore{alreadyHas: map[string]bool{}} }

func (s *fakeStore) InsertDepositEvent(_ context.Context, ev billing.DepositEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return false, s.insertErr
	}
	key := ev.TransactionSignature + ":" + itoa64(int64(ev.InstructionIndex))
	if s.alreadyHas[key] {
		return false, nil
	}
	s.alreadyHas[key] = true
	s.inserts = append(s.inserts, ev)
	return true, nil
}

func (s *fakeStore) ApplyDeposit(_ context.Context, signature string, index int, account string, _ time.Time) (billing.DepositEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applyErr != nil {
		return billing.DepositEvent{}, s.applyErr
	}
	s.applies = append(s.applies, applyCall{signature, index, account})
	return billing.DepositEvent{TransactionSignature: signature, InstructionIndex: index, AssignmentState: "assigned"}, nil
}

func (s *fakeStore) DepositCursor(_ context.Context, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
}

func (s *fakeStore) AdvanceDepositCursor(_ context.Context, _ string, fromSignature, toSignature string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.advanceErr != nil {
		return false, s.advanceErr
	}
	if s.cursor != fromSignature {
		return false, nil
	}
	s.cursor = toSignature
	s.advances = append(s.advances, advanceCall{from: fromSignature, to: toSignature})
	return true, nil
}

type fakeLinker struct {
	accounts map[string]string
	err      error
	lookup   func(wallet string) (string, bool, error)
}

func (l *fakeLinker) AccountForWallet(_ context.Context, wallet string) (string, bool, error) {
	if l.lookup != nil {
		return l.lookup(wallet)
	}
	if l.err != nil {
		return "", false, l.err
	}
	a, ok := l.accounts[wallet]
	return a, ok, nil
}

// newService builds a watcher wired to fakes and returns it plus the derived
// treasury ATA (so tests build transfers to the right destination).
func newService(t *testing.T, linker *fakeLinker) (*Service, string) {
	t.Helper()
	rpc := &fakeRPC{}
	st := newFakeStore()
	svc, err := New(rpc, st, linker, treasuryWallet, mint, tokenProgram, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, svc.TreasuryATA()
}

// --- tests ---

func TestNewValidatesAddresses(t *testing.T) {
	rpc := &fakeRPC{}
	st := newFakeStore()
	ln := &fakeLinker{}
	if _, err := New(rpc, st, ln, "", mint, tokenProgram, time.Second); err == nil {
		t.Fatal("empty treasury should error")
	}
	if _, err := New(rpc, st, ln, treasuryWallet, "", tokenProgram, time.Second); err == nil {
		t.Fatal("empty mint should error")
	}
	if _, err := New(rpc, st, ln, treasuryWallet, mint, "", time.Second); err == nil {
		t.Fatal("empty tokenProgram should error")
	}
	if _, err := New(rpc, st, ln, treasuryWallet, mint, "notbase58!@", time.Second); err == nil {
		t.Fatal("bad tokenProgram should error")
	}
}

func TestRunOnceCreditsLinkedWallet(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-1", Slot: 100, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-1": buildTx(100, "srcATA", ata, "walletA", 7_000_000),
	}

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("examined = %d, want 1", n)
	}
	if len(st.inserts) != 1 || st.inserts[0].AmountRaw != 7_000_000 || st.inserts[0].FromWallet != "walletA" {
		t.Fatalf("inserts = %+v", st.inserts)
	}
	if len(st.applies) != 1 || st.applies[0].account != "acct-1" {
		t.Fatalf("applies = %+v", st.applies)
	}
	if svc.lastSig != "sig-1" || st.cursor != "sig-1" {
		t.Fatalf("lastSig = %q cursor = %q, want sig-1", svc.lastSig, st.cursor)
	}
	if len(rpc.gotSig) == 0 || rpc.gotSig[0].commitment != "finalized" {
		t.Fatalf("commitment = %+v, want finalized", rpc.gotSig)
	}
}

func TestRunOnceUnlinkedWalletLeftUnassigned(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{}}) // no links
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-u", Slot: 1, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-u": buildTx(1, "srcATA", ata, "walletUnknown", 500),
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 1 {
		t.Fatalf("unassigned transfer should still be persisted, inserts=%d", len(st.inserts))
	}
	if len(st.applies) != 0 {
		t.Fatalf("unlinked wallet must not be credited, applies=%+v", st.applies)
	}
}

func TestRunOnceSkipsFailedTransaction(t *testing.T) {
	svc, _ := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-fail", Slot: 1, Err: rawJSON(`"Custom"`), ConfirmationStatus: "finalized"}}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 0 || len(rpc.gotTx) != 0 {
		t.Fatalf("failed tx must be skipped, inserts=%d gotTx=%d", len(st.inserts), len(rpc.gotTx))
	}
}

func TestRunOnceSkipsTransferToOtherATA(t *testing.T) {
	svc, _ := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-other", Slot: 1, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-other": buildTx(1, "srcATA", "someOtherATA", "walletA", 999),
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 0 {
		t.Fatalf("transfer to non-treasury must not be persisted, inserts=%d", len(st.inserts))
	}
}

func TestRunOnceIdempotentAcrossPasses(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-1", Slot: 1, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{"sig-1": buildTx(1, "src", ata, "walletA", 100)}

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second pass: lastSig == "sig-1", so the loop stops at the high-water mark
	// without re-processing.
	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second pass examined = %d, want 0 (stop at high-water mark)", n)
	}
	if len(st.inserts) != 1 || len(st.applies) != 1 {
		t.Fatalf("after re-run: inserts=%d applies=%d, want 1/1", len(st.inserts), len(st.applies))
	}
}

func TestRunOnceNewSignatureAfterHighWater(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-1": buildTx(1, "src", ata, "walletA", 100),
		"sig-2": buildTx(2, "src", ata, "walletA", 200),
	}
	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-1", Slot: 1, ConfirmationStatus: "finalized"}}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A newer signature arrives at the front; the older one trails behind it.
	rpc.sigs = []solana.SignatureInfo{
		{Signature: "sig-2", Slot: 2, ConfirmationStatus: "finalized"},
		{Signature: "sig-1", Slot: 1, ConfirmationStatus: "finalized"},
	}
	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("examined = %d, want 1 (only sig-2 is new)", n)
	}
	if len(st.inserts) != 2 || len(st.applies) != 2 {
		t.Fatalf("inserts=%d applies=%d, want 2/2", len(st.inserts), len(st.applies))
	}
	if svc.lastSig != "sig-2" || st.cursor != "sig-2" {
		t.Fatalf("lastSig = %q cursor = %q, want sig-2", svc.lastSig, st.cursor)
	}
}

func TestRunOnceRPCErrorBubblesUp(t *testing.T) {
	svc, _ := newService(t, &fakeLinker{})
	rpc := svc.rpc.(*fakeRPC)
	rpc.getErr = errors.New("rpc down")
	if _, err := svc.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "rpc down") {
		t.Fatalf("err = %v, want rpc down", err)
	}
}

// TestRunOnceProcessesSuccessfulTxWithNullErr guards against a regression
// where the live RPC encodes a successful transaction's err as JSON `null`.
// With json.RawMessage (a []byte), `"err": null` decodes to the 4 bytes
// `null` (non-nil), which would wrongly cause the watcher to skip EVERY
// successful transaction. The field is therefore *json.RawMessage, so JSON
// null yields a nil pointer and the transfer is credited.
func TestRunOnceProcessesSuccessfulTxWithNullErr(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	// Decode exactly what the live RPC returns for a successful tx.
	var sigs []solana.SignatureInfo
	raw := []byte(`[{"signature":"sig-ok","slot":7,"err":null,"confirmationStatus":"finalized"}]`)
	if err := json.Unmarshal(raw, &sigs); err != nil {
		t.Fatal(err)
	}
	if sigs[0].Err != nil {
		t.Fatalf("decoding `err: null` must yield nil pointer, got %q", string(*sigs[0].Err))
	}
	rpc.sigs = sigs
	rpc.txs = map[string]*solana.ConfirmedTransaction{"sig-ok": buildTx(7, "srcATA", ata, "walletA", 5_000_000)}

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("examined = %d, want 1 (successful tx must not be skipped)", n)
	}
	if len(st.inserts) != 1 || len(st.applies) != 1 {
		t.Fatalf("inserts=%d applies=%d, want 1/1 (null err must be credited)", len(st.inserts), len(st.applies))
	}
}

// TestRunOnceSkipsFailedTxWithObjectErr guards the other direction: a live
// RPC encodes a failed transaction's err as a JSON object/array/string, which
// must decode to a non-nil pointer and be skipped without crediting.
func TestRunOnceSkipsFailedTxWithObjectErr(t *testing.T) {
	svc, _ := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	var sigs []solana.SignatureInfo
	raw := []byte(`[{"signature":"sig-bad","slot":7,"err":{"InstructionError":[1,"Custom"]},"confirmationStatus":"finalized"}]`)
	if err := json.Unmarshal(raw, &sigs); err != nil {
		t.Fatal(err)
	}
	if sigs[0].Err == nil {
		t.Fatal("decoding a non-null err must yield a non-nil pointer")
	}
	rpc.sigs = sigs
	rpc.txs = map[string]*solana.ConfirmedTransaction{} // must not be fetched

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.inserts) != 0 || len(rpc.gotTx) != 0 {
		t.Fatalf("failed tx (object err) must be skipped: inserts=%d gotTx=%d", len(st.inserts), len(rpc.gotTx))
	}
}

func TestRunDoesNotCrashOnPerTxError(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	// sig-bad fails to fetch; sig-ok still succeeds, but the cursor must stay
	// behind the failed older signature so the page is retried.
	rpc.sigs = []solana.SignatureInfo{
		{Signature: "sig-ok", Slot: 2, ConfirmationStatus: "finalized"},
		{Signature: "sig-bad", Slot: 1, ConfirmationStatus: "finalized"},
	}
	rpc.txErrs = map[string]error{"sig-bad": errors.New("get tx failed")}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-ok":  buildTx(2, "src-ok", ata, "walletA", 200),
		"sig-bad": buildTx(1, "src-bad", ata, "walletA", 100),
	}
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should absorb per-tx errors, got %v", err)
	}
	if len(st.inserts) != 1 || st.inserts[0].TransactionSignature != "sig-ok" {
		t.Fatalf("inserts=%+v, want only sig-ok persisted on first pass", st.inserts)
	}
	if len(st.applies) != 1 || st.applies[0].signature != "sig-ok" {
		t.Fatalf("applies=%+v, want only sig-ok applied on first pass", st.applies)
	}
	if st.cursor != "" || svc.lastSig != "" {
		t.Fatalf("lastSig = %q cursor = %q, want empty after failed older signature", svc.lastSig, st.cursor)
	}

	delete(rpc.txErrs, "sig-bad")
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce retry: %v", err)
	}
	if st.cursor != "sig-ok" || svc.lastSig != "sig-ok" {
		t.Fatalf("lastSig = %q cursor = %q, want sig-ok after retry", svc.lastSig, st.cursor)
	}
	if len(st.inserts) != 2 {
		t.Fatalf("inserts=%d, want 2 after retry", len(st.inserts))
	}
}

func TestRunOnceBoundedFirstPass(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"w": "a"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)
	// A full first page of history (pollLimit defaults to 100). The watcher
	// must process exactly this page and NOT page backward into older
	// history on its first pass — bounding startup cost for fresh treasuries.
	sigs := make([]solana.SignatureInfo, svc.pollLimit)
	txs := map[string]*solana.ConfirmedTransaction{}
	for i := range sigs {
		name := fmt.Sprintf("sig-%03d", i)
		sigs[i] = solana.SignatureInfo{Signature: name, Slot: int64(i), ConfirmationStatus: "finalized"}
		txs[name] = buildTx(int64(i), "src", ata, "w", int64(100+i))
	}
	rpc.sigs = sigs
	rpc.txs = txs

	n, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != svc.pollLimit {
		t.Fatalf("first pass examined = %d, want %d (one page only)", n, svc.pollLimit)
	}
	if len(rpc.gotSig) != 1 {
		t.Fatalf("GetSignaturesForAddress calls = %d, want 1 (no backward paging on first pass)", len(rpc.gotSig))
	}
	if len(st.inserts) != svc.pollLimit {
		t.Fatalf("inserts = %d, want %d", len(st.inserts), svc.pollLimit)
	}
	if svc.lastSig != sigs[0].Signature || st.cursor != sigs[0].Signature {
		t.Fatalf("lastSig = %q cursor = %q, want %q", svc.lastSig, st.cursor, sigs[0].Signature)
	}
}

func TestRunOnceRetriesApplyForAlreadyPersistedDeposit(t *testing.T) {
	svc, ata := newService(t, &fakeLinker{accounts: map[string]string{"walletA": "acct-1"}})
	rpc := svc.rpc.(*fakeRPC)
	st := svc.store.(*fakeStore)

	rpc.sigs = []solana.SignatureInfo{{Signature: "sig-persisted", Slot: 1, ConfirmationStatus: "finalized"}}
	rpc.txs = map[string]*solana.ConfirmedTransaction{
		"sig-persisted": buildTx(1, "srcATA", ata, "walletA", 123),
	}
	st.alreadyHas["sig-persisted:0"] = true

	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.inserts) != 0 {
		t.Fatalf("inserts=%d, want 0 for already-persisted row", len(st.inserts))
	}
	if len(st.applies) != 1 || st.applies[0].signature != "sig-persisted" {
		t.Fatalf("applies=%+v, want retry apply for persisted deposit", st.applies)
	}
	if st.cursor != "sig-persisted" {
		t.Fatalf("cursor = %q, want sig-persisted", st.cursor)
	}
}

// TestRunBacksOffOnPersistentError verifies Run does NOT busy-loop when the
// RPC fails on a (simulated) second page after the first succeeded with a
// full page — the historical case where `err != nil && n >= pollLimit`.
// We assert the poll count grows slowly (paced by pollInterval), not
// thousands of times.
func TestRunBacksOffOnPersistentError(t *testing.T) {
	svc, _ := newService(t, &fakeLinker{accounts: map[string]string{}})
	rpc := svc.rpc.(*fakeRPC)
	svc.pollInterval = 20 * time.Millisecond
	// First call returns a full page (all skipped, no txs). Subsequent calls
	// error. Without the backoff, Run would re-poll immediately thousands of
	// times before the 400ms deadline.
	page := make([]solana.SignatureInfo, svc.pollLimit)
	for i := range page {
		page[i] = solana.SignatureInfo{Signature: fmt.Sprintf("s%d", i), Slot: int64(i), ConfirmationStatus: "finalized"}
	}
	// Make the very first call error too — simplest way to force err != nil.
	rpc.getErr = errors.New("rpc down")

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	svc.Run(ctx)
	elapsed := time.Since(start)

	// At 20ms pacing, ~20 polls fit in 400ms. A busy loop would do thousands.
	// Allow generous headroom (a paused scheduler) without weakening the test.
	if len(rpc.gotSig) > 200 {
		t.Fatalf("Run busy-looped: %d GetSignaturesForAddress calls in %s (want pace-by-pollInterval)", len(rpc.gotSig), elapsed)
	}
	if len(rpc.gotSig) < 2 {
		t.Fatalf("Run did not retry: %d calls in %s", len(rpc.gotSig), elapsed)
	}
}
