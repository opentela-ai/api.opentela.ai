// Package delegations implements the Phase 2 allowance poller (design §11.3):
// it re-reads each linked wallet's OTELA token account on a cadence and
// mirrors SPL delegations to the settlement authority into the registry —
// grants credit the account, revocations (including re-approval to a
// different delegate or a lower cap) debit it clamped at open reservations.
// The reserve gate's min(credit, Σ allowance) bound tracks the chain within
// one refresh window, the same staleness class as the deposit watcher.
//
// Observation semantics (exact, not heuristic): the registry stores the
// ABSOLUTE delegated amount, and every mirror is a delta against it. The
// settlement worker (increment 2) keeps that invariant by decrementing the
// registry when it exercises the delegation on-chain, so a poller read at a
// lower chain amount maps to a revoke only when it exceeds worker
// consumption — see store.ConsumeAllowance.
package delegations

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// allowanceStore is the registry surface the poller needs.
type allowanceStore interface {
	// LinkedWalletAccounts lists every (account, wallet) link to inspect.
	LinkedWalletAccounts(ctx context.Context) ([]billing.WalletAccount, error)
	// UpsertAllowance applies an absolute observation (grant/revoke mirror).
	UpsertAllowance(ctx context.Context, ch billing.AllowanceChange) (billing.AllowanceChangeResult, error)
	// AccountAllowances reports current registry rows, so a missing/moved
	// delegation is only written as a revoke when we actually held one.
	AccountAllowances(ctx context.Context, accountID string) ([]billing.Allowance, error)
}

// chainReader is the RPC surface the poller needs.
type chainReader interface {
	// TokenDelegation reads (delegate, delegatedAmount) for a token account;
	// ok=false when the account does not exist.
	TokenDelegation(ctx context.Context, ata string) (solana.TokenDelegate, bool, error)
}

// Service is the allowance poller.
type Service struct {
	rpc          chainReader
	store        allowanceStore
	authority    string // base58 pubkey of the settlement authority (our delegate)
	mint         string
	tokenProgram string
	refresh      time.Duration
	logf         func(format string, args ...any)

	runOnce sync.Mutex
}

// New builds a poller. authority is the settlement authority's base58 pubkey
// — the delegate buyers approve. mint/tokenProgram identify the OTELA ATA to
// inspect per wallet.
func New(rpc chainReader, store allowanceStore, authority, mint, tokenProgram string, refresh time.Duration) (*Service, error) {
	if authority == "" {
		return nil, fmt.Errorf("delegations: settlement authority is required")
	}
	if mint == "" || tokenProgram == "" {
		return nil, fmt.Errorf("delegations: mint and token program are required")
	}
	if refresh <= 0 {
		refresh = 60 * time.Second
	}
	return &Service{
		rpc:          rpc,
		store:        store,
		authority:    authority,
		mint:         mint,
		tokenProgram: tokenProgram,
		refresh:      refresh,
	}, nil
}

// SetLogger installs a printf-style logger (wired to the standard logger).
func (s *Service) SetLogger(fn func(format string, args ...any)) {
	s.logf = fn
}

func (s *Service) logfOrDefault(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Run drives the poll loop until ctx is cancelled. Ticks never overlap:
// RunOnce holds a mutex, so a slow RPC pass delays the next tick instead of
// stacking observations.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.RunOnce(ctx); err != nil {
				s.logfOrDefault("poll pass failed: %v", err)
			}
		}
	}
}

// RunOnce performs one pass over every linked wallet and returns the number
// of registry writes applied (grants + revocations). Per-wallet errors are
// logged and skipped — one dead RPC response must not stall the others.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	s.runOnce.Lock()
	defer s.runOnce.Unlock()

	links, err := s.store.LinkedWalletAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("delegations: list wallet links: %w", err)
	}
	applied := 0
	for _, link := range links {
		select {
		case <-ctx.Done():
			return applied, ctx.Err()
		default:
		}
		wrote, err := s.observeWallet(ctx, link)
		if err != nil {
			s.logfOrDefault("wallet %s (account %s): %v", link.Wallet, link.AccountID, err)
			continue
		}
		applied += wrote
	}
	return applied, nil
}

// observeWallet reads the wallet's OTELA ATA and mirrors the delegation
// state. Returns 1 when a registry write was applied, 0 otherwise.
func (s *Service) observeWallet(ctx context.Context, link billing.WalletAccount) (int, error) {
	walletBytes, err := solana.DecodeBase58(link.Wallet, solana.PublicKeyBytes)
	if err != nil {
		return 0, fmt.Errorf("linked wallet is not valid base58: %w", err)
	}
	mintBytes, err := solana.DecodeBase58(s.mint, solana.PublicKeyBytes)
	if err != nil {
		return 0, fmt.Errorf("mint is not valid base58: %w", err)
	}
	tpBytes, err := solana.DecodeBase58(s.tokenProgram, solana.PublicKeyBytes)
	if err != nil {
		return 0, fmt.Errorf("token program is not valid base58: %w", err)
	}
	ataBytes, err := solana.AssociatedTokenAddress(walletBytes, mintBytes, tpBytes)
	if err != nil {
		return 0, fmt.Errorf("derive ATA: %w", err)
	}
	ata := solana.EncodeBase58(ataBytes)

	td, ok, err := s.rpc.TokenDelegation(ctx, ata)
	if err != nil {
		return 0, fmt.Errorf("read token delegation: %w", err)
	}

	if ok && td.Delegate == s.authority {
		// Live delegation to us: grant, raise, or lower — the delta math in
		// UpsertAllowance handles all three from the absolute amount.
		if _, err := s.store.UpsertAllowance(ctx, billing.AllowanceChange{
			AccountID:    link.AccountID,
			Delegate:     s.authority,
			AllowanceRaw: int64(td.DelegatedAmountRaw),
			ObservedAt:   time.Now().UTC(),
			Ref:          fmt.Sprintf("obs:%s:%d", link.Wallet, td.Slot),
		}); err != nil {
			return 0, fmt.Errorf("mirror delegation: %w", err)
		}
		return 1, nil
	}

	// No ATA, no delegate, or delegated elsewhere. Only a revoke when the
	// registry still holds an active row for us — otherwise this is the
	// steady state of an account that never delegated and writing zeros
	// every poll would spam the registry.
	rows, err := s.store.AccountAllowances(ctx, link.AccountID)
	if err != nil {
		return 0, fmt.Errorf("read registry: %w", err)
	}
	for _, a := range rows {
		if a.Delegate == s.authority && a.Active() {
			if _, err := s.store.UpsertAllowance(ctx, billing.AllowanceChange{
				AccountID:    link.AccountID,
				Delegate:     s.authority,
				AllowanceRaw: 0,
				ObservedAt:   time.Now().UTC(),
				Ref:          fmt.Sprintf("obs:%s:%d", link.Wallet, td.Slot),
			}); err != nil {
				return 0, fmt.Errorf("mirror revocation: %w", err)
			}
			return 1, nil
		}
	}
	return 0, nil
}
