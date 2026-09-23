package aclapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/store"
)

const (
	maxPeerBatch             = 100
	controlAuthFailureHeader = "X-Otela-Control-Auth-Failed"

	// bucketSweepInterval is how often allow scans for reapable buckets.
	bucketSweepInterval = time.Minute
	// bucketIdleTTL is how long a caller bucket may go unused before it is
	// reaped. Without eviction the bucket map would grow for the process
	// lifetime — every distinct caller identity creates an entry, and
	// permissionless keys accumulate one per issued API key. Reaping only
	// ever forgives debt, so eviction can make the limiter more lenient
	// for long-idle callers, never stricter.
	bucketIdleTTL = 10 * time.Minute
)

type Service struct {
	store           aclStore
	mesh            peerLookup
	internalDigest  [32]byte
	nodeVerifier    nodeCredentialVerifier
	identityMaxAge  time.Duration
	ownershipMaxAge time.Duration
	cacheTTL        time.Duration
	evalLimiter     *rateLimiter
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

type nodeCredentialVerifier interface {
	Verify(ctx context.Context, raw string) (nodecred.Claims, error)
}

type evaluateRequest struct {
	KeyHash string   `json:"key_hash"`
	PeerIDs []string `json:"peer_ids"`
}

type evaluateV2Request struct {
	KeyHash        string   `json:"key_hash"`
	Partition      string   `json:"partition"`
	Region         string   `json:"region"`
	RouteKind      string   `json:"route_kind"`
	Service        string   `json:"service"`
	PeerIDs        []string `json:"peer_ids"`
	UpstreamPeerID string   `json:"upstream_peer_id"`
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

type decisionScope struct {
	Partition      string `json:"partition"`
	Region         string `json:"region"`
	RouteKind      string `json:"route_kind"`
	Service        string `json:"service"`
	RegionRevision int64  `json:"region_revision"`
}

type peerDecision struct {
	PeerID                string    `json:"peer_id"`
	Allowed               bool      `json:"allowed"`
	Reason                string    `json:"reason"`
	PolicyScope           string    `json:"policy_scope"`
	ServiceExposure       string    `json:"service_exposure,omitempty"`
	PolicyRevision        int64     `json:"policy_revision"`
	ServicePolicyRevision int64     `json:"service_policy_revision,omitempty"`
	MembershipRevision    int64     `json:"membership_revision,omitempty"`
	EvaluatedAt           time.Time `json:"evaluated_at"`
}

type evaluateV2Response struct {
	KeyID         string         `json:"key_id"`
	PrimaryWallet string         `json:"primary_wallet"`
	DecisionScope decisionScope  `json:"decision_scope"`
	Decisions     []peerDecision `json:"decisions"`
}

type principalContext struct {
	key             store.ActiveKey
	identity        store.IdentityInfo
	identityLoaded  bool
	identityErr     error
	walletSet       map[string]struct{}
	walletSetLoaded bool
	walletSetErr    error
	walletsLoaded   bool
	primaryWallet   string
}

func New(pg aclStore, meshClient peerLookup, token string, identityMaxAge, ownershipMaxAge, cacheTTL time.Duration) *Service {
	return NewWithNodeVerifier(pg, meshClient, token, nil, identityMaxAge, ownershipMaxAge, cacheTTL)
}

func NewWithNodeVerifier(pg aclStore, meshClient peerLookup, token string, nodeVerifier nodeCredentialVerifier, identityMaxAge, ownershipMaxAge, cacheTTL time.Duration) *Service {
	return &Service{
		store:           pg,
		mesh:            meshClient,
		internalDigest:  sha256.Sum256([]byte(token)),
		nodeVerifier:    nodeVerifier,
		identityMaxAge:  identityMaxAge,
		ownershipMaxAge: ownershipMaxAge,
		cacheTTL:        cacheTTL,
		now:             time.Now,
	}
}

func (s *Service) Handler() http.Handler {
	return s.HandlerV1()
}

func (s *Service) HandlerV1() http.Handler {
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
		if !validKeyHash(req.KeyHash) {
			http.Error(w, "invalid key hash", http.StatusBadRequest)
			return
		}
		peerIDs := dedupePeers(req.PeerIDs)
		if len(peerIDs) == 0 || len(peerIDs) > maxPeerBatch {
			http.Error(w, "invalid peer_ids", http.StatusBadRequest)
			return
		}
		resp, status, err := s.evaluateV1(r.Context(), req.KeyHash, peerIDs)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, resp)
	})
}

func (s *Service) HandlerV2() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req evaluateV2Request
		if err := httputil.DecodeStrict(w, r, 48<<10, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if !validKeyHash(req.KeyHash) {
			http.Error(w, "invalid key hash", http.StatusBadRequest)
			return
		}
		partition := strings.ToLower(strings.TrimSpace(req.Partition))
		if partition != "permissionless" && partition != "trusted_region" {
			http.Error(w, "invalid partition", http.StatusBadRequest)
			return
		}
		routeKind := strings.ToLower(strings.TrimSpace(req.RouteKind))
		if routeKind != "service_ingress" && routeKind != "p2p_ingress" && routeKind != "worker" {
			http.Error(w, "invalid route_kind", http.StatusBadRequest)
			return
		}
		if !validServiceName(req.Service) {
			http.Error(w, "invalid service", http.StatusBadRequest)
			return
		}
		if partition == "trusted_region" && strings.TrimSpace(req.Region) == "" {
			http.Error(w, "invalid region", http.StatusBadRequest)
			return
		}
		if partition == "permissionless" {
			req.Region = ""
		}
		if routeKind == "worker" && strings.TrimSpace(req.UpstreamPeerID) == "" {
			http.Error(w, "invalid upstream_peer_id", http.StatusBadRequest)
			return
		}
		peerIDs := dedupePeers(req.PeerIDs)
		if len(peerIDs) == 0 || len(peerIDs) > maxPeerBatch {
			http.Error(w, "invalid peer_ids", http.StatusBadRequest)
			return
		}
		rawAuth := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		var claims *nodecred.Claims
		if partition == "permissionless" {
			if !s.authorized(r.Header.Get("Authorization")) {
				w.Header().Set(controlAuthFailureHeader, "true")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else {
			if s.nodeVerifier == nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			verified, err := s.nodeVerifier.Verify(r.Context(), rawAuth)
			if err != nil {
				w.Header().Set(controlAuthFailureHeader, "true")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			claims = &verified
		}
		// Per-caller rate limiting. Trusted decisions are never cached, so a
		// flood of live evaluations from one head can starve the control
		// plane; throttle by the authenticated caller identity (peer id for
		// trusted, key hash for permissionless) before doing any DB work.
		rateKey := req.KeyHash
		if partition == "trusted_region" && claims != nil {
			rateKey = "peer:" + claims.Subject
		}
		if s.evalLimiter != nil {
			if ok, retryAfter := s.evalLimiter.allow(rateKey); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
		}
		resp, status, err := s.evaluateV2(r.Context(), req, claims)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, resp)
	})
}

func (s *Service) evaluate(ctx context.Context, keyHash string, peerIDs []string) (evaluateResponse, int, error) {
	return s.evaluateV1(ctx, keyHash, peerIDs)
}

func (s *Service) evaluateV1(ctx context.Context, keyHash string, peerIDs []string) (evaluateResponse, int, error) {
	key, peers, observations, status, err := s.loadEvaluationState(ctx, keyHash, peerIDs, nil)
	if err != nil {
		return evaluateResponse{}, status, err
	}
	resp := evaluateResponse{
		KeyID:           strconv.FormatInt(key.KeyID, 10),
		AllowedPeerIDs:  make([]string, 0, len(peerIDs)),
		Denied:          make([]deniedPeer, 0, len(peerIDs)),
		CacheTTLSeconds: int(s.cacheTTL / time.Second),
	}
	principalState := &principalContext{key: key}
	now := s.now().UTC()
	for _, peerID := range peerIDs {
		inst, ok := peers[peerID]
		if !ok {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
			continue
		}
		if inst.PolicyScope == store.PolicyScopeService {
			resp.Denied = append(resp.Denied, deniedPeer{PeerID: peerID, Reason: "service_context_required"})
			continue
		}
		reason, ok := s.evaluatePeerScoped(inst, observations[peerID], "permissionless", "", "service_ingress")
		if !ok {
			resp.Denied = append(resp.Denied, deniedPeer{PeerID: peerID, Reason: reason})
			continue
		}
		allowed, allowReason, status, err := s.applyInstanceACL(ctx, principalState, inst.AccessMode, inst.AccountID, inst.Rules, now)
		if err != nil {
			return evaluateResponse{}, status, err
		}
		if allowed {
			resp.AllowedPeerIDs = append(resp.AllowedPeerIDs, peerID)
		} else {
			resp.Denied = append(resp.Denied, deniedPeer{PeerID: peerID, Reason: allowReason})
		}
	}
	resp.PrimaryWallet = principalState.primaryWalletValue(ctx, s.store)
	return resp, http.StatusOK, nil
}

func (s *Service) evaluateV2(ctx context.Context, req evaluateV2Request, claims *nodecred.Claims) (evaluateV2Response, int, error) {
	key, peers, observations, status, err := s.loadEvaluationState(ctx, req.KeyHash, req.PeerIDs, claims)
	if err != nil {
		return evaluateV2Response{}, status, err
	}
	now := s.now().UTC()
	if req.Partition == "trusted_region" {
		if status, err := s.authorizeTrustedRequester(ctx, peers, observations, req, claims, now); err != nil {
			return evaluateV2Response{}, status, err
		}
	}
	principalState := &principalContext{key: key}
	workerUpstreamAllowed := true
	if req.Partition == "trusted_region" && req.RouteKind == "worker" {
		workerUpstreamAllowed = trustedUpstreamAllowed(peers[req.UpstreamPeerID], observations[req.UpstreamPeerID], req.Region, now)
	}
	resp := evaluateV2Response{
		KeyID: strconv.FormatInt(key.KeyID, 10),
		DecisionScope: decisionScope{
			Partition: req.Partition,
			Region:    req.Region,
			RouteKind: req.RouteKind,
			Service:   req.Service,
		},
		Decisions: make([]peerDecision, 0, len(req.PeerIDs)),
	}
	for _, peerID := range req.PeerIDs {
		inst, ok := peers[peerID]
		if !ok {
			if req.Partition == "permissionless" {
				resp.Decisions = append(resp.Decisions, peerDecision{PeerID: peerID, Allowed: true, Reason: "unmanaged", EvaluatedAt: now})
				continue
			}
			resp.Decisions = append(resp.Decisions, peerDecision{PeerID: peerID, Allowed: false, Reason: "unmanaged", EvaluatedAt: now})
			continue
		}
		obs := observations[peerID]
		decision := peerDecision{
			PeerID:         peerID,
			PolicyScope:    inst.PolicyScope,
			PolicyRevision: inst.PolicyRevision,
			EvaluatedAt:    now,
		}
		if inst.Membership != nil && resp.DecisionScope.RegionRevision == 0 {
			resp.DecisionScope.RegionRevision = inst.Membership.RegionRevision
		}
		switch inst.PolicyScope {
		case store.PolicyScopeService:
			decision = s.evaluateServiceScopedDecision(ctx, principalState, req, inst, obs, decision, now, workerUpstreamAllowed)
		default:
			decision = s.evaluatePeerScopedDecision(ctx, principalState, req, inst, obs, decision, now, workerUpstreamAllowed)
		}
		resp.Decisions = append(resp.Decisions, decision)
	}
	resp.PrimaryWallet = principalState.primaryWalletValue(ctx, s.store)
	return resp, http.StatusOK, nil
}

func (s *Service) evaluatePeerScopedDecision(ctx context.Context, principalState *principalContext, req evaluateV2Request, inst store.InstanceInfo, obs mesh.PeerObservation, decision peerDecision, now time.Time, workerUpstreamAllowed bool) peerDecision {
	reason, ok := s.evaluatePeerScoped(inst, obs, req.Partition, req.Region, req.RouteKind)
	if !ok {
		decision.Allowed = false
		decision.Reason = reason
		if inst.Membership != nil {
			decision.MembershipRevision = inst.Membership.MembershipRevision
		}
		return decision
	}
	if req.Partition == "trusted_region" {
		if req.RouteKind == "worker" && !workerUpstreamAllowed {
			decision.Allowed = false
			decision.Reason = "untrusted_upstream"
			return decision
		}
		if inst.Membership == nil || !trustedTargetRoleAllowed(inst.Membership.NodeRole) {
			decision.Allowed = false
			decision.Reason = "wrong_role"
			return decision
		}
	}
	allowed, allowReason, status, err := s.applyInstanceACL(ctx, principalState, inst.AccessMode, inst.AccountID, inst.Rules, now)
	if err != nil {
		decision.Allowed = false
		decision.Reason = "service_unavailable"
		if status == http.StatusUnauthorized {
			decision.Reason = "unauthorized"
		}
		return decision
	}
	decision.Allowed = allowed
	if allowed {
		decision.Reason = allowReason
	} else {
		decision.Reason = allowReason
	}
	if inst.Membership != nil {
		decision.MembershipRevision = inst.Membership.MembershipRevision
	}
	return decision
}

func (s *Service) evaluateServiceScopedDecision(ctx context.Context, principalState *principalContext, req evaluateV2Request, inst store.InstanceInfo, obs mesh.PeerObservation, decision peerDecision, now time.Time, workerUpstreamAllowed bool) peerDecision {
	decision.ServiceExposure = ""
	service, found := findServicePolicy(inst.Services, req.Service)
	if req.Service == "" {
		decision.Allowed = false
		decision.Reason = "service_context_required"
		return decision
	}
	if countService(obs, req.Service) > 1 {
		decision.Allowed = false
		decision.Reason = "duplicate_service_name"
		return decision
	}
	if !found {
		decision.Allowed = false
		decision.Reason = "service_undeclared"
		return decision
	}
	decision.ServiceExposure = service.Exposure
	decision.ServicePolicyRevision = service.ServicePolicyRevision
	if service.Exposure == store.ExposureDisabled {
		decision.Allowed = false
		decision.Reason = "service_disabled"
		return decision
	}
	if req.Partition == "permissionless" && service.Exposure == store.ExposureTrustedRegion {
		decision.Allowed = false
		decision.Reason = "trusted_route_required"
		return decision
	}
	if req.Partition == "trusted_region" && service.Exposure == store.ExposurePermissionless {
		decision.Allowed = false
		decision.Reason = "partition_mismatch"
		return decision
	}
	if service.Exposure == store.ExposureTrustedRegion {
		reason, ok := s.evaluateTrustedBinding(inst, obs, req.Region)
		if !ok {
			decision.Allowed = false
			decision.Reason = reason
			if inst.Membership != nil {
				decision.MembershipRevision = inst.Membership.MembershipRevision
			}
			return decision
		}
		if req.RouteKind == "worker" && !workerUpstreamAllowed {
			decision.Allowed = false
			decision.Reason = "untrusted_upstream"
			return decision
		}
		if inst.Membership == nil || !trustedTargetRoleAllowed(inst.Membership.NodeRole) {
			decision.Allowed = false
			decision.Reason = "wrong_role"
			return decision
		}
	}
	allowed, allowReason, status, err := s.applyServiceACL(ctx, principalState, inst, service, now)
	if err != nil {
		decision.Allowed = false
		decision.Reason = "service_unavailable"
		if status == http.StatusUnauthorized {
			decision.Reason = "unauthorized"
		}
		return decision
	}
	decision.Allowed = allowed
	decision.Reason = allowReason
	if inst.Membership != nil {
		decision.MembershipRevision = inst.Membership.MembershipRevision
	}
	return decision
}

func (s *Service) evaluatePeerScoped(inst store.InstanceInfo, obs mesh.PeerObservation, partition, region, routeKind string) (string, bool) {
	if !s.observationFresh(obs) {
		return "ownership_unavailable", false
	}
	if obs.Wallet != inst.OwnerWallet {
		return "ownership_mismatch", false
	}
	if partition == "permissionless" {
		if inst.Membership == nil {
			return "", true
		}
		if membershipActive(inst.Membership, s.now().UTC()) && region == inst.Membership.RegionSlug {
			if routeKind == "worker" || routeKind == "service_ingress" || routeKind == "p2p_ingress" {
				return "trusted_route_required", false
			}
		}
		return membershipReason(inst.Membership, s.now().UTC()), false
	}
	if inst.Membership == nil {
		return "not_region_member", false
	}
	if inst.Membership.RegionSlug != region {
		return "not_region_member", false
	}
	if !membershipActive(inst.Membership, s.now().UTC()) {
		return membershipReason(inst.Membership, s.now().UTC()), false
	}
	return "", true
}

func (s *Service) evaluateTrustedBinding(inst store.InstanceInfo, obs mesh.PeerObservation, region string) (string, bool) {
	if inst.Membership == nil {
		return "not_region_member", false
	}
	if inst.Membership.RegionSlug != region {
		return "not_region_member", false
	}
	if !s.observationFresh(obs) {
		return "ownership_unavailable", false
	}
	if obs.Wallet != inst.OwnerWallet {
		return "ownership_mismatch", false
	}
	if !membershipActive(inst.Membership, s.now().UTC()) {
		return membershipReason(inst.Membership, s.now().UTC()), false
	}
	return "", true
}

func membershipReason(membership *store.RegionMembership, now time.Time) string {
	if membership == nil {
		return "not_region_member"
	}
	switch membership.Status {
	case "suspended":
		return "membership_suspended"
	case "expired":
		return "membership_expired"
	case "revoked":
		return "membership_revoked"
	case "ownership_mismatch":
		return "ownership_mismatch"
	case "ownership_unavailable":
		return "ownership_unavailable"
	default:
		if membership.ExpiresAt != nil && !now.Before(membership.ExpiresAt.UTC()) {
			return "membership_expired"
		}
		if membership.RegionStatus != "active" {
			return "membership_revoked"
		}
		return "not_region_member"
	}
}

func membershipActive(membership *store.RegionMembership, now time.Time) bool {
	return membership != nil && membership.Status == "active" && membership.RegionStatus == "active" &&
		(membership.ExpiresAt == nil || now.Before(membership.ExpiresAt.UTC()))
}

func (s *Service) authorizeTrustedRequester(ctx context.Context, peers map[string]store.InstanceInfo, observations map[string]mesh.PeerObservation, req evaluateV2Request, claims *nodecred.Claims, now time.Time) (int, error) {
	if claims == nil {
		return http.StatusUnauthorized, errors.New("unauthorized")
	}
	subject, ok := peers[claims.Subject]
	if !ok || subject.Membership == nil {
		return http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	if claims.Region != req.Region || subject.Membership.RegionSlug != req.Region || !membershipActive(subject.Membership, now) {
		return http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	if subject.Membership.MembershipRevision != claims.MembershipRevision {
		return http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	// The credential's role must match the DB-authoritative membership
	// role. A JWT claiming role=head for a worker member is a forged
	// credential (the membership_revision check above already rules out a
	// legitimate re-role, since that would bump the revision), so deny it
	// conclusively rather than trusting the JWT-asserted role below.
	if claims.Role != subject.Membership.NodeRole {
		return http.StatusForbidden, errors.New("role mismatch")
	}
	obs := observations[claims.Subject]
	if !s.observationFresh(obs) || obs.Wallet != subject.OwnerWallet {
		return http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	if req.RouteKind == "worker" {
		if len(req.PeerIDs) != 1 || claims.Subject != req.PeerIDs[0] {
			return http.StatusServiceUnavailable, errors.New("service unavailable")
		}
		if claims.Role != "worker" && claims.Role != "combined" {
			return http.StatusServiceUnavailable, errors.New("service unavailable")
		}
		upstream, ok := peers[req.UpstreamPeerID]
		if !ok || upstream.Membership == nil || upstream.Membership.RegionSlug != req.Region || !membershipActive(upstream.Membership, now) || (upstream.Membership.NodeRole != "head" && upstream.Membership.NodeRole != "combined") {
			return http.StatusOK, nil
		}
		upstreamObs := observations[req.UpstreamPeerID]
		if !s.observationFresh(upstreamObs) || upstreamObs.Wallet != upstream.OwnerWallet {
			return http.StatusOK, nil
		}
		return http.StatusOK, nil
	}
	if claims.Role != "head" && claims.Role != "combined" {
		return http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	return http.StatusOK, nil
}

func trustedTargetRoleAllowed(role string) bool {
	return role == "worker" || role == "combined"
}

func trustedUpstreamAllowed(inst store.InstanceInfo, obs mesh.PeerObservation, region string, now time.Time) bool {
	if inst.Membership == nil || inst.Membership.RegionSlug != region || !membershipActive(inst.Membership, now) {
		return false
	}
	if inst.Membership.NodeRole != "head" && inst.Membership.NodeRole != "combined" {
		return false
	}
	if obs.ObservedAt.IsZero() || obs.Wallet != inst.OwnerWallet {
		return false
	}
	return true
}

func (s *Service) applyInstanceACL(ctx context.Context, principalState *principalContext, accessMode, ownerAccountID string, rules []store.ACLRule, now time.Time) (bool, string, int, error) {
	switch accessMode {
	case "public":
		return true, "instance_acl_allow", http.StatusOK, nil
	case "restricted":
		if principalState.key.UserID != nil && *principalState.key.UserID == ownerAccountID {
			return true, "instance_acl_allow", http.StatusOK, nil
		}
		return s.applyRules(ctx, principalState, ownerAccountID, rules, now)
	default:
		return false, "no_match", http.StatusOK, nil
	}
}

func (s *Service) applyServiceACL(ctx context.Context, principalState *principalContext, inst store.InstanceInfo, service store.InstanceService, now time.Time) (bool, string, int, error) {
	switch service.AccessMode {
	case store.AccessModeInherit:
		return s.applyInstanceACL(ctx, principalState, inst.AccessMode, inst.AccountID, inst.Rules, now)
	case store.AccessModePublic:
		return true, "instance_acl_allow", http.StatusOK, nil
	case store.AccessModeRestricted:
		if principalState.key.UserID != nil && *principalState.key.UserID == inst.AccountID {
			return true, "instance_acl_allow", http.StatusOK, nil
		}
		return s.applyRules(ctx, principalState, inst.AccountID, service.Rules, now)
	default:
		return false, "no_match", http.StatusOK, nil
	}
}

func (s *Service) applyRules(ctx context.Context, principalState *principalContext, ownerAccountID string, rules []store.ACLRule, now time.Time) (bool, string, int, error) {
	matched := false
	var enrichmentErr error
	// api_key rules match the caller's non-secret key prefix (sk- + 8 hex);
	// no principal enrichment is required. Keep the existing reason strings:
	// the node fleet whitelists decision reasons and fails closed on unknown
	// ones, so a match must reuse "instance_acl_allow".
	for _, rule := range rules {
		if rule.Kind != "api_key" {
			continue
		}
		if principalState.key.KeyPrefix != "" && principalState.key.KeyPrefix == rule.Value {
			matched = true
			break
		}
	}
	if !matched {
		for _, rule := range rules {
			if rule.Kind != "wallet" {
				continue
			}
			wallets, err := principalState.walletSetValue(ctx, s.store)
			if err != nil {
				enrichmentErr = err
				break
			}
			if _, ok := wallets[rule.Value]; ok {
				matched = true
				break
			}
		}
	}
	if !matched {
		for _, rule := range rules {
			if rule.Kind != "email_domain" {
				continue
			}
			identity, err := principalState.identityValue(ctx, s.store)
			if err != nil {
				enrichmentErr = err
				break
			}
			if identity.EmailVerified && identity.EmailDomain == rule.Value && !identity.LastVerifiedAt.IsZero() && now.Sub(identity.LastVerifiedAt) <= s.identityMaxAge {
				matched = true
				break
			}
		}
	}
	if enrichmentErr != nil {
		return false, "", http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	if matched {
		return true, "instance_acl_allow", http.StatusOK, nil
	}
	return false, "no_match", http.StatusOK, nil
}

func (s *Service) loadEvaluationState(ctx context.Context, keyHash string, peerIDs []string, claims *nodecred.Claims) (store.ActiveKey, map[string]store.InstanceInfo, map[string]mesh.PeerObservation, int, error) {
	key, err := s.store.LookupActiveKey(ctx, keyHash)
	if errors.Is(err, store.ErrNotFound) {
		return store.ActiveKey{}, nil, nil, http.StatusUnauthorized, errors.New("unauthorized")
	}
	if err != nil {
		return store.ActiveKey{}, nil, nil, http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	queryIDs := append([]string(nil), peerIDs...)
	if claims != nil && claims.Subject != "" {
		queryIDs = append(queryIDs, claims.Subject)
	}
	managed, err := s.store.ListManagedInstancesByPeerIDs(ctx, dedupePeers(queryIDs))
	if err != nil {
		return store.ActiveKey{}, nil, nil, http.StatusServiceUnavailable, errors.New("service unavailable")
	}
	peers := make(map[string]store.InstanceInfo, len(managed))
	managedPeerIDs := make([]string, 0, len(managed))
	for _, inst := range managed {
		peers[inst.PeerID] = inst
		managedPeerIDs = append(managedPeerIDs, inst.PeerID)
	}
	observations := map[string]mesh.PeerObservation{}
	if len(managedPeerIDs) > 0 {
		observations, err = s.mesh.LookupPeers(ctx, managedPeerIDs)
		if err != nil {
			return store.ActiveKey{}, nil, nil, http.StatusServiceUnavailable, errors.New("service unavailable")
		}
	}
	return key, peers, observations, http.StatusOK, nil
}

func (p *principalContext) identityValue(ctx context.Context, backend aclStore) (store.IdentityInfo, error) {
	if !p.identityLoaded {
		p.identityLoaded = true
		if p.key.UserID != nil {
			p.identity, p.identityErr = backend.GetIdentity(ctx, *p.key.UserID)
			if errors.Is(p.identityErr, store.ErrNotFound) {
				p.identityErr = nil
			}
		}
	}
	return p.identity, p.identityErr
}

func (p *principalContext) walletSetValue(ctx context.Context, backend aclStore) (map[string]struct{}, error) {
	if !p.walletSetLoaded {
		p.walletSetLoaded = true
		p.walletSet = map[string]struct{}{}
		if p.key.UserID != nil {
			wallets, err := backend.GetUserWalletSet(ctx, *p.key.UserID)
			p.walletSetErr = err
			for _, wallet := range wallets {
				p.walletSet[wallet] = struct{}{}
			}
		}
	}
	return p.walletSet, p.walletSetErr
}

func (p *principalContext) primaryWalletValue(ctx context.Context, backend aclStore) string {
	if p.walletsLoaded || p.key.UserID == nil {
		return p.primaryWallet
	}
	p.walletsLoaded = true
	if wallets, err := backend.ListWalletsByUser(ctx, *p.key.UserID); err == nil {
		for _, wallet := range wallets {
			if wallet.Primary {
				p.primaryWallet = wallet.Wallet
				break
			}
		}
	}
	return p.primaryWallet
}

func findServicePolicy(services []store.InstanceService, name string) (store.InstanceService, bool) {
	for _, service := range services {
		if service.ServiceName == name {
			return service, true
		}
	}
	return store.InstanceService{}, false
}

func countService(obs mesh.PeerObservation, name string) int {
	count := 0
	for _, service := range obs.Services {
		if service.Name == name {
			count++
		}
	}
	return count
}

func validKeyHash(v string) bool {
	if len(v) != 64 || strings.ToLower(v) != v {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func validServiceName(v string) bool {
	if len(v) == 0 || len(v) > 80 {
		return false
	}
	for i, r := range v {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func (s *Service) authorized(authz string) bool {
	const prefix = "Bearer "
	if len(authz) < len(prefix) || authz[:len(prefix)] != prefix {
		return false
	}
	candidate := sha256.Sum256([]byte(authz[len(prefix):]))
	return subtle.ConstantTimeCompare(candidate[:], s.internalDigest[:]) == 1
}

func dedupePeers(peerIDs []string) []string {
	out := make([]string, 0, len(peerIDs))
	seen := map[string]struct{}{}
	for _, peerID := range peerIDs {
		peerID = strings.TrimSpace(peerID)
		if peerID == "" {
			continue
		}
		if _, ok := seen[peerID]; ok {
			continue
		}
		seen[peerID] = struct{}{}
		out = append(out, peerID)
	}
	slices.Sort(out)
	return out
}

// WithEvaluatorRateLimit enables per-caller throttling on the v2 evaluator.
// A zero or negative rps disables it. Without a limit, trusted decisions
// (which are never cached client-side) can be driven at unbounded rate by a
// single authorized head, starving the control plane.
func (s *Service) WithEvaluatorRateLimit(rps float64, burst int) *Service {
	s.evalLimiter = newRateLimiter(rps, burst)
	return s
}

// rateLimiter is a minimal, dependency-free per-key token bucket. Each caller
// (a peer id for trusted requests, a key hash for permissionless) gets its own
// bucket.
type rateLimiter struct {
	mu        sync.Mutex
	rps       float64
	burst     int
	now       func() time.Time
	lastSweep time.Time
	bkts      map[string]*rlBucket
}

type rlBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	if rps <= 0 || burst <= 0 {
		return nil
	}
	return &rateLimiter{rps: rps, burst: burst, now: time.Now, bkts: make(map[string]*rlBucket)}
}

// allow reports whether the caller may proceed, consuming one token; when
// it denies, it also returns a Retry-After hint in whole seconds (minimum
// 1). The first request from a key is granted the full burst. Idle buckets
// are reaped every bucketSweepInterval once they have been unused for
// bucketIdleTTL, keeping the per-caller map bounded by active callers.
func (rl *rateLimiter) allow(key string) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	if now.Sub(rl.lastSweep) >= bucketSweepInterval {
		for k, b := range rl.bkts {
			if now.Sub(b.last) >= bucketIdleTTL {
				delete(rl.bkts, k)
			}
		}
		rl.lastSweep = now
	}
	b, ok := rl.bkts[key]
	if !ok {
		b = &rlBucket{tokens: float64(rl.burst), last: now}
		rl.bkts[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rl.rps
		if b.tokens > float64(rl.burst) {
			b.tokens = float64(rl.burst)
		}
		b.last = now
	}
	if b.tokens < 1 {
		retry := int(math.Ceil((1 - b.tokens) / rl.rps))
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	b.tokens--
	return true, 0
}
