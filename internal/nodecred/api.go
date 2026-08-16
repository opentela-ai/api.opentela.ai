package nodecred

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

const (
	challengeTTL = 2 * time.Minute
	maxTokenTTL  = 15 * time.Minute
)

type challengeStore interface {
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
	CreateNodeCredentialChallenge(ctx context.Context, ch store.NodeCredentialChallenge) error
	ConsumeNodeCredentialChallenge(ctx context.Context, id, peerID, nonce, audience string, now time.Time) (store.NodeCredentialChallenge, error)
}

type challengeMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

type Service struct {
	store           challengeStore
	mesh            challengeMesh
	signer          *Signer
	verifier        *Verifier
	pricingSigner   *Signer
	ownershipMaxAge time.Duration
	now             func() time.Time
	internalDigest  [32]byte
	internalEnabled bool
}

// WithPricing equips the service with a pricing-scoped signer
// (PricingAudience) so it can issue seller-ask credentials via
// PricingChallengeHandler/PricingIssueHandler. Without it those handlers
// return 503. The ACL signer/verifier are unaffected.
func (s *Service) WithPricing(signer *Signer) *Service {
	cp := *s
	cp.pricingSigner = signer
	return &cp
}

type challengeRequest struct {
	PeerID string `json:"peer_id"`
}

type challengeResponse struct {
	ChallengeID string    `json:"challenge_id"`
	PeerID      string    `json:"peer_id"`
	Region      string    `json:"region"`
	Role        string    `json:"role"`
	Audience    string    `json:"audience"`
	Nonce       string    `json:"nonce"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Message     string    `json:"message"`
}

type issueRequest struct {
	ChallengeID string `json:"challenge_id"`
	PeerID      string `json:"peer_id"`
	Nonce       string `json:"nonce"`
	PublicKey   string `json:"public_key"`
	Signature   string `json:"signature"`
}

type issueResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	KID       string    `json:"kid"`
}

func NewService(pg challengeStore, meshClient challengeMesh, signer *Signer, verifier *Verifier, ownershipMaxAge time.Duration, internalToken string) *Service {
	return &Service{
		store: pg, mesh: meshClient, signer: signer, verifier: verifier,
		ownershipMaxAge: ownershipMaxAge, now: time.Now,
		internalDigest:  sha256.Sum256([]byte(internalToken)),
		internalEnabled: strings.TrimSpace(internalToken) != "",
	}
}

func (s *Service) ChallengeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			w.Header().Set("X-Otela-Control-Auth-Failed", "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req challengeRequest
		if err := httputil.DecodeStrict(w, r, 8<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		inst, err := s.store.GetInstanceByPeerID(r.Context(), strings.TrimSpace(req.PeerID))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		now := s.now().UTC()
		if !activeMembership(inst.Membership, now) {
			http.Error(w, "trusted region unavailable", http.StatusConflict)
			return
		}
		obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			http.Error(w, "trusted region unavailable", http.StatusConflict)
			return
		}
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
		message := canonicalChallengeMessage(Audience, challengeID, inst.PeerID, inst.Membership.RegionSlug, inst.Membership.NodeRole, nonce, issuedAt, expiresAt)
		if err := s.store.CreateNodeCredentialChallenge(r.Context(), store.NodeCredentialChallenge{
			ID:               challengeID,
			PeerID:           inst.PeerID,
			RegionSlug:       inst.Membership.RegionSlug,
			NodeRole:         inst.Membership.NodeRole,
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
			PeerID:      inst.PeerID,
			Region:      inst.Membership.RegionSlug,
			Role:        inst.Membership.NodeRole,
			Audience:    Audience,
			Nonce:       nonce,
			IssuedAt:    issuedAt,
			ExpiresAt:   expiresAt,
			Message:     message,
		})
	})
}

func (s *Service) IssueHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			w.Header().Set("X-Otela-Control-Auth-Failed", "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req issueRequest
		if err := httputil.DecodeStrict(w, r, 32<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		now := s.now().UTC()
		challenge, err := s.store.ConsumeNodeCredentialChallenge(r.Context(), req.ChallengeID, req.PeerID, req.Nonce, Audience, now)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				http.Error(w, "not found", http.StatusNotFound)
			case errors.Is(err, store.ErrChallengeExpired), errors.Is(err, store.ErrChallengeConsumed):
				http.Error(w, "challenge unavailable", http.StatusConflict)
			default:
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		pub, sig, err := decodePeerProof(req.PublicKey, req.Signature)
		if err != nil {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		derived, err := peer.IDFromPublicKey(pub)
		if err != nil || derived.String() != req.PeerID {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		ok, err := pub.Verify([]byte(challenge.ChallengeMessage), sig)
		if err != nil || !ok {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		inst, err := s.store.GetInstanceByPeerID(r.Context(), req.PeerID)
		if err != nil || inst.Membership == nil || inst.Membership.RegionSlug != challenge.RegionSlug || inst.Membership.NodeRole != challenge.NodeRole || !activeMembership(inst.Membership, now) {
			http.Error(w, "trusted region unavailable", http.StatusConflict)
			return
		}
		obs, err := s.mesh.LookupPeer(r.Context(), req.PeerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			http.Error(w, "trusted region unavailable", http.StatusConflict)
			return
		}
		jti, err := randomString(18)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		issuedAt := now
		expiresAt := issuedAt.Add(maxTokenTTL)
		token, err := s.signer.Sign(Claims{
			Subject:            req.PeerID,
			Role:               inst.Membership.NodeRole,
			Region:             inst.Membership.RegionSlug,
			MembershipRevision: inst.Membership.MembershipRevision,
			JTI:                jti,
			IssuedAt:           issuedAt,
			ExpiresAt:          expiresAt,
		})
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, issueResponse{Token: token, ExpiresAt: expiresAt, KID: s.signer.kid})
	})
}

// PricingChallengeHandler issues a pricing-scoped challenge to a billable,
// payable provider. Unlike the ACL challenge it does NOT require trusted-
// region membership: the marketplace is for permissionless providers. The
// peer must still prove it controls the node's owner wallet (via a fresh
// mesh observation) so a credential cannot be minted for a provider whose
// ownership has changed.
func (s *Service) PricingChallengeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			w.Header().Set("X-Otela-Control-Auth-Failed", "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.pricingSigner == nil {
			http.Error(w, "pricing credentials not configured", http.StatusServiceUnavailable)
			return
		}
		var req challengeRequest
		if err := httputil.DecodeStrict(w, r, 8<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		inst, err := s.store.GetInstanceByPeerID(r.Context(), strings.TrimSpace(req.PeerID))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if !billing.BillableProvider(inst.AccountID, inst.OwnerWallet) {
			// Only a provider that can be charged and paid can publish asks.
			http.Error(w, "not a billable provider", http.StatusConflict)
			return
		}
		obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			http.Error(w, "ownership unavailable", http.StatusConflict)
			return
		}
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
		issuedAt := s.now().UTC()
		expiresAt := issuedAt.Add(challengeTTL)
		message := canonicalChallengeMessage(PricingAudience, challengeID, inst.PeerID, "", "", nonce, issuedAt, expiresAt)
		if err := s.store.CreateNodeCredentialChallenge(r.Context(), store.NodeCredentialChallenge{
			ID:               challengeID,
			PeerID:           inst.PeerID,
			Audience:         PricingAudience,
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
			PeerID:      inst.PeerID,
			Audience:    PricingAudience,
			Nonce:       nonce,
			IssuedAt:    issuedAt,
			ExpiresAt:   expiresAt,
			Message:     message,
		})
	})
}

// PricingIssueHandler consumes a pricing-scoped challenge and issues a
// pricing-scoped credential (Audience = PricingAudience). The credential
// carries no role/region: the pricing endpoint verifies billable-provider
// eligibility against the live instance at publication time.
func (s *Service) PricingIssueHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			w.Header().Set("X-Otela-Control-Auth-Failed", "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.pricingSigner == nil {
			http.Error(w, "pricing credentials not configured", http.StatusServiceUnavailable)
			return
		}
		var req issueRequest
		if err := httputil.DecodeStrict(w, r, 32<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		now := s.now().UTC()
		challenge, err := s.store.ConsumeNodeCredentialChallenge(r.Context(), req.ChallengeID, req.PeerID, req.Nonce, PricingAudience, now)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				http.Error(w, "not found", http.StatusNotFound)
			case errors.Is(err, store.ErrChallengeExpired), errors.Is(err, store.ErrChallengeConsumed):
				http.Error(w, "challenge unavailable", http.StatusConflict)
			default:
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		// Defense in depth: a challenge created for the ACL audience must not
		// yield a pricing credential even if the store filter were bypassed.
		if challenge.Audience != PricingAudience {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		pub, sig, err := decodePeerProof(req.PublicKey, req.Signature)
		if err != nil {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		derived, err := peer.IDFromPublicKey(pub)
		if err != nil || derived.String() != req.PeerID {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		ok, err := pub.Verify([]byte(challenge.ChallengeMessage), sig)
		if err != nil || !ok {
			http.Error(w, "invalid proof", http.StatusBadRequest)
			return
		}
		inst, err := s.store.GetInstanceByPeerID(r.Context(), req.PeerID)
		if err != nil || !billing.BillableProvider(inst.AccountID, inst.OwnerWallet) {
			http.Error(w, "not a billable provider", http.StatusConflict)
			return
		}
		obs, err := s.mesh.LookupPeer(r.Context(), req.PeerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			http.Error(w, "ownership unavailable", http.StatusConflict)
			return
		}
		jti, err := randomString(18)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		issuedAt := now
		expiresAt := issuedAt.Add(maxTokenTTL)
		token, err := s.pricingSigner.Sign(Claims{
			Subject:   req.PeerID,
			Audience:  PricingAudience,
			JTI:       jti,
			IssuedAt:  issuedAt,
			ExpiresAt: expiresAt,
		})
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, issueResponse{Token: token, ExpiresAt: expiresAt, KID: s.pricingSigner.kid})
	})
}

func activeMembership(membership *store.RegionMembership, now time.Time) bool {
	return membership != nil && membership.Status == "active" && membership.RegionStatus == "active" &&
		(membership.ExpiresAt == nil || now.Before(membership.ExpiresAt.UTC()))
}

func (s *Service) Verify(ctx context.Context, raw string) (Claims, error) {
	return s.verifier.Verify(ctx, raw)
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func (s *Service) authorized(header string) bool {
	if !s.internalEnabled {
		return false
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	token := strings.TrimSpace(header[len(prefix):])
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(s.internalDigest[:], digest[:]) == 1
}

func canonicalChallengeMessage(audience, challengeID, peerID, region, role, nonce string, issuedAt, expiresAt time.Time) string {
	return fmt.Sprintf(
		"opentela-node-credential-challenge\nchallenge_id=%s\npeer_id=%s\nregion=%s\nrole=%s\naudience=%s\nnonce=%s\nissued_at=%s\nexpires_at=%s\n",
		challengeID,
		peerID,
		region,
		role,
		audience,
		nonce,
		issuedAt.UTC().Format(time.RFC3339),
		expiresAt.UTC().Format(time.RFC3339),
	)
}

func decodePeerProof(publicKeyB64, signatureB64 string) (crypto.PubKey, []byte, error) {
	pubRaw, err := decodeRawBase64(publicKeyB64)
	if err != nil {
		return nil, nil, err
	}
	pub, err := crypto.UnmarshalPublicKey(pubRaw)
	if err != nil {
		return nil, nil, err
	}
	sigRaw, err := decodeRawBase64(signatureB64)
	if err != nil {
		return nil, nil, err
	}
	return pub, sigRaw, nil
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
