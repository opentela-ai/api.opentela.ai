package instancesapi

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/identity"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
	"github.com/opentela-ai/api/internal/walletsapi"
)

const (
	maxBodyBytes = 16 << 10
	maxRules     = 100
	maxLabelLen  = 200
	maxPeerIDLen = 128
)

type Service struct {
	store           instanceStore
	mesh            instanceMesh
	identityMaxAge  time.Duration
	ownershipMaxAge time.Duration
	now             func() time.Time
}

type instanceResponse struct {
	ID                  int64             `json:"id"`
	PeerID              string            `json:"peer_id"`
	Label               string            `json:"label"`
	OwnerWallet         string            `json:"owner_wallet"`
	Mode                string            `json:"mode"`
	PolicyScope         string            `json:"policy_scope"`
	PolicyRevision      int64             `json:"policy_revision"`
	OwnershipStatus     string            `json:"ownership_status"`
	OwnershipObservedAt *time.Time        `json:"ownership_observed_at,omitempty"`
	Online              bool              `json:"online"`
	Rules               []aclRuleResponse `json:"rules"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type aclRuleResponse struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
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
	Instances []instanceResponse `json:"instances"`
	Identity  identitySnapshot   `json:"identity"`
}

type instanceStore interface {
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
	CreateInstance(ctx context.Context, in store.InstanceInfo) (store.InstanceInfo, error)
	ReclaimInstance(ctx context.Context, id int64, accountID, ownerWallet string, observedWallet *string, observedAt *time.Time) (store.InstanceInfo, error)
	ListInstancesByUser(ctx context.Context, accountID string) ([]store.InstanceInfo, error)
	GetIdentity(ctx context.Context, accountID string) (store.IdentityInfo, error)
	GetInstanceByIDForUser(ctx context.Context, accountID string, id int64) (store.InstanceInfo, error)
	UpdateInstanceMetadata(ctx context.Context, accountID string, id int64, label, mode, ownershipStatus string, observedWallet *string, observedAt *time.Time) (store.InstanceInfo, error)
	ReplaceInstanceACL(ctx context.Context, accountID string, id int64, mode, ownershipStatus string, observedWallet *string, observedAt *time.Time, rules []store.ACLRule) (store.InstanceInfo, error)
	DeleteInstanceByIDForUser(ctx context.Context, accountID string, id int64) (bool, error)
	GetUserWalletSet(ctx context.Context, accountID string) ([]string, error)
	GetInstanceServicesForUser(ctx context.Context, accountID string, instanceID int64) (store.InstanceInfo, error)
	ReplaceInstanceServicePolicy(ctx context.Context, accountID string, instanceID int64, in store.ReplaceServicePolicyInput) (store.InstanceInfo, error)
	ReplaceInstanceServiceACL(ctx context.Context, accountID string, instanceID, serviceID int64, accessMode string, rules []store.ACLRule) (store.InstanceService, error)
}

type instanceMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
	OnlineStatus(ctx context.Context, peerIDs []string) (map[string]bool, error)
}

func New(pg instanceStore, meshClient instanceMesh, identityMaxAge, ownershipMaxAge time.Duration) *Service {
	return &Service{
		store:           pg,
		mesh:            meshClient,
		identityMaxAge:  identityMaxAge,
		ownershipMaxAge: ownershipMaxAge,
		now:             time.Now,
	}
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/instances", s.handleCreate)
	mux.HandleFunc("GET /manage/instances", s.handleList)
	mux.HandleFunc("PATCH /manage/instances/{id}", s.handlePatch)
	mux.HandleFunc("PUT /manage/instances/{id}/acl", s.handleReplaceACL)
	mux.HandleFunc("GET /manage/instances/{id}/services", s.handleListServices)
	mux.HandleFunc("PUT /manage/instances/{id}/services", s.handleReplaceServices)
	mux.HandleFunc("PUT /manage/instances/{id}/services/{service_id}/acl", s.handleReplaceServiceACL)
	mux.HandleFunc("DELETE /manage/instances/{id}", s.handleDelete)
	return mux
}

type createRequest struct {
	PeerID string `json:"peer_id"`
	Label  string `json:"label"`
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	var req createRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	peerID := strings.TrimSpace(req.PeerID)
	if !validPeerID(peerID) {
		http.Error(w, "invalid peer_id", http.StatusBadRequest)
		return
	}
	if len(req.Label) > maxLabelLen {
		http.Error(w, "label too long", http.StatusBadRequest)
		return
	}
	obs, err := s.mesh.LookupPeer(r.Context(), peerID)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if !errors.Is(err, mesh.ErrPeerUnavailable) && !errors.Is(err, mesh.ErrPeerUnverifiable) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "peer ownership could not be verified", status)
		return
	}
	if !s.observationFresh(obs) {
		http.Error(w, "peer ownership could not be verified", http.StatusUnprocessableEntity)
		return
	}
	wallets, err := walletsapi.UserWalletSet(r.Context(), s.store, userID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, ok := wallets[obs.Wallet]; !ok {
		http.Error(w, "peer wallet is not linked", http.StatusUnprocessableEntity)
		return
	}
	existing, err := s.store.GetInstanceByPeerID(r.Context(), peerID)
	switch {
	case err == nil:
		if existing.OwnerWallet == obs.Wallet {
			http.Error(w, "peer already claimed", http.StatusConflict)
			return
		}
		reclaimed, reclaimErr := s.store.ReclaimInstance(r.Context(), existing.ID, userID, obs.Wallet, &obs.Wallet, &obs.ObservedAt)
		if reclaimErr != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		httputil.WriteJSON(w, http.StatusCreated, instanceToResponse(reclaimed, true))
		return
	case !errors.Is(err, store.ErrNotFound):
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	created, err := s.store.CreateInstance(r.Context(), store.InstanceInfo{
		AccountID:           userID,
		PeerID:              peerID,
		Label:               req.Label,
		OwnerWallet:         obs.Wallet,
		AccessMode:          "restricted",
		OwnershipStatus:     "active",
		ObservedWallet:      &obs.Wallet,
		OwnershipObservedAt: &obs.ObservedAt,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			http.Error(w, "peer already claimed", http.StatusConflict)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, instanceToResponse(created, true))
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	instances, err := s.store.ListInstancesByUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	peerIDs := make([]string, 0, len(instances))
	for _, inst := range instances {
		peerIDs = append(peerIDs, inst.PeerID)
	}
	statuses := map[string]bool{}
	if len(peerIDs) > 0 {
		statuses, err = s.mesh.OnlineStatus(r.Context(), peerIDs)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	out := make([]instanceResponse, 0, len(instances))
	for _, inst := range instances {
		out = append(out, instanceToResponse(inst, statuses[inst.PeerID]))
	}
	httputil.WriteJSON(w, http.StatusOK, listResponse{
		Instances: out,
		Identity:  s.identitySnapshot(r.Context(), userID),
	})
}

type patchRequest struct {
	Label *string `json:"label"`
	Mode  string  `json:"mode"`
}

func (s *Service) handlePatch(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	var req patchRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	mode := ""
	if strings.TrimSpace(req.Mode) != "" {
		mode = normalizeMode(req.Mode)
		if mode == "" {
			http.Error(w, "invalid mode", http.StatusBadRequest)
			return
		}
	}
	if req.Label != nil && len(*req.Label) > maxLabelLen {
		http.Error(w, "label too long", http.StatusBadRequest)
		return
	}
	inst, ok := s.requireCurrentOwnership(w, r, userID, id)
	if !ok {
		return
	}
	if mode == "" {
		mode = inst.AccessMode
	}
	label := inst.Label
	if req.Label != nil {
		label = *req.Label
	}
	updated, err := s.store.UpdateInstanceMetadata(r.Context(), userID, id, label, mode, inst.OwnershipStatus, inst.ObservedWallet, inst.OwnershipObservedAt)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, instanceToResponse(updated, true))
}

type aclRequest struct {
	Mode  string          `json:"mode"`
	Rules []store.ACLRule `json:"rules"`
}

func (s *Service) handleReplaceACL(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	var req aclRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	mode := normalizeMode(req.Mode)
	if mode == "" {
		http.Error(w, "invalid mode", http.StatusBadRequest)
		return
	}
	rules, err := normalizeRules(req.Rules)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	inst, ok := s.requireCurrentOwnership(w, r, userID, id)
	if !ok {
		return
	}
	updated, err := s.store.ReplaceInstanceACL(r.Context(), userID, id, mode, inst.OwnershipStatus, inst.ObservedWallet, inst.OwnershipObservedAt, rules)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, instanceToResponse(updated, true))
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	changed, err := s.store.DeleteInstanceByIDForUser(r.Context(), userID, id)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !changed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseInstanceID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func (s *Service) requireCurrentOwnership(w http.ResponseWriter, r *http.Request, userID string, id int64) (store.InstanceInfo, bool) {
	inst, err := s.store.GetInstanceByIDForUser(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return store.InstanceInfo{}, false
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return store.InstanceInfo{}, false
	}
	obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
	if err != nil {
		updated, upErr := s.store.UpdateInstanceMetadata(r.Context(), userID, id, inst.Label, inst.AccessMode, "unavailable", nil, nil)
		if upErr == nil {
			inst = updated
		}
		http.Error(w, "peer ownership currently unavailable", http.StatusConflict)
		return store.InstanceInfo{}, false
	}
	if obs.Wallet != inst.OwnerWallet {
		updated, upErr := s.store.UpdateInstanceMetadata(r.Context(), userID, id, inst.Label, inst.AccessMode, "mismatch", &obs.Wallet, &obs.ObservedAt)
		if upErr == nil {
			inst = updated
		}
		http.Error(w, "peer ownership no longer matches", http.StatusConflict)
		return store.InstanceInfo{}, false
	}
	if !s.observationFresh(obs) {
		updated, upErr := s.store.UpdateInstanceMetadata(r.Context(), userID, id, inst.Label, inst.AccessMode, "unavailable", &obs.Wallet, &obs.ObservedAt)
		if upErr == nil {
			inst = updated
		}
		http.Error(w, "peer ownership currently unavailable", http.StatusConflict)
		return store.InstanceInfo{}, false
	}
	inst.OwnershipStatus = "active"
	inst.ObservedWallet = &obs.Wallet
	inst.OwnershipObservedAt = &obs.ObservedAt
	return inst, true
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func normalizeMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "public":
		return "public"
	case "restricted":
		return "restricted"
	default:
		return ""
	}
}

func normalizeRules(in []store.ACLRule) ([]store.ACLRule, error) {
	if len(in) > maxRules {
		return nil, errors.New("too many rules")
	}
	seen := map[string]struct{}{}
	out := make([]store.ACLRule, 0, len(in))
	for _, rule := range in {
		kind := strings.TrimSpace(rule.Kind)
		value := strings.TrimSpace(rule.Value)
		switch kind {
		case "email_domain":
			domain, err := identity.NormalizeRuleDomain(value)
			if err != nil {
				return nil, err
			}
			value = domain
		case "wallet":
			wallet, err := solana.NormalizeWallet(value)
			if err != nil {
				return nil, err
			}
			value = wallet
		case "api_key":
			prefix, err := normalizeKeyPrefix(value)
			if err != nil {
				return nil, err
			}
			value = prefix
		default:
			return nil, errors.New("invalid rule kind")
		}
		key := kind + "\x00" + value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, store.ACLRule{Kind: kind, Value: value})
	}
	slices.SortFunc(out, func(a, b store.ACLRule) int {
		if a.Kind == b.Kind {
			return strings.Compare(a.Value, b.Value)
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	return out, nil
}

func normalizeKeyPrefix(v string) (string, error) {
	v = strings.ToLower(v)
	if len(v) != 11 || !strings.HasPrefix(v, "sk-") || !isLowerHex(v[3:]) {
		return "", errors.New("api_key rules must be the non-secret key prefix: sk- plus 8 hex characters")
	}
	return v, nil
}

func isLowerHex(v string) bool {
	for _, c := range v {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validPeerID(v string) bool {
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

func instanceToResponse(inst store.InstanceInfo, online bool) instanceResponse {
	rules := make([]aclRuleResponse, 0, len(inst.Rules))
	for _, rule := range inst.Rules {
		rules = append(rules, aclRuleResponse{Kind: rule.Kind, Value: rule.Value})
	}
	return instanceResponse{
		ID:                  inst.ID,
		PeerID:              inst.PeerID,
		Label:               inst.Label,
		OwnerWallet:         inst.OwnerWallet,
		Mode:                inst.AccessMode,
		PolicyScope:         inst.PolicyScope,
		PolicyRevision:      inst.PolicyRevision,
		OwnershipStatus:     inst.OwnershipStatus,
		OwnershipObservedAt: inst.OwnershipObservedAt,
		Online:              online,
		Rules:               rules,
		CreatedAt:           inst.CreatedAt,
		UpdatedAt:           inst.UpdatedAt,
	}
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
