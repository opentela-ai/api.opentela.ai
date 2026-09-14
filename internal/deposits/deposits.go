// Package deposits watches the Solana treasury associated token account for
// inbound SPL transfers and credits the linked owner's account. Every
// transfer instruction is persisted (internal/store) BEFORE any credit is
// applied, so a crash mid-watch can never lose or double-count a deposit.
// Transfers from wallets that are not yet linked are recorded 'unassigned'
// and reconciled automatically once the wallet is linked
// (store.ReconcileDepositsForWallet).
//
// The watcher is read-only with respect to the chain: it polls
// getSignaturesForAddress at finalized commitment and credits only
// finalized, successful (err == null) transfers of the configured mint into
// the dedicated treasury ATA. It never accepts account ids from the chain or
// a memo — attribution is resolved solely against linked wallets.
package deposits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// signaturesReader is the subset of solana.RPCClient the watcher needs.
type signaturesReader interface {
	GetSignaturesForAddress(ctx context.Context, address string, limit int, before, commitment string) ([]solana.SignatureInfo, error)
	GetTransaction(ctx context.Context, signature string) (*solana.ConfirmedTransaction, error)
}

// depositStore is the subset of the billing store the watcher needs.
type depositStore interface {
	InsertDepositEvent(ctx context.Context, ev billing.DepositEvent) (bool, error)
	ApplyDeposit(ctx context.Context, signature string, instructionIndex int, accountID string, now time.Time) (billing.DepositEvent, error)
	DepositCursor(ctx context.Context, treasuryATA string) (string, error)
	AdvanceDepositCursor(ctx context.Context, treasuryATA, fromSignature, toSignature string) (bool, error)
}

// walletLinker resolves the owning account for a wallet pubkey, so the
// watcher can credit linked wallets immediately and leave unlinked ones for
// later reconciliation.
type walletLinker interface {
	AccountForWallet(ctx context.Context, wallet string) (string, bool, error)
}

// Service polls the treasury ATA for finalized inbound transfers and credits
// linked accounts. It is safe to construct with New; Run blocks until ctx is
// canceled. RunOnce performs a single pass and is the unit test entry point.
type Service struct {
	rpc          signaturesReader
	store        depositStore
	linker       walletLinker
	treasuryATA  string // base58, the account watched by getSignaturesForAddress
	mint         string // base58, filters transferChecked by mint (ATA filter covers plain transfers)
	tokenProgram string // base58, filters which transfer instructions count
	pollLimit    int
	pollInterval time.Duration
	lastSig      string // last persisted high-water signature loaded/advanced this pass
	now          func() time.Time
	log          func(format string, args ...any)
}

// New builds a Service. treasuryWallet is the wallet that owns the treasury
// ATA (deposits arrive at its associated token account for mint/tokenProgram).
// The treasury ATA is derived via solana.AssociatedTokenAddress. pollInterval
// is the cadence between passes (the watcher also re-polls immediately when
// a full page of new signatures arrives, to keep up with bursts).
func New(rpc signaturesReader, store depositStore, linker walletLinker, treasuryWallet, mint, tokenProgram string, pollInterval time.Duration) (*Service, error) {
	if rpc == nil || store == nil || linker == nil {
		return nil, fmt.Errorf("deposits: nil dependency")
	}
	if err := validateBase58(treasuryWallet, "treasury wallet"); err != nil {
		return nil, err
	}
	if err := validateBase58(mint, "mint"); err != nil {
		return nil, err
	}
	if err := validateBase58(tokenProgram, "token program"); err != nil {
		return nil, err
	}
	tpBytes, err := solana.DecodeBase58(tokenProgram, solana.PublicKeyBytes)
	if err != nil || len(tpBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("deposits: invalid token program: %w", err)
	}
	mintBytes, err := solana.DecodeBase58(mint, solana.PublicKeyBytes)
	if err != nil || len(mintBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("deposits: invalid mint: %w", err)
	}
	treasuryBytes, err := solana.DecodeBase58(treasuryWallet, solana.PublicKeyBytes)
	if err != nil || len(treasuryBytes) != solana.PublicKeyBytes {
		return nil, fmt.Errorf("deposits: invalid treasury wallet: %w", err)
	}
	ata, err := solana.AssociatedTokenAddress(treasuryBytes, mintBytes, tpBytes)
	if err != nil {
		return nil, fmt.Errorf("deposits: derive treasury ATA: %w", err)
	}
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	return &Service{
		rpc:          rpc,
		store:        store,
		linker:       linker,
		treasuryATA:  solana.EncodeBase58(ata),
		mint:         mint,
		tokenProgram: tokenProgram,
		pollLimit:    100,
		pollInterval: pollInterval,
		now:          time.Now,
		log:          func(string, ...any) {},
	}, nil
}

// SetLogger installs a printf-style logger (default is silent).
func (s *Service) SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		s.log = fn
	}
}

// TreasuryATA returns the derived associated token account the watcher polls.
func (s *Service) TreasuryATA() string { return s.treasuryATA }

// Run polls the treasury ATA at pollInterval until ctx is canceled. Transient
// RPC errors are logged and the loop backs off one interval; they never
// terminate the worker. When a full page of new signatures arrives, the next
// pass runs immediately (no sleep) so a burst is drained promptly.
func (s *Service) Run(ctx context.Context) {
	s.log("deposits: watching %s (mint %s, poll %s)", s.treasuryATA, s.mint, s.pollInterval)
	t := time.NewTicker(s.pollInterval)
	defer t.Stop()
	for {
		n, err := s.RunOnce(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			// Always back off after an error, even if a partial page was
			// examined, so a persistent RPC outage does not busy-loop.
			s.log("deposits: poll error: %v", err)
		} else if n >= s.pollLimit {
			// Drain bursts: a full page means more signatures may be
			// available — re-poll immediately without sleeping.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce performs a single polling pass: fetch the newest page of finalized
// signatures, page backward (newest → oldest) until reaching the persisted
// high-water mark or exhausting the page, then process the candidate span
// oldest → newest. The cursor advances only across the contiguous successful
// prefix, so transient transaction/store/linker failures are retried and never
// skipped. Returns the count of signatures examined this pass.
//
// On the very first pass (no persisted cursor) only the newest page is
// processed; older history is not replayed (devnet treasuries are fresh, and
// bounding startup cost matters more than retroactive catch-up).
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	examined := 0
	lastSig, err := s.store.DepositCursor(ctx, s.treasuryATA)
	if err != nil {
		return 0, fmt.Errorf("load cursor: %w", err)
	}
	s.lastSig = lastSig
	firstPass := lastSig == ""
	before := ""
	var pending []solana.SignatureInfo
	for {
		sigs, err := s.rpc.GetSignaturesForAddress(ctx, s.treasuryATA, s.pollLimit, before, "finalized")
		if err != nil {
			return examined, fmt.Errorf("signatures: %w", err)
		}
		if len(sigs) == 0 {
			return examined, nil
		}
		stop := false
		for _, sig := range sigs {
			if sig.Signature == lastSig {
				stop = true
				break
			}
			pending = append(pending, sig)
			examined++
		}
		if stop || len(sigs) < s.pollLimit || firstPass {
			break
		}
		// Page backward: the oldest signature of this page becomes `before`.
		before = sigs[len(sigs)-1].Signature
	}
	advanceBlocked := false
	cursor := lastSig
	for i := len(pending) - 1; i >= 0; i-- {
		sig := pending[i]
		if err := s.processSignature(ctx, sig); err != nil {
			s.log("deposits: %s: %v", sig.Signature, err)
			advanceBlocked = true
			continue
		}
		if advanceBlocked {
			continue
		}
		advanced, err := s.store.AdvanceDepositCursor(ctx, s.treasuryATA, cursor, sig.Signature)
		if err != nil {
			return examined, fmt.Errorf("advance cursor %q -> %q: %w", cursor, sig.Signature, err)
		}
		if !advanced {
			fresh, err := s.store.DepositCursor(ctx, s.treasuryATA)
			if err != nil {
				return examined, fmt.Errorf("reload cursor: %w", err)
			}
			s.lastSig = fresh
			return examined, nil
		}
		cursor = sig.Signature
		s.lastSig = cursor
	}
	return examined, nil
}

// processSignature fetches one finalized transaction, extracts inbound SPL
// Token transfers into the treasury ATA, persists each (idempotent), and
// credits the linked owner. Failed transactions (err != null) are skipped
// without crediting — they never move tokens. Unknown-wallet transfers are
// recorded 'unassigned' for later reconciliation.
func (s *Service) processSignature(ctx context.Context, sig solana.SignatureInfo) error {
	if sig.Err != nil {
		// Failed transaction: nothing to persist or credit. Advancing the
		// high-water mark past it (done in RunOnce) prevents re-querying.
		return nil
	}
	tx, err := s.rpc.GetTransaction(ctx, sig.Signature)
	if err != nil {
		return fmt.Errorf("get transaction: %w", err)
	}
	if tx == nil {
		return fmt.Errorf("get transaction: missing finalized transaction")
	}
	// s.mint is enforced for transferChecked (which carries the mint on the
	// instruction); plain transfers are attributed via the treasury ATA
	// filter, which is mint-unique by construction.
	transfers := tx.TokenTransfers(s.mint, s.tokenProgram)
	if len(transfers) == 0 {
		// Make the silent-skip class of bug observable: a signature that
		// touched the treasury but contains no inbound transfer into the
		// treasury ATA (wrong destination, misrouted nested account, etc.).
		s.log("deposits: %s: no inbound transfer into treasury; skipped", sig.Signature)
	}
	for _, tr := range transfers {
		if tr.Destination != s.treasuryATA {
			continue // not an inbound deposit into the treasury ATA
		}
		fromWallet := tx.SenderWallet(tr.Source, tr.Authority)
		ev := billing.DepositEvent{
			TransactionSignature: sig.Signature,
			InstructionIndex:     tr.InstructionIndex,
			Slot:                 sig.Slot,
			FromWallet:           fromWallet,
			AmountRaw:            tr.AmountRaw,
		}
		inserted, err := s.store.InsertDepositEvent(ctx, ev)
		if err != nil {
			return fmt.Errorf("insert deposit %d: %w", tr.InstructionIndex, err)
		}
		if !inserted {
			s.log("deposits: %s: transfer %d already persisted; retrying attribution", sig.Signature, tr.InstructionIndex)
		}
		accountID, ok, err := s.linker.AccountForWallet(ctx, fromWallet)
		if err != nil {
			return fmt.Errorf("lookup wallet %s: %w", fromWallet, err)
		}
		if !ok {
			s.log("deposits: %s: wallet %s not linked; leaving unassigned", sig.Signature, fromWallet)
			continue // reconciled automatically when the wallet is linked
		}
		if _, err := s.store.ApplyDeposit(ctx, ev.TransactionSignature, ev.InstructionIndex, accountID, s.now()); err != nil {
			return fmt.Errorf("apply to %s: %w", accountID, err)
		}
		s.log("deposits: %s: credited %d to %s from %s", sig.Signature, ev.AmountRaw, accountID, fromWallet)
	}
	return nil
}

// validateBase58 ensures v decodes to a 32-byte Solana public key.
func validateBase58(v, name string) error {
	if v == "" {
		return fmt.Errorf("deposits: %s is required", name)
	}
	b, err := solana.DecodeBase58(v, solana.PublicKeyBytes)
	if err != nil || len(b) != solana.PublicKeyBytes {
		return fmt.Errorf("deposits: invalid %s: %w", name, err)
	}
	return nil
}
