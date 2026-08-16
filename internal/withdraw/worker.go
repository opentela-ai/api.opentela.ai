// Package withdraw advances durable withdrawal records
// (reserved -> signed -> broadcast -> finalized) from off-chain OTELA credit
// to an on-chain SPL transfer out of the treasury. The reserve debits
// account credit immediately (internal/store); this worker only signs,
// broadcasts, and finalizes — it never computes amounts or moves balances.
//
// The worker is the mirror of the deposit watcher: it reuses the shared
// Solana wire-format builders (internal/solana/tx.go), polls at a configured
// cadence, and on an ambiguous RPC result keeps funds reserved and queries
// transaction status. Credit is restored only after blockhash expiry proves
// the transaction cannot land.
package withdraw

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// rpcClient is the subset of solana.RPCClient the worker needs.
type rpcClient interface {
	LatestBlockhash(ctx context.Context) ([]byte, uint64, error)
	GetBlockHeight(ctx context.Context) (uint64, error)
	AccountExists(ctx context.Context, pubkey string) (bool, error)
	SendTransaction(ctx context.Context, wire []byte) (string, error)
	GetSignatureStatuses(ctx context.Context, signatures []string) ([]*solana.SignatureStatusValue, error)
}

// withdrawalStore is the subset of the billing store the worker needs. Each
// transition is state-guarded and row-locked inside its own short transaction,
// so a replica that dies mid-work releases the row and another claims it next
// sweep. The worker does exactly one transition per row per RunOnce, and the
// broadcast/finalize steps always read the PERSISTED signed wire/signature
// (never the worker's own computation), so two replicas racing cannot
// broadcast different transactions for the same withdrawal.
type withdrawalStore interface {
	SweepWithdrawalsDue(ctx context.Context, limit int) ([]billing.Withdrawal, error)
	MarkWithdrawalSigned(ctx context.Context, id int64, signedWire, signature, blockhash string, lastValidBlockHeight uint64, blockhashExpiresAt, now time.Time) (billing.Withdrawal, error)
	MarkWithdrawalBroadcast(ctx context.Context, id int64, now time.Time) (billing.Withdrawal, error)
	FinalizeWithdrawal(ctx context.Context, id int64, now time.Time) (billing.Withdrawal, error)
	RestoreWithdrawal(ctx context.Context, id int64, reason string, now time.Time) (billing.Withdrawal, error)
}

// Service advances due withdrawals. Run blocks until ctx is canceled; RunOnce
// performs a single pass and is the unit-test entry point.
type Service struct {
	rpc             rpcClient
	store           withdrawalStore
	treasuryKey     ed25519.PrivateKey
	treasuryPub     []byte
	treasuryATA     []byte
	mint            []byte
	tokenProgram    []byte
	token2022       bool
	batchLimit      int
	pollInterval    time.Duration
	blockhashMaxAge time.Duration
	rpcTimeout      time.Duration
	now             func() time.Time
	log             func(format string, args ...any)
}

// New builds a Service. treasuryKey is the private key that owns the treasury
// ATA and signs outgoing transfers; its public key must match the wallet the
// deposit watcher credits. mint and tokenProgram describe the OTELA token
// (same as the deposit/faucet configuration). pollInterval is the cadence
// between sweeps; blockhashMaxAge is the conservative wall-clock window after
// which a non-finalized broadcast is treated as expired and restored.
func New(rpc rpcClient, store withdrawalStore, treasuryKey ed25519.PrivateKey, mint, tokenProgram string, pollInterval, blockhashMaxAge time.Duration) (*Service, error) {
	if rpc == nil || store == nil {
		return nil, fmt.Errorf("withdraw: nil dependency")
	}
	if len(treasuryKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("withdraw: treasury key must be %d bytes", ed25519.PrivateKeySize)
	}
	mintBytes, err := solana.DecodeBase58(mint, solana.PublicKeyBytes)
	if err != nil || len(mintBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("withdraw: invalid mint: %w", err)
	}
	tpBytes, err := solana.DecodeBase58(tokenProgram, solana.PublicKeyBytes)
	if err != nil || len(tpBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("withdraw: invalid token program: %w", err)
	}
	treasuryPub := treasuryKey.Public().(ed25519.PublicKey)
	ata, err := solana.AssociatedTokenAddress(treasuryPub, mintBytes, tpBytes)
	if err != nil {
		return nil, fmt.Errorf("withdraw: derive treasury ATA: %w", err)
	}
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	if blockhashMaxAge <= 0 {
		blockhashMaxAge = 90 * time.Second
	}
	return &Service{
		rpc:             rpc,
		store:           store,
		treasuryKey:     treasuryKey,
		treasuryPub:     treasuryPub,
		treasuryATA:     ata,
		mint:            mintBytes,
		tokenProgram:    tpBytes,
		token2022:       bytesEqual(tpBytes, mustDecodeB58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")),
		batchLimit:      20,
		pollInterval:    pollInterval,
		blockhashMaxAge: blockhashMaxAge,
		rpcTimeout:      20 * time.Second,
		now:             time.Now,
		log:             func(string, ...any) {},
	}, nil
}

// SetLogger installs a printf-style logger (default is silent).
func (s *Service) SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		s.log = fn
	}
}

// withTimeout derives a context bounded by the per-RPC timeout so a slow node
// cannot stall the sweep indefinitely.
func (s *Service) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.rpcTimeout)
}

// Run sweeps due withdrawals at pollInterval until ctx is canceled. Transient
// errors are logged and the loop backs off one interval; they never terminate
// the worker.
func (s *Service) Run(ctx context.Context) {
	s.log("withdraw: sweeping (treasury %s, mint %s, poll %s)", solana.EncodeBase58(s.treasuryPub), solana.EncodeBase58(s.mint), s.pollInterval)
	t := time.NewTicker(s.pollInterval)
	defer t.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.log("withdraw: sweep error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce performs a single sweep: claim the batch of due withdrawals
// (reserved/signed/broadcast) and advance each exactly one state. Returns the
// number of withdrawals advanced this pass.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	due, err := s.store.SweepWithdrawalsDue(ctx, s.batchLimit)
	if err != nil {
		return 0, fmt.Errorf("sweep: %w", err)
	}
	advanced := 0
	for _, w := range due {
		if err := ctx.Err(); err != nil {
			return advanced, err
		}
		if err := s.advance(ctx, w); err != nil {
			s.log("withdraw %d (%s): %v", w.ID, w.State, err)
			continue
		}
		advanced++
	}
	return advanced, nil
}

// advance performs exactly one state transition for w. The store's
// state-guarded updates mean a lost race (another replica advanced the row) is
// a clean no-op.
func (s *Service) advance(ctx context.Context, w billing.Withdrawal) error {
	switch w.State {
	case billing.WithdrawalReserved:
		return s.signAndStore(ctx, w)
	case billing.WithdrawalSigned:
		return s.broadcastStored(ctx, w)
	case billing.WithdrawalBroadcast:
		return s.finalizeOrRestore(ctx, w)
	default:
		// SweepWithdrawalsDue only returns reserved/signed/broadcast, so this
		// is unreachable in practice; treat as already done.
		return nil
	}
}

// signAndStore builds and signs the SPL transfer (treasury ATA -> destination
// ATA, creating the dest ATA when absent) against a fresh blockhash and
// persists it as 'signed'. A lost race (row no longer 'reserved') is a no-op.
func (s *Service) signAndStore(ctx context.Context, w billing.Withdrawal) error {
	destOwner, err := solana.DecodeBase58(w.DestinationWallet, solana.PublicKeyBytes)
	if err != nil || len(destOwner) != solana.PublicKeyBytes {
		// The API validated this at reserve time, but a persisted bad
		// destination should not wedge the worker forever — restore the
		// credit so the operator can see the error.
		_, rerr := s.store.RestoreWithdrawal(ctx, w.ID, "invalid destination wallet", s.now())
		if rerr != nil {
			return fmt.Errorf("restore after invalid destination: %w", rerr)
		}
		return nil
	}
	destATA, err := solana.AssociatedTokenAddress(destOwner, s.mint, s.tokenProgram)
	if err != nil {
		return fmt.Errorf("derive dest ATA: %w", err)
	}

	rctx, cancel := s.withTimeout(ctx)
	defer cancel()
	exists, err := s.rpc.AccountExists(rctx, solana.EncodeBase58(destATA))
	if err != nil {
		return fmt.Errorf("account exists: %w", err)
	}
	bh, lastValidBlockHeight, err := s.rpc.LatestBlockhash(rctx)
	if err != nil {
		return fmt.Errorf("latest blockhash: %w", err)
	}

	message := buildTransferMessage(
		s.treasuryPub, s.treasuryATA, destATA, destOwner, s.mint,
		s.tokenProgram, !exists, s.token2022, bh, uint64(w.AmountRaw),
	)
	sig := ed25519.Sign(s.treasuryKey, message)
	wire := solana.SerializeTransaction(sig, message)
	signature := solana.EncodeBase58(sig)
	wireB64 := base64.StdEncoding.EncodeToString(wire)

	if _, err := s.store.MarkWithdrawalSigned(ctx, w.ID, wireB64, signature,
		solana.EncodeBase58(bh), lastValidBlockHeight, s.now().Add(s.blockhashMaxAge), s.now()); err != nil {
		return fmt.Errorf("mark signed: %w", err)
	}
	return nil
}

// broadcastStored reads the PERSISTED signed wire (the winning replica's, even
// if this replica lost the signing race) and broadcasts it. On an ambiguous
// result (timeout / 5xx) the row stays 'signed' for the next sweep; on a
// definitive blockhash-expiry or rejection the credit is restored.
func (s *Service) broadcastStored(ctx context.Context, w billing.Withdrawal) error {
	if w.SignedWire == "" {
		// Nothing to broadcast (another replica is mid-sign, or a prior
		// attempt crashed before persisting). Leave for the next sweep.
		return fmt.Errorf("no signed wire")
	}
	wire, err := base64.StdEncoding.DecodeString(w.SignedWire)
	if err != nil {
		_, rerr := s.store.RestoreWithdrawal(ctx, w.ID, "corrupt signed wire", s.now())
		if rerr != nil {
			return fmt.Errorf("restore after corrupt wire: %w", rerr)
		}
		return nil
	}
	rctx, cancel := s.withTimeout(ctx)
	defer cancel()
	if _, err := s.rpc.SendTransaction(rctx, wire); err != nil {
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "already been processed"),
			strings.Contains(msg, "already known"),
			strings.Contains(msg, "transaction results in an account already in use"):
			// The transaction landed (or is in flight) — advance to broadcast
			// so the next sweep finalizes it via getSignatureStatuses.
			break
		case strings.Contains(msg, "blockhash not found"),
			strings.Contains(msg, "blockhash expired"),
			strings.Contains(msg, "block has expired"):
			_, rerr := s.store.RestoreWithdrawal(ctx, w.ID, "blockhash_expired", s.now())
			if rerr != nil {
				return fmt.Errorf("restore after blockhash expiry: %w", rerr)
			}
			return nil
		default:
			// Ambiguous (network timeout, 5xx) or a transient preflight error:
			// keep the row signed and retry next sweep. Do NOT restore — the
			// transaction may have landed without our hearing the response.
			return fmt.Errorf("send (will retry): %w", err)
		}
	}
	if _, err := s.store.MarkWithdrawalBroadcast(ctx, w.ID, s.now()); err != nil {
		return fmt.Errorf("mark broadcast: %w", err)
	}
	return nil
}

// finalizeOrRestore queries the on-chain status of the persisted signature.
// Finalized -> finalize; a failed transaction -> restore; an unknown
// signature after blockhash expiry -> restore; otherwise keep waiting.
func (s *Service) finalizeOrRestore(ctx context.Context, w billing.Withdrawal) error {
	if w.Signature == "" {
		return fmt.Errorf("no signature to finalize")
	}
	rctx, cancel := s.withTimeout(ctx)
	defer cancel()
	statuses, err := s.rpc.GetSignatureStatuses(rctx, []string{w.Signature})
	if err != nil {
		// Ambiguous RPC: keep waiting; the blockhash timer still bounds it.
		return fmt.Errorf("signature statuses (will retry): %w", err)
	}
	var st *solana.SignatureStatusValue
	if len(statuses) > 0 {
		st = statuses[0]
	}
	now := s.now()
	switch {
	case st != nil && st.Finalized():
		if _, err := s.store.FinalizeWithdrawal(ctx, w.ID, now); err != nil {
			return fmt.Errorf("finalize: %w", err)
		}
		return nil
	case st != nil && len(st.Status.Err) > 0:
		// The transaction was rejected on chain — return the credit.
		if _, err := s.store.RestoreWithdrawal(ctx, w.ID, "tx_failed", now); err != nil {
			return fmt.Errorf("restore after tx failure: %w", err)
		}
		return nil
	case st == nil && w.LastValidBlockHeight != nil:
		height, err := s.rpc.GetBlockHeight(rctx)
		if err != nil {
			return fmt.Errorf("block height (will retry): %w", err)
		}
		if height > *w.LastValidBlockHeight {
			if _, err := s.store.RestoreWithdrawal(ctx, w.ID, "blockhash_expired_unconfirmed", now); err != nil {
				return fmt.Errorf("restore after blockhash expiry: %w", err)
			}
			return nil
		}
		return fmt.Errorf("pending")
	default:
		// Pending (processed/confirmed but not finalized, or unknown and
		// still within the last-valid-block-height window). Keep waiting.
		return fmt.Errorf("pending")
	}
}

// buildTransferMessage composes the signed-message bytes for a treasury
// withdrawal: it transfers amountRaw OTELA from the treasury's associated
// token account to the destination's associated token account, creating the
// destination account first when it does not exist yet. It is identical in
// shape to the faucet's payout (the same shared wire-format builders), with
// the treasury wallet as the signing authority/fee payer.
func buildTransferMessage(
	treasury, sourceATA, destATA, owner, mint []byte,
	tokenProgram []byte, createATA bool, token2022 bool, blockhash []byte, amountRaw uint64,
) []byte {
	accounts := []solana.AccountMeta{
		{Key: treasury, Signer: true, Writable: true},
		{Key: sourceATA, Writable: true},
		{Key: destATA, Writable: true},
	}
	var ixs []solana.Instruction
	if createATA {
		accounts = append(accounts,
			solana.AccountMeta{Key: owner},
			solana.AccountMeta{Key: mint},
			solana.AccountMeta{Key: solana.SystemProgramID},
			solana.AccountMeta{Key: solana.ATAProgramID},
			solana.AccountMeta{Key: tokenProgram},
		)
		createAccounts := []byte{0, 2, 3, 4, 5, 7}
		if !token2022 {
			accounts = append(accounts, solana.AccountMeta{Key: solana.RentSysvarID})
			createAccounts = append(createAccounts, 8)
		}
		ixs = append(ixs, solana.Instruction{
			ProgramIndex: 6, // ATA program
			Accounts:     createAccounts,
			Data:         solana.CreateATAInstructionData,
		})
	} else {
		accounts = append(accounts, solana.AccountMeta{Key: tokenProgram})
	}
	tokenProgramIndex := byte(3)
	if createATA {
		tokenProgramIndex = 7
	}
	ixs = append(ixs, solana.Instruction{
		ProgramIndex: tokenProgramIndex,
		Accounts:     []byte{1, 2, 0}, // source, destination, authority
		Data:         solana.TransferInstructionData(amountRaw),
	})
	return solana.BuildMessage(accounts, blockhash, ixs)
}

func mustDecodeB58(s string) []byte {
	b, err := solana.DecodeBase58(s, 64)
	if err != nil {
		panic(fmt.Sprintf("withdraw: invalid built-in address %q: %v", s, err))
	}
	return b
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
