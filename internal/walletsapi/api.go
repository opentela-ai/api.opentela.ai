package walletsapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

const (
	maxBodyBytes = 4 << 10
	challengeTTL = 5 * time.Minute
)

type Service struct {
	store          walletStore
	reconciler     Reconciler
	identityMaxAge time.Duration
	now            func() time.Time
	logf           func(format string, args ...any)
}

type walletResponse struct {
	ID        int64     `json:"id"`
	Wallet    string    `json:"wallet"`
	Primary   bool      `json:"primary"`
	CreatedAt time.Time `json:"created_at"`
}

type identitySnapshot struct {
	Email          string     `json:"email"`
	EmailDomain    string     `json:"email_domain"`
	EmailVerified  bool       `json:"email_verified"`
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	MaxAgeSeconds  int        `json:"max_age_seconds"`
}

type listResponse struct {
	Wallets  []walletResponse `json:"wallets"`
	Identity identitySnapshot `json:"identity"`
}

type walletStore interface {
	ListWalletsByUser(ctx context.Context, accountID string) ([]store.WalletInfo, error)
	GetIdentity(ctx context.Context, accountID string) (store.IdentityInfo, error)
	CreateWalletChallenge(ctx context.Context, ch store.WalletChallenge) error
	GetWalletChallenge(ctx context.Context, accountID, id string) (store.WalletChallenge, error)
	ConsumeWalletChallenge(ctx context.Context, accountID, id string, now time.Time) error
	LinkWallet(ctx context.Context, accountID, wallet string) (store.WalletInfo, error)
	DeleteWalletByIDForUser(ctx context.Context, accountID string, id int64) (bool, error)
	GetUserWalletSet(ctx context.Context, accountID string) ([]string, error)
}

// Reconciler credits a wallet's previously-unassigned deposits once it is
// linked. The deposit watcher records transfers from unknown wallets as
// 'unassigned'; this hook, invoked right after LinkWallet with the just-
// linked accountID, sweeps them into the account so users are credited for
// deposits sent before linking. It is optional: when nil, unassigned
// deposits remain pending until a later manual reconciliation.
type Reconciler interface {
	ReconcileDepositsForWallet(ctx context.Context, wallet, accountID string) (int, error)
}

func New(store walletStore, identityMaxAge time.Duration) *Service {
	return &Service{store: store, identityMaxAge: identityMaxAge, now: time.Now, logf: log.Printf}
}

// WithReconciler installs a deposit reconciler (Step 5) so newly-linked
// wallets are credited for pre-linkage deposits. Chainable; nil is a no-op.
func (s *Service) WithReconciler(r Reconciler) *Service {
	s.reconciler = r
	return s
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manage/wallets", s.handleList)
	mux.HandleFunc("POST /manage/wallets/challenges", s.handleChallenge)
	mux.HandleFunc("POST /manage/wallets", s.handleLink)
	mux.HandleFunc("DELETE /manage/wallets/{id}", s.handleDelete)
	return mux
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	wallets, err := s.store.ListWalletsByUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	out := make([]walletResponse, 0, len(wallets))
	for _, wallet := range wallets {
		out = append(out, walletResponse{
			ID: wallet.ID, Wallet: wallet.Wallet, Primary: wallet.Primary, CreatedAt: wallet.CreatedAt,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, listResponse{
		Wallets:  out,
		Identity: s.identitySnapshot(r.Context(), userID),
	})
}

type challengeRequest struct {
	Wallet string `json:"wallet"`
}

type challengeResponse struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) handleChallenge(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	var req challengeRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	wallet, err := solana.NormalizeWallet(req.Wallet)
	if err != nil {
		http.Error(w, "invalid wallet", http.StatusBadRequest)
		return
	}
	now := s.now().UTC()
	id, err := solana.NewChallengeID()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	nonce, err := solana.NewChallengeNonce()
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	expiresAt := now.Add(challengeTTL)
	message := solana.BuildChallengeMessage(userID, wallet, nonce, now, expiresAt)
	if err := s.store.CreateWalletChallenge(r.Context(), store.WalletChallenge{
		ID: id, AccountID: userID, Wallet: wallet, Nonce: nonce, Message: message, IssuedAt: now, ExpiresAt: expiresAt,
	}); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, challengeResponse{ID: id, Message: message, ExpiresAt: expiresAt})
}

type linkRequest struct {
	ChallengeID string `json:"challenge_id"`
	Signature   string `json:"signature"`
}

func (s *Service) handleLink(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	var req linkRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil || req.ChallengeID == "" || req.Signature == "" {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ch, err := s.store.GetWalletChallenge(r.Context(), userID, req.ChallengeID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "challenge not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := solana.VerifySignature(ch.Wallet, ch.Message, req.Signature); err != nil {
		http.Error(w, "signature verification failed", http.StatusUnprocessableEntity)
		return
	}
	now := s.now().UTC()
	if err := s.store.ConsumeWalletChallenge(r.Context(), userID, req.ChallengeID, now); err != nil {
		switch {
		case errors.Is(err, store.ErrChallengeExpired):
			http.Error(w, "challenge expired", http.StatusConflict)
		case errors.Is(err, store.ErrChallengeConsumed):
			http.Error(w, "challenge already used", http.StatusConflict)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "challenge not found", http.StatusNotFound)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	info, err := s.store.LinkWallet(r.Context(), userID, ch.Wallet)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrWalletAlreadyLinked):
			http.Error(w, "account already has a linked wallet; one account operates a single wallet for all its peers", http.StatusConflict)
		case errors.Is(err, store.ErrWalletOtherAccount):
			http.Error(w, "wallet already linked to another account", http.StatusConflict)
		case errors.Is(err, store.ErrConflict):
			http.Error(w, "wallet already linked", http.StatusConflict)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	// Step 5: credit any deposits this wallet received before it was linked.
	// The link itself already succeeded, so reconciliation failures are
	// logged and never reported to the user — a later poll or retry settles
	// them. Reconciliation runs in the request context so it is canceled if
	// the client disconnects; a subsequent deposit watcher poll or a retry
	// on the next link attempt picks up anything left uncredited.
	if s.reconciler != nil {
		if n, rerr := s.reconciler.ReconcileDepositsForWallet(r.Context(), ch.Wallet, userID); rerr != nil {
			s.logf("reconcile deposits for %s: %v", ch.Wallet, rerr)
		} else if n > 0 {
			s.logf("credited %d pre-linkage deposit(s) to %s", n, ch.Wallet)
		}
	}
	httputil.WriteJSON(w, http.StatusCreated, walletResponse{
		ID: info.ID, Wallet: info.Wallet, Primary: info.Primary, CreatedAt: info.CreatedAt,
	})
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid wallet id", http.StatusBadRequest)
		return
	}
	changed, err := s.store.DeleteWalletByIDForUser(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, store.ErrWalletInUse) {
			http.Error(w, "wallet is still proving an active instance", http.StatusConflict)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !changed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func UserWalletSet(ctx context.Context, pg interface {
	GetUserWalletSet(ctx context.Context, accountID string) ([]string, error)
}, accountID string) (map[string]struct{}, error) {
	wallets, err := pg.GetUserWalletSet(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(wallets))
	for _, wallet := range wallets {
		out[wallet] = struct{}{}
	}
	return out, nil
}

func (s *Service) identitySnapshot(ctx context.Context, accountID string) identitySnapshot {
	out := identitySnapshot{MaxAgeSeconds: int(s.identityMaxAge / time.Second)}
	info, err := s.store.GetIdentity(ctx, accountID)
	if err != nil {
		return out
	}
	out.Email = info.Email
	out.EmailDomain = info.EmailDomain
	out.EmailVerified = info.EmailVerified
	out.LastVerifiedAt = &info.LastVerifiedAt
	if s.identityMaxAge > 0 {
		expiresAt := info.LastVerifiedAt.Add(s.identityMaxAge)
		out.ExpiresAt = &expiresAt
	}
	return out
}
