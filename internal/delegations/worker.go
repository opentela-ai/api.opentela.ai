package delegations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
	"golang.org/x/crypto/ed25519"
)

// The settlement worker (design §11.5): replays delegation-backed ledger
// legs on-chain. Each pass (1) claims the next wave of unsettled legs —
// netted per (buyer, destination), allowance consumed atomically at claim —
// and (2) advances every in-flight batch exactly one state:
//
//	pending → signed (transfer_checked signed by the settlement authority,
//	            the delegate; it also pays the tx fee, so the authority
//	            keypair must hold SOL)
//	signed  → broadcast (persisted wire; ambiguous send errors retry,
//	            blockhash expiry restores the allowance)
//	broadcast → finalized (getSignatureStatuses; on-chain failure or
//	            expiry restores the allowance)
//
// Restore refunds the buyer's registry allowance — the on-chain authority
// is still there — and leaves the legs anchored; §11.5.5 reconciliation
// owns the residual exposure. The state machine mirrors the withdrawal
// worker (internal/withdraw) including SKIP LOCKED replica safety.

// settlementRPC is the chain surface the worker needs (implemented by
// *solana.RPCClient).
type settlementRPC interface {
	AccountExists(ctx context.Context, pubkey string) (bool, error)
	LatestBlockhash(ctx context.Context) ([]byte, uint64, error)
	SendTransaction(ctx context.Context, wire []byte) (string, error)
	GetSignatureStatuses(ctx context.Context, signatures []string) ([]*solana.SignatureStatusValue, error)
	GetBlockHeight(ctx context.Context) (uint64, error)
}

// settlementStore is the durable surface the worker needs.
type settlementStore interface {
	ClaimDelegationSettlements(ctx context.Context, params store.SettlementClaimParams, now time.Time) ([]billing.DelegationSettlement, error)
	SweepDelegationSettlements(ctx context.Context, limit int) ([]billing.DelegationSettlement, error)
	MarkSettlementSigned(ctx context.Context, id int64, wireB64, signature, blockhash string, lastValidBlockHeight uint64, expiresAt, now time.Time) (bool, error)
	MarkSettlementBroadcast(ctx context.Context, id int64, now time.Time) (bool, error)
	FinalizeSettlement(ctx context.Context, id int64, now time.Time) (bool, error)
	RestoreSettlement(ctx context.Context, id int64, reason string, now time.Time) (bool, error)
}

// SettlementWorker is the settlement worker.
type SettlementWorker struct {
	rpc             settlementRPC
	store           settlementStore
	key             ed25519.PrivateKey // the delegate: signs transfer_checked
	authority       string             // base58 pubkey of key
	mint            []byte
	tokenProgram    []byte
	treasuryWallet  string
	decimals        uint8
	claimLimit      int
	sweepLimit      int
	pollInterval    time.Duration
	blockhashMaxAge time.Duration
	logf            func(format string, args ...any)
	nowFn           func() time.Time

	runOnce sync.Mutex
}

// New builds a settlement worker. key must be the settlement authority's
// keypair (config verifies it against BILLING_SETTLEMENT_AUTHORITY on boot).
func NewSettlementWorker(rpc settlementRPC, st settlementStore, key ed25519.PrivateKey, authority, mint, tokenProgram, treasuryWallet string, decimals int, pollInterval, blockhashMaxAge time.Duration) (*SettlementWorker, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("delegations: settlement keypair is required")
	}
	if authority == "" {
		return nil, fmt.Errorf("delegations: settlement authority is required")
	}
	if got := solana.EncodeBase58(key.Public().(ed25519.PublicKey)); got != authority {
		return nil, fmt.Errorf("delegations: settlement keypair does not match authority (got %s, want %s)", got, authority)
	}
	m, err := solana.DecodeBase58(mint, solana.PublicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("delegations: mint: %w", err)
	}
	tp, err := solana.DecodeBase58(tokenProgram, solana.PublicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("delegations: token program: %w", err)
	}
	if decimals < 0 || decimals > 255 {
		return nil, fmt.Errorf("delegations: decimals out of range: %d", decimals)
	}
	if pollInterval <= 0 {
		pollInterval = 10 * time.Second
	}
	if blockhashMaxAge <= 0 {
		blockhashMaxAge = 90 * time.Second
	}
	return &SettlementWorker{
		rpc:             rpc,
		store:           st,
		key:             key,
		authority:       authority,
		mint:            m,
		tokenProgram:    tp,
		treasuryWallet:  treasuryWallet,
		decimals:        uint8(decimals),
		claimLimit:      500,
		sweepLimit:      20,
		pollInterval:    pollInterval,
		blockhashMaxAge: blockhashMaxAge,
		nowFn:           time.Now,
	}, nil
}

// SetLogger installs a printf-style logger.
func (s *SettlementWorker) SetLogger(fn func(format string, args ...any)) {
	s.logf = fn
}

func (s *SettlementWorker) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Run drives the worker until ctx is cancelled.
func (s *SettlementWorker) Run(ctx context.Context) {
	s.log("settlement worker started (authority %s, poll %s)", s.authority, s.pollInterval)
	t := time.NewTicker(s.pollInterval)
	defer t.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log("settlement sweep error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce performs one sweep: claim a wave of new batches, then advance
// each in-flight batch exactly one state. Returns the number of batches
// advanced.
func (s *SettlementWorker) RunOnce(ctx context.Context) (int, error) {
	s.runOnce.Lock()
	defer s.runOnce.Unlock()

	// 1. Claim new batches from unsettled legs.
	claimed, err := s.store.ClaimDelegationSettlements(ctx, store.SettlementClaimParams{
		Delegate:       s.authority,
		Mint:           solana.EncodeBase58(s.mint),
		TokenProgram:   solana.EncodeBase58(s.tokenProgram),
		TreasuryWallet: s.treasuryWallet,
		LegLimit:       s.claimLimit,
	}, s.nowFn())
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if len(claimed) > 0 {
		s.log("claimed %d settlement batch(es) from unsettled legs", len(claimed))
	}

	// 2. Advance in-flight batches one state each.
	due, err := s.store.SweepDelegationSettlements(ctx, s.sweepLimit)
	if err != nil {
		return 0, fmt.Errorf("sweep: %w", err)
	}
	advanced := 0
	for _, b := range due {
		if err := ctx.Err(); err != nil {
			return advanced, err
		}
		if err := s.advance(ctx, b); err != nil {
			s.log("settlement %d (%s): %v", b.ID, b.State, err)
			continue
		}
		advanced++
	}
	return advanced, nil
}

// advance performs exactly one state transition for b.
func (s *SettlementWorker) advance(ctx context.Context, b billing.DelegationSettlement) error {
	switch b.State {
	case billing.SettlementPending:
		return s.signAndStore(ctx, b)
	case billing.SettlementSigned:
		return s.broadcastStored(ctx, b)
	case billing.SettlementBroadcast:
		return s.finalizeOrRestore(ctx, b)
	default:
		return nil
	}
}

// signAndStore builds and signs the transfer_checked (buyer ATA →
// destination ATA, authority = the delegate) against a fresh blockhash and
// persists it as 'signed'. A lost race (another replica signed first) is a
// clean no-op via the store's state guard.
func (s *SettlementWorker) signAndStore(ctx context.Context, b billing.DelegationSettlement) error {
	source, err := solana.DecodeBase58(b.SourceATA, solana.PublicKeyBytes)
	if err != nil {
		if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "invalid source ata", s.nowFn()); rerr != nil {
			return fmt.Errorf("restore after invalid source: %w", rerr)
		}
		return nil
	}
	destATA, err := solana.DecodeBase58(b.DestinationATA, solana.PublicKeyBytes)
	if err != nil {
		if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "invalid destination ata", s.nowFn()); rerr != nil {
			return fmt.Errorf("restore after invalid destination: %w", rerr)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	exists, err := s.rpc.AccountExists(rctx, b.DestinationATA)
	if err != nil {
		return fmt.Errorf("account exists: %w", err)
	}
	bh, lastValidBlockHeight, err := s.rpc.LatestBlockhash(rctx)
	if err != nil {
		return fmt.Errorf("latest blockhash: %w", err)
	}

	message := buildTransferCheckedMessage(
		source, s.mint, destATA, s.key.Public().(ed25519.PublicKey),
		s.tokenProgram, !exists, bh, uint64(b.AmountRaw), s.decimals,
	)
	sig := ed25519.Sign(s.key, message)
	wire := solana.SerializeTransaction(sig, message)
	signature := solana.EncodeBase58(sig)
	wireB64 := base64.StdEncoding.EncodeToString(wire)

	ok, err := s.store.MarkSettlementSigned(ctx, b.ID, wireB64, signature,
		solana.EncodeBase58(bh), lastValidBlockHeight,
		s.nowFn().Add(s.blockhashMaxAge), s.nowFn())
	if err != nil {
		return fmt.Errorf("mark signed: %w", err)
	}
	if !ok {
		return nil // another replica won the signing race
	}
	return nil
}

// broadcastStored reads the PERSISTED signed wire and broadcasts it.
// Ambiguous results retry next sweep; blockhash expiry restores.
func (s *SettlementWorker) broadcastStored(ctx context.Context, b billing.DelegationSettlement) error {
	if b.SignedWire == "" {
		return fmt.Errorf("no signed wire")
	}
	wire, err := base64.StdEncoding.DecodeString(b.SignedWire)
	if err != nil {
		if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "corrupt signed wire", s.nowFn()); rerr != nil {
			return fmt.Errorf("restore after corrupt wire: %w", rerr)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := s.rpc.SendTransaction(rctx, wire); err != nil {
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "already been processed"),
			strings.Contains(msg, "already known"),
			strings.Contains(msg, "transaction results in an account already in use"):
			// Landed or in flight — advance so the next sweep finalizes.
		case strings.Contains(msg, "blockhash not found"),
			strings.Contains(msg, "blockhash expired"),
			strings.Contains(msg, "block has expired"):
			if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "blockhash_expired", s.nowFn()); rerr != nil {
				return fmt.Errorf("restore after blockhash expiry: %w", rerr)
			}
			return nil
		default:
			// Ambiguous: retry next sweep. Never restore on ambiguity —
			// the transaction may have landed unheard.
			return fmt.Errorf("send (will retry): %w", err)
		}
	}
	if _, err := s.store.MarkSettlementBroadcast(ctx, b.ID, s.nowFn()); err != nil {
		return fmt.Errorf("mark broadcast: %w", err)
	}
	return nil
}

// finalizeOrRestore queries the persisted signature. Finalized → finalize;
// failed on-chain or expired unknown → restore (refund the allowance).
func (s *SettlementWorker) finalizeOrRestore(ctx context.Context, b billing.DelegationSettlement) error {
	if b.Signature == "" {
		return fmt.Errorf("no signature to finalize")
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	statuses, err := s.rpc.GetSignatureStatuses(rctx, []string{b.Signature})
	if err != nil {
		return fmt.Errorf("signature statuses (will retry): %w", err)
	}
	var st *solana.SignatureStatusValue
	if len(statuses) > 0 {
		st = statuses[0]
	}
	now := s.nowFn()
	switch {
	case st != nil && st.Finalized():
		if _, err := s.store.FinalizeSettlement(ctx, b.ID, now); err != nil {
			return fmt.Errorf("finalize: %w", err)
		}
		return nil
	case st != nil && len(st.Status.Err) > 0:
		if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "tx_failed", now); rerr != nil {
			return fmt.Errorf("restore after tx failure: %w", rerr)
		}
		return nil
	case st == nil && b.LastValidBlockHeight != nil:
		height, err := s.rpc.GetBlockHeight(rctx)
		if err != nil {
			return fmt.Errorf("block height (will retry): %w", err)
		}
		if height > *b.LastValidBlockHeight {
			if _, rerr := s.store.RestoreSettlement(ctx, b.ID, "blockhash_expired_unconfirmed", now); rerr != nil {
				return fmt.Errorf("restore after blockhash expiry: %w", rerr)
			}
			return nil
		}
		return fmt.Errorf("pending")
	default:
		return fmt.Errorf("pending")
	}
}

// buildTransferCheckedMessage composes the signed-message bytes for a
// delegation settlement: transfer_checked from the buyer's ATA to the
// destination ATA with the settlement authority (the delegate) as the
// spending authority. The destination ATA is created first when absent.
//
// SPL Token transfer_checked: discriminator 12, data = u64 amount + u8
// decimals; accounts = [source, mint, destination, authority].
func buildTransferCheckedMessage(
	source, mint, destATA, authority, tokenProgram []byte,
	createDestATA bool, blockhash []byte, amountRaw uint64, decimals uint8,
) []byte {
	accounts := []solana.AccountMeta{
		{Key: source, Writable: true},                   // 0 source
		{Key: mint, Writable: false},                    // 1 mint
		{Key: destATA, Writable: true},                  // 2 destination
		{Key: authority, Signer: true, Writable: false}, // 3 authority (delegate, fee payer)
	}
	var ixs []solana.Instruction
	tokenProgramIndex := byte(4)
	if createDestATA {
		accounts = append(accounts,
			solana.AccountMeta{Key: authority}, // 4 payer for the ATA create
			solana.AccountMeta{Key: mint},      // 5
			solana.AccountMeta{Key: solana.SystemProgramID},
			solana.AccountMeta{Key: solana.ATAProgramID},
			solana.AccountMeta{Key: tokenProgram},
		)
		createAccounts := []byte{0, 2, 3, 5, 7}
		ixs = append(ixs, solana.Instruction{
			ProgramIndex: 6, // ATA program
			Accounts:     createAccounts,
			Data:         solana.CreateATAInstructionData,
		})
		tokenProgramIndex = 8
	}
	ixs = append(ixs, solana.Instruction{
		ProgramIndex: tokenProgramIndex,
		Accounts:     []byte{0, 1, 2, 3}, // source, mint, destination, authority
		Data:         solana.TransferCheckedInstructionData(amountRaw, decimals),
	})
	return solana.BuildMessage(accounts, blockhash, ixs)
}
