package deploykeys

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

// Audience isolates link challenges from ACL/pricing node-credential
// challenges sharing the node_credential_challenges table.
const Audience = "api.opentela.ai/internal/instances/link"

const (
	maxBodyBytes     = 8 << 10
	maxPeerIDLen     = 128
	maxLabelLen      = 200
	maxNameLen       = 100
	challengeTTL     = 2 * time.Minute
	challengeMaxBody = 4 << 10
	defaultMaxUses   = 1
)

// linkStore is the persistence surface the link handlers need. *store.Postgres
// satisfies it.
type linkStore interface {
	FindActiveDeployKey(ctx context.Context, keyHash string, now time.Time) (store.DeployKeyInfo, error)
	ConsumeDeployKeyUse(ctx context.Context, keyHash string, now time.Time) (string, error)
	CreateInstanceLinkChallenge(ctx context.Context, ch store.NodeCredentialChallenge) error
	ConsumeNodeCredentialChallenge(ctx context.Context, id, peerID, nonce, audience string, now time.Time) (store.NodeCredentialChallenge, error)
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
	CreateInstance(ctx context.Context, in store.InstanceInfo) (store.InstanceInfo, error)
	GetUserWalletSet(ctx context.Context, accountID string) ([]string, error)
	// Deploy-key CRUD (satisfies the embedded crud Service's Store).
	InsertDeployKey(ctx context.Context, userID, keyHash, keyPrefix, name string, maxUses int, expiresAt *time.Time) (store.DeployKeyInfo, error)
	ListDeployKeysByUser(ctx context.Context, userID string) ([]store.DeployKeyInfo, error)
	CountActiveDeployKeysByUser(ctx context.Context, userID string) (int, error)
	RevokeDeployKeyByIDForUser(ctx context.Context, userID string, id int64) (keyHash string, changed bool, err error)
}

type linkMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

// LinkService serves the deploy-key-authenticated link endpoints plus the
// JWT-managed deploy-key CRUD routes (the latter share the manage plane's
// principal middleware and delegate to the embedded CRUD Service).
type LinkService struct {
	store           linkStore
	crud            *Service
	mesh            linkMesh
	ownershipMaxAge time.Duration
	now             func() time.Time
}

// NewLinkService wires the link handlers. ownershipMaxAge mirrors
// instancesapi: observations older than it are not consulted.
// manageMaxPerUser bounds the account's non-revoked deploy keys.
func NewLinkService(pg linkStore, meshClient linkMesh, ownershipMaxAge time.Duration, manageMaxPerUser int) *LinkService {
	return &LinkService{
		store:           pg,
		crud:            New(pg, manageMaxPerUser),
		mesh:            meshClient,
		ownershipMaxAge: ownershipMaxAge,
		now:             time.Now,
	}
}

type challengeRequest struct {
	PeerID string `json:"peer_id"`
}

type challengeResponse struct {
	ChallengeID string    `json:"challenge_id"`
	PeerID      string    `json:"peer_id"`
	Audience    string    `json:"audience"`
	Nonce       string    `json:"nonce"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Message     string    `json:"message"`
}

type linkRequest struct {
	PeerID      string `json:"peer_id"`
	ChallengeID string `json:"challenge_id"`
	Nonce       string `json:"nonce"`
	PublicKey   string `json:"public_key"`
	Signature   string `json:"signature"`
	Label       string `json:"label"`
}

type linkResponse struct {
	ID              int64      `json:"id"`
	PeerID          string     `json:"peer_id"`
	Label           string     `json:"label"`
	OwnerWallet     string     `json:"owner_wallet"`
	Mode            string     `json:"mode"`
	OwnershipStatus string     `json:"ownership_status"`
	Relinked        bool       `json:"relinked,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// LinkChallengeHandler issues a single-use libp2p signing challenge to the
// bearer of a valid deploy key. Unlike the trusted-region challenge this
// does NOT require the peer to already exist: linking is how a peer first
// appears under an account.
func (s *LinkService) LinkChallengeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if _, ok := s.authorized(w, r); !ok {
			return
		}
		var req challengeRequest
		if err := httputil.DecodeStrict(w, r, challengeMaxBody, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		peerID := strings.TrimSpace(req.PeerID)
		if !ValidPeerID(peerID) {
			http.Error(w, "invalid peer_id", http.StatusBadRequest)
			return
		}
		now := s.now().UTC()
		nonce, err := randomString(24)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		challengeID, err := randomString(18)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		issuedAt := now
		expiresAt := issuedAt.Add(challengeTTL)
		message := canonicalChallengeMessage(Audience, challengeID, peerID, nonce, issuedAt, expiresAt)
		if err := s.store.CreateInstanceLinkChallenge(r.Context(), store.NodeCredentialChallenge{
			ID:               challengeID,
			PeerID:           peerID,
			Audience:         Audience,
			NonceHash:        store.HashKey(nonce),
			ChallengeMessage: message,
			IssuedAt:         issuedAt,
			ExpiresAt:        expiresAt,
		}); errors.Is(err, store.ErrConflict) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many pending challenges", http.StatusTooManyRequests)
			return
		} else if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, challengeResponse{
			ChallengeID: challengeID,
			PeerID:      peerID,
			Audience:    Audience,
			Nonce:       nonce,
			IssuedAt:    issuedAt,
			ExpiresAt:   expiresAt,
			Message:     message,
		})
	})
}

// LinkHandler completes the flow: consume the challenge, verify the libp2p
// proof (only the holder of the peer's private key can produce it), spend
// one use of the deploy key, and bind the peer to the key's account. No
// wallet observation is required: ownership lives server-side. If the peer
// is already observed on the mesh under a DIFFERENT wallet the link is
// refused, preserving the wallet-flow reclaim semantics.
func (s *LinkService) LinkHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key, ok := s.authorized(w, r)
		if !ok {
			return
		}
		var req linkRequest
		if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		peerID := strings.TrimSpace(req.PeerID)
		if !ValidPeerID(peerID) {
			http.Error(w, "invalid peer_id", http.StatusBadRequest)
			return
		}
		if len(req.Label) > maxLabelLen {
			http.Error(w, "label too long", http.StatusBadRequest)
			return
		}
		now := s.now().UTC()
		challenge, err := s.store.ConsumeNodeCredentialChallenge(r.Context(), req.ChallengeID, peerID, req.Nonce, Audience, now)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				http.Error(w, "challenge not found", http.StatusNotFound)
			case errors.Is(err, store.ErrChallengeExpired), errors.Is(err, store.ErrChallengeConsumed):
				http.Error(w, "challenge unavailable", http.StatusConflict)
			default:
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if err := verifyPeerProof(req.PublicKey, req.Signature, peerID, challenge.ChallengeMessage); err != nil {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		existing, err := s.store.GetInstanceByPeerID(r.Context(), peerID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if err == nil {
			if existing.AccountID == key.UserID {
				// Idempotent re-link: the peer is already this account's. No
				// key use is spent, and an exhausted key may still re-link
				// (e.g. a node reinstall re-verifying itself).
				httputil.WriteJSON(w, http.StatusOK, linkResponse{
					ID: existing.ID, PeerID: existing.PeerID, Label: existing.Label,
					OwnerWallet: existing.OwnerWallet, Mode: existing.AccessMode,
					OwnershipStatus: existing.OwnershipStatus, Relinked: true,
					CreatedAt: existing.CreatedAt, UpdatedAt: &existing.UpdatedAt,
				})
				return
			}
			http.Error(w, "peer already claimed", http.StatusConflict)
			return
		}
		if !key.Usable(now) {
			http.Error(w, "deploy key exhausted or expired", http.StatusForbidden)
			return
		}
		wallets, err := s.store.GetUserWalletSet(r.Context(), key.UserID)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if len(wallets) == 0 {
			// Billing settles through the account's wallet, so a linked wallet
			// is a precondition even though the LINK itself is wallet-free.
			http.Error(w, "link a wallet to your account first (billing requires one)", http.StatusUnprocessableEntity)
			return
		}
		ownerWallet := wallets[0] // oldest first; one-wallet-per-account makes this the wallet
		if obs, err := s.mesh.LookupPeer(r.Context(), peerID); err == nil && s.observationFresh(obs) && obs.Wallet != ownerWallet {
			http.Error(w, "peer belongs to a different wallet", http.StatusConflict)
			return
		}
		userID, err := s.store.ConsumeDeployKeyUse(r.Context(), key.KeyHash, now)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "deploy key exhausted or expired", http.StatusForbidden)
				return
			}
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		created, err := s.store.CreateInstance(r.Context(), store.InstanceInfo{
			AccountID:       userID,
			PeerID:          peerID,
			Label:           req.Label,
			OwnerWallet:     ownerWallet,
			AccessMode:      "restricted",
			OwnershipStatus: "active",
			// No wallet observation: ownership was proven by the libp2p
			// challenge, and management ops skip the mesh cross-check for
			// rows with no observed wallet (see instancesapi).
		})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				http.Error(w, "peer already claimed", http.StatusConflict)
				return
			}
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, linkResponse{
			ID: created.ID, PeerID: created.PeerID, Label: created.Label,
			OwnerWallet: created.OwnerWallet, Mode: created.AccessMode,
			OwnershipStatus: created.OwnershipStatus, CreatedAt: created.CreatedAt,
		})
	})
}

// authorized extracts and resolves the deploy key from the Authorization
// header. It resolves the full row (not just existence) so handlers see
// UserID / UsesLeft; the write-path re-checks Usable atomically.
func (s *LinkService) authorized(w http.ResponseWriter, r *http.Request) (store.DeployKeyInfo, bool) {
	header := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" || !strings.HasPrefix(token, "otd-") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="deploy-key"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return store.DeployKeyInfo{}, false
	}
	key, err := s.store.FindActiveDeployKey(r.Context(), store.HashKey(token), s.now())
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="deploy-key", error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return store.DeployKeyInfo{}, false
	}
	return key, true
}

func (s *LinkService) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func canonicalChallengeMessage(audience, challengeID, peerID, nonce string, issuedAt, expiresAt time.Time) string {
	return fmt.Sprintf(
		"opentela-instance-link-challenge\nchallenge_id=%s\npeer_id=%s\nregion=\nrole=\naudience=%s\nnonce=%s\nissued_at=%s\nexpires_at=%s\n",
		challengeID,
		peerID,
		audience,
		nonce,
		issuedAt.UTC().Format(time.RFC3339),
		expiresAt.UTC().Format(time.RFC3339),
	)
}

func verifyPeerProof(publicKeyB64, signatureB64, peerID, message string) error {
	pubRaw, err := decodeRawBase64(publicKeyB64)
	if err != nil {
		return err
	}
	pub, err := crypto.UnmarshalPublicKey(pubRaw)
	if err != nil {
		return err
	}
	sigRaw, err := decodeRawBase64(signatureB64)
	if err != nil {
		return err
	}
	derived, err := peer.IDFromPublicKey(pub)
	if err != nil || derived.String() != peerID {
		return errors.New("peer id mismatch")
	}
	ok, err := pub.Verify([]byte(message), sigRaw)
	if err != nil || !ok {
		return errors.New("signature invalid")
	}
	return nil
}

func decodeRawBase64(v string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(v); err == nil {
		return raw, nil
	}
	return base64.StdEncoding.DecodeString(v)
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidPeerID reports whether v is a well-formed libp2p peer ID. It mirrors
// instancesapi.ValidPeerID (kept local to avoid coupling the packages); the
// authoritative check is verifyPeerProof deriving the ID from the proven
// public key.
func ValidPeerID(v string) bool {
	if v == "" || len(v) > maxPeerIDLen {
		return false
	}
	raw, err := solana.DecodeBase58(v, maxPeerIDLen)
	if err != nil {
		return false
	}
	_, codeBytes := binary.Uvarint(raw)
	if codeBytes <= 0 || codeBytes >= len(raw) {
		return false
	}
	digestLen, lengthBytes := binary.Uvarint(raw[codeBytes:])
	if lengthBytes <= 0 || digestLen == 0 {
		return false
	}
	return int(digestLen) == len(raw)-codeBytes-lengthBytes
}

// ── console-facing CRUD (JWT principal middleware applied by manageapi) ──

type createRequest struct {
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses"`
	// TTLSeconds is the key lifetime; 0 means no expiry. Bounded by
	// MinTTL/MaxTTL in the service.
	TTLSeconds int64 `json:"ttl_seconds"`
}

type createResponse struct {
	ID        int64      `json:"id"`
	Key       string     `json:"key"`
	Prefix    string     `json:"prefix"`
	Name      string     `json:"name"`
	MaxUses   int        `json:"max_uses"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type deployKeyResponse struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	MaxUses    int        `json:"max_uses"`
	UseCount   int        `json:"use_count"`
	UsesLeft   int        `json:"uses_left"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ManageRoutes builds the /manage/deploy-keys handler tree. The manage
// plane's principal middleware is applied by the caller.
func (s *LinkService) ManageRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/deploy-keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := principal.UserID(r.Context())
		var req createRequest
		if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.Name) > maxNameLen {
			http.Error(w, "name too long", http.StatusBadRequest)
			return
		}
		maxUses := req.MaxUses
		if maxUses == 0 {
			maxUses = defaultMaxUses
		}
		var ttl time.Duration
		if req.TTLSeconds != 0 {
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}
		token, info, err := s.crud.Create(r.Context(), userID, req.Name, maxUses, ttl)
		if errors.Is(err, ErrTooManyKeys) {
			http.Error(w, "deploy key limit reached", http.StatusConflict)
			return
		}
		if errors.Is(err, ErrInvalidTTL) || errors.Is(err, ErrInvalidMaxUses) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, createResponse{
			ID: info.ID, Key: token, Prefix: info.KeyPrefix, Name: info.Name,
			MaxUses: info.MaxUses, ExpiresAt: info.ExpiresAt, CreatedAt: info.CreatedAt,
		})
	})
	mux.HandleFunc("GET /manage/deploy-keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := principal.UserID(r.Context())
		keys, err := s.crud.List(r.Context(), userID)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		out := make([]deployKeyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, deployKeyResponse{
				ID: k.ID, Name: k.Name, Prefix: k.KeyPrefix, MaxUses: k.MaxUses,
				UseCount: k.UseCount, UsesLeft: k.UsesLeft(), ExpiresAt: k.ExpiresAt,
				LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
			})
		}
		httputil.WriteJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /manage/deploy-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := principal.UserID(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid key id", http.StatusBadRequest)
			return
		}
		changed, err := s.crud.Revoke(r.Context(), userID, id)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if !changed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
