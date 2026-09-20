// Package faucetapi exposes the OTELA faucet to authenticated accounts: a
// status endpoint and a one-time claim endpoint. Claims require a Neon Auth
// session whose email is verified and at least one linked wallet.
package faucetapi

import (
	"context"
	"errors"
	"log"
	"math/big"
	"net/http"
	"time"

	"github.com/opentela-ai/api/internal/faucet"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

type Service struct {
	store     faucetStore
	faucet    *faucet.Service // nil when the faucet is disabled
	amountRaw uint64
	decimals  int
}

type faucetStore interface {
	GetFaucetClaim(ctx context.Context, accountID string) (store.FaucetClaim, error)
	ClaimFaucet(ctx context.Context, accountID, wallet, txSignature string, amountRaw int64) (bool, error)
	CompleteFaucetClaim(ctx context.Context, accountID, txSignature string) error
	ClearFaucetClaim(ctx context.Context, accountID string) error
	ListWalletsByUser(ctx context.Context, accountID string) ([]store.WalletInfo, error)
}

// New builds the faucet handler. A nil faucet service means the faucet is
// disabled: the status endpoint reports enabled=false and claim returns 404.
func New(pg faucetStore, svc *faucet.Service, amountRaw uint64, decimals int) *Service {
	return &Service{store: pg, faucet: svc, amountRaw: amountRaw, decimals: decimals}
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manage/faucet", s.handleStatus)
	mux.HandleFunc("POST /manage/faucet/claim", s.handleClaim)
	return mux
}

type statusResponse struct {
	Enabled       bool       `json:"enabled"`
	EmailVerified bool       `json:"email_verified"`
	Mint          string     `json:"mint"`
	AmountRaw     uint64     `json:"amount_raw"`
	AmountUI      string     `json:"amount_ui"`
	Decimals      int        `json:"decimals"`
	Claimed       bool       `json:"claimed"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	Wallet        string     `json:"wallet,omitempty"`
	TxSignature   string     `json:"tx_signature,omitempty"`
	// Funded reports whether the faucet wallet can afford one payout right
	// now (nil when the check could not run, e.g. the Solana RPC is down).
	Funded *bool `json:"funded,omitempty"`
	// FaucetWallet is the on-chain address that pays claims; funding goes here.
	FaucetWallet string `json:"faucet_wallet,omitempty"`
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	out := statusResponse{
		Enabled:       s.faucet != nil,
		EmailVerified: s.emailVerified(r),
		Mint:          s.mint(),
		AmountRaw:     s.amountRaw,
		AmountUI:      formatAmount(s.amountRaw, s.decimals),
		Decimals:      s.decimals,
	}
	if claim, err := s.store.GetFaucetClaim(r.Context(), userID); err == nil && claim.TxSignature != "" {
		out.Claimed = true
		out.ClaimedAt = &claim.ClaimedAt
		out.Wallet = claim.Wallet
		out.TxSignature = claim.TxSignature
	}
	if s.faucet != nil {
		out.FaucetWallet = s.faucet.FaucetWallet()
		if check, err := s.faucet.CheckFunded(r.Context()); err != nil {
			log.Printf("faucet: status fund check failed: %v", err)
		} else {
			out.Funded = &check.Funded
		}
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

type claimResponse struct {
	Status      string `json:"status"`
	Wallet      string `json:"wallet"`
	AmountRaw   uint64 `json:"amount_raw"`
	AmountUI    string `json:"amount_ui"`
	TxSignature string `json:"tx_signature"`
}

func (s *Service) handleClaim(w http.ResponseWriter, r *http.Request) {
	if s.faucet == nil {
		http.Error(w, "faucet disabled", http.StatusNotFound)
		return
	}
	if !s.emailVerified(r) {
		http.Error(w, "email must be verified to claim from the faucet", http.StatusForbidden)
		return
	}
	// Affordability pre-check: fail fast with a clear message when the faucet
	// wallet cannot pay for the transfer, instead of reserving a claim row and
	// then rolling it back after the on-chain send fails. An RPC hiccup here
	// is logged but not fatal — the send path still guards the same failure.
	if check, err := s.faucet.CheckFunded(r.Context()); err != nil {
		log.Printf("faucet: fund pre-check failed: %v", err)
	} else if !check.Funded {
		log.Printf(
			"faucet: wallet %s underfunded: token %d/%d raw, lamports %d/%d",
			s.faucet.FaucetWallet(), check.TokenRaw, check.NeededTokenRaw,
			check.Lamports, check.NeededLamports,
		)
		http.Error(w, "faucet wallet underfunded — the operator must top it up", http.StatusServiceUnavailable)
		return
	}
	userID, _ := principal.UserID(r.Context())
	wallet, err := s.primaryWallet(r.Context(), userID)
	if err != nil || wallet == "" {
		http.Error(w, "link a wallet before claiming from the faucet", http.StatusConflict)
		return
	}
	if claim, err := s.store.GetFaucetClaim(r.Context(), userID); err == nil && claim.TxSignature != "" {
		http.Error(w, "already claimed", http.StatusConflict)
		return
	}

	// Phase 1: reserve the claim with a pending (empty) tx_signature. The
	// unique constraint on account_id prevents a concurrent request from
	// reaching the on-chain send below.
	reserved, err := s.store.ClaimFaucet(r.Context(), userID, wallet, "", int64(s.amountRaw))
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !reserved {
		http.Error(w, "already claimed", http.StatusConflict)
		return
	}

	// Phase 2: broadcast the on-chain transfer.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sig, err := s.faucet.Send(ctx, wallet)
	if err != nil {
		log.Printf("faucet: send failed for %s wallet %s: %v", userID, wallet, err)
		// Roll back the pending reservation so the account can retry.
		if clearErr := s.store.ClearFaucetClaim(r.Context(), userID); clearErr != nil {
			// The pending row remains; the next claim attempt will refresh it.
			log.Printf("faucet: clear pending claim for %s: %v", userID, clearErr)
		}
		http.Error(w, "faucet payout failed", http.StatusServiceUnavailable)
		return
	}

	// Phase 3: complete the claim with the on-chain signature.
	if err := s.store.CompleteFaucetClaim(r.Context(), userID, sig); err != nil {
		// The transaction landed on-chain but we could not record it. Log
		// for manual reconciliation; the user should not be able to claim
		// again because the row still exists (pending).
		log.Printf("faucet: complete claim for %s (sig %s): %v", userID, sig, err)
		http.Error(w, "faucet payout sent but not recorded", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, claimResponse{
		Status:      "claimed",
		Wallet:      wallet,
		AmountRaw:   s.amountRaw,
		AmountUI:    formatAmount(s.amountRaw, s.decimals),
		TxSignature: sig,
	})
}

func (s *Service) emailVerified(r *http.Request) bool {
	p, ok := principal.FromContext(r.Context())
	return ok && p.EmailVerified
}

func (s *Service) primaryWallet(ctx context.Context, accountID string) (string, error) {
	wallets, err := s.store.ListWalletsByUser(ctx, accountID)
	if err != nil {
		return "", err
	}
	for _, w := range wallets {
		if w.Primary {
			return w.Wallet, nil
		}
	}
	if len(wallets) > 0 {
		return wallets[0].Wallet, nil
	}
	return "", errors.New("faucet: no linked wallet")
}

func (s *Service) mint() string {
	if s.faucet == nil {
		return ""
	}
	return s.faucet.Mint()
}

// formatAmount renders base units as a decimal string with the token's
// decimals, e.g. 1000000000 @ 9 decimals -> "1".
func formatAmount(raw uint64, decimals int) string {
	amount := new(big.Int).SetUint64(raw)
	if decimals == 0 {
		return amount.String()
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole := new(big.Int).Div(amount, pow)
	frac := new(big.Int).Mod(amount, pow)
	if frac.Sign() == 0 {
		return whole.String()
	}
	fracStr := frac.String()
	for len(fracStr) < decimals {
		fracStr = "0" + fracStr
	}
	fracStr = trimTrailingZeros(fracStr)
	if fracStr == "" {
		return whole.String()
	}
	return whole.String() + "." + fracStr
}

func trimTrailingZeros(s string) string {
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	return s[:i]
}
