package aclapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

const (
	maxPeerBatch             = 100
	controlAuthFailureHeader = "X-Otela-Control-Auth-Failed"
)

type Service struct {
	store           aclStore
	mesh            peerLookup
	internalDigest  [32]byte
	identityMaxAge  time.Duration
	ownershipMaxAge time.Duration
	cacheTTL        time.Duration
	now             func() time.Time
}

type aclStore interface {
	LookupActiveKey(ctx context.Context, keyHash string) (store.ActiveKey, error)
	ListManagedInstancesByPeerIDs(ctx context.Context, peerIDs []string) ([]store.InstanceInfo, error)
	GetIdentity(ctx context.Context, accountID string) (store.IdentityInfo, error)
	GetUserWalletSet(ctx context.Context, accountID string) ([]string, error)
	ListWalletsByUser(ctx context.Context, accountID string) ([]store.WalletInfo, error)
}

type peerLookup interface {
	LookupPeers(ctx context.Context, peerIDs []string) (map[string]mesh.PeerObservation, error)
}

type evaluateRequest struct {
	KeyHash string   `json:"key_hash"`
	PeerIDs []string `json:"peer_ids"`
}

type deniedPeer struct {
	PeerID string `json:"peer_id"`
	Reason string `json:"reason"`
}

type evaluateResponse struct {
	KeyID           string       `json:"key_id"`
	AllowedPeerIDs  []string     `json:"allowed_peer_ids"`
	Denied          []deniedPeer `json:"denied"`
	PrimaryWallet   string       `json:"primary_wallet"`
	CacheTTLSeconds int          `json:"cache_ttl_seconds"`
}

func New(pg aclStore, meshClient peerLookup, token string, identityMaxAge, ownershipMaxAge, cacheTTL time.Duration) *Service {
	return &Service{
		store:           pg,
		mesh:            meshClient,
		internalDigest:  sha256.Sum256([]byte(token)),
		identityMaxAge:  identityMaxAge,
		ownershipMaxAge: ownershipMaxAge,
		cacheTTL:        cacheTTL,
		now:             time.Now,
	}
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			w.Header().Set(controlAuthFailureHeader, "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req evaluateRequest
		if err := httputil.DecodeStrict(w, r, 32<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.KeyHash) != 64 || strings.ToLower(req.KeyHash) != req.KeyHash {
			http.Error(w, "invalid key hash", http.StatusBadRequest)
			return
		}
		if _, err := hex.DecodeString(req.KeyHash); err != nil {
			http.Error(w, "invalid key hash", http.StatusBadRequest)
			return
		}
		peerIDs := dedupePeers(req.PeerIDs)
		if len(peerIDs) == 0 || len(peerIDs) > maxPeerBatch {
			http.Error(w, "invalid peer_ids", http.StatusBadRequest)
			return
		}
		resp, status, err := s.evaluate(r.Context(), req.KeyHash, peerIDs)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, resp)
	})
}

func (s *Service) evaluate(ctx context.Context, keyHash string, peerIDs []string) (evaluateResponse, int, error) {
	key, err := s.store.LookupActiveKey(ctx, keyHash)
	if errors.Is(err, store.ErrNotFound) {
		return evaluateResponse{}, http.StatusUnauthorized, errors.New("unauthorized")
	}
	if err != nil {
		return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	managed, err := s.store.ListManagedInstancesByPeerIDs(ctx, peerIDs)
	if err != nil {
		return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	managedByPeer := make(map[string]store.InstanceInfo, len(managed))
	managedPeerIDs := make([]string, 0, len(managed))
	for _, inst := range managed {
		managedByPeer[inst.PeerID] = inst
		managedPeerIDs = append(managedPeerIDs, inst.PeerID)
	}

	resp := evaluateResponse{
		KeyID:           strconv.FormatInt(key.KeyID, 10),
		AllowedPeerIDs:  make([]string, 0, len(peerIDs)),
		Denied:          make([]deniedPeer, 0, len(peerIDs)),
		CacheTTLSeconds: int(s.cacheTTL / time.Second),
	}

	var (
		identity        store.IdentityInfo
		identityErr     error
		identityLoaded  bool
		walletSet       map[string]struct{}
		walletSetErr    error
		walletSetLoaded bool
	)
	loadIdentity := func() (store.IdentityInfo, error) {
		if !identityLoaded {
			identityLoaded = true
			if key.UserID == nil {
				return identity, nil
			}
			identity, identityErr = s.store.GetIdentity(ctx, *key.UserID)
			if errors.Is(identityErr, store.ErrNotFound) {
				identityErr = nil
			}
		}
		return identity, identityErr
	}
	loadWalletSet := func() (map[string]struct{}, error) {
		if !walletSetLoaded {
			walletSetLoaded = true
			if key.UserID == nil {
				walletSet = map[string]struct{}{}
				return walletSet, nil
			}
			wallets, loadErr := s.store.GetUserWalletSet(ctx, *key.UserID)
			walletSetErr = loadErr
			walletSet = make(map[string]struct{}, len(wallets))
			for _, wallet := range wallets {
				walletSet[wallet] = struct{}{}
			}
		}
		return walletSet, walletSetErr
	}

	now := s.now().UTC()
	observations := map[string]mesh.PeerObservation{}
	if len(managedPeerIDs) > 0 {
		observations, err = s.mesh.LookupPeers(ctx, managedPeerIDs)
		if err != nil {
			return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
		}
	}
	for _, peerID := range peerIDs {
		inst, ok := managedByPeer[peerID]
		if !ok {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
			continue
		}
		obs, ok := observations[peerID]
		if !ok {
			return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
		}
		if !s.observationFresh(obs) {
			return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
		}
		if obs.Wallet != inst.OwnerWallet {
			resp.Denied = append(resp.Denied, deniedPeer{PeerID: peerID, Reason: "ownership_mismatch"})
			continue
		}
		if inst.AccessMode == "public" {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
			continue
		}
		if key.UserID != nil && *key.UserID == inst.AccountID {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
			continue
		}
		matched := false
		var enrichmentErr error
		for _, rule := range inst.Rules {
			if rule.Kind != "wallet" {
				continue
			}
			wallets, loadErr := loadWalletSet()
			if loadErr != nil {
				enrichmentErr = loadErr
				break
			}
			if _, ok := wallets[rule.Value]; ok {
				matched = true
				break
			}
		}
		if !matched {
			for _, rule := range inst.Rules {
				if rule.Kind != "email_domain" {
					continue
				}
				principalIdentity, loadErr := loadIdentity()
				if loadErr != nil {
					enrichmentErr = loadErr
					break
				}
				if principalIdentity.EmailVerified && principalIdentity.EmailDomain == rule.Value && !principalIdentity.LastVerifiedAt.IsZero() && now.Sub(principalIdentity.LastVerifiedAt) <= s.identityMaxAge {
					matched = true
					break
				}
			}
		}
		if matched {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
			continue
		}
		if enrichmentErr != nil {
			return evaluateResponse{}, http.StatusServiceUnavailable, errors.New("service unavailable")
		}
		resp.Denied = append(resp.Denied, deniedPeer{PeerID: peerID, Reason: "no_match"})
	}
	// Primary wallet propagation is optional response metadata. A failure to
	// load it must not turn an otherwise conclusive public, owner, or ACL
	// decision into a control-plane outage; downstream local ACLs fail closed
	// when the value is absent.
	if key.UserID != nil {
		if listed, listErr := s.store.ListWalletsByUser(ctx, *key.UserID); listErr == nil {
			for _, wallet := range listed {
				if wallet.Primary {
					resp.PrimaryWallet = wallet.Wallet
					break
				}
			}
		}
	}
	return resp, http.StatusOK, nil
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func (s *Service) authorized(header string) bool {
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return false
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(s.internalDigest[:], digest[:]) == 1
}

func dedupePeers(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
