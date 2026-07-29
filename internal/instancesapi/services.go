package instancesapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

const servicePolicyCapability = "service-policy-v2"

type servicePolicyResponse struct {
	ID                    int64             `json:"id"`
	ServiceName           string            `json:"service_name"`
	Exposure              string            `json:"exposure"`
	RegionID              *int64            `json:"region_id,omitempty"`
	RegionSlug            *string           `json:"region_slug,omitempty"`
	AccessMode            string            `json:"access_mode"`
	ServicePolicyRevision int64             `json:"service_policy_revision"`
	ObservedPresent       bool              `json:"observed_present"`
	ObservedLastSeenAt    *time.Time        `json:"observed_last_seen_at,omitempty"`
	Rules                 []aclRuleResponse `json:"rules"`
}

type servicePolicyListResponse struct {
	InstanceID              int64                   `json:"instance_id"`
	PolicyScope             string                  `json:"policy_scope"`
	PolicyRevision          int64                   `json:"policy_revision"`
	SupportsServicePolicyV2 bool                    `json:"supports_service_policy_v2"`
	ObservedAt              *time.Time              `json:"observed_at,omitempty"`
	Services                []servicePolicyResponse `json:"services"`
}

type replaceServicesRequest struct {
	PolicyScope           string                      `json:"policy_scope"`
	AcknowledgeScopeReset bool                        `json:"acknowledge_scope_reset"`
	Services              []replaceServicePolicyEntry `json:"services"`
}

type replaceServicePolicyEntry struct {
	ServiceName string          `json:"service_name"`
	Exposure    string          `json:"exposure"`
	RegionID    *int64          `json:"region_id"`
	AccessMode  string          `json:"access_mode"`
	Rules       []store.ACLRule `json:"rules"`
}

type replaceServiceACLRequest struct {
	AccessMode string          `json:"access_mode"`
	Rules      []store.ACLRule `json:"rules"`
}

func (s *Service) handleListServices(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	inst, ok := s.requireCurrentOwnership(w, r, userID, id)
	if !ok {
		return
	}
	loaded, err := s.store.GetInstanceServicesForUser(r.Context(), userID, inst.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	inventory := buildInventory(obs)
	httputil.WriteJSON(w, http.StatusOK, servicePolicyListResponse{
		InstanceID:              loaded.ID,
		PolicyScope:             loaded.PolicyScope,
		PolicyRevision:          loaded.PolicyRevision,
		SupportsServicePolicyV2: inventory.SupportsPolicyV2,
		ObservedAt:              observedAtPtr(inventory),
		Services:                servicePoliciesToResponse(loaded.Services),
	})
}

func (s *Service) handleReplaceServices(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	inst, ok := s.requireCurrentOwnership(w, r, userID, id)
	if !ok {
		return
	}
	var req replaceServicesRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	scope := normalizePolicyScope(req.PolicyScope)
	if scope == "" {
		http.Error(w, "invalid policy_scope", http.StatusBadRequest)
		return
	}
	services, err := normalizeServiceEntries(req.Services)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	inventory := buildInventory(obs)
	if scope == store.PolicyScopeService && !inventory.SupportsPolicyV2 {
		http.Error(w, "peer does not advertise service-policy-v2 capability", http.StatusConflict)
		return
	}
	for _, observed := range inventory.Services {
		if observed.Count > 1 {
			http.Error(w, "duplicate live service names must be resolved first", http.StatusConflict)
			return
		}
	}
	updated, err := s.store.ReplaceInstanceServicePolicy(r.Context(), userID, id, store.ReplaceServicePolicyInput{
		PolicyScope:           scope,
		AcknowledgeScopeReset: req.AcknowledgeScopeReset,
		Inventory:             inventory,
		Services:              services,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, store.ErrConflict):
			http.Error(w, "service policy transition rejected", http.StatusConflict)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	httputil.WriteJSON(w, http.StatusOK, servicePolicyListResponse{
		InstanceID:              updated.ID,
		PolicyScope:             updated.PolicyScope,
		PolicyRevision:          updated.PolicyRevision,
		SupportsServicePolicyV2: inventory.SupportsPolicyV2,
		ObservedAt:              observedAtPtr(inventory),
		Services:                servicePoliciesToResponse(updated.Services),
	})
}

func (s *Service) handleReplaceServiceACL(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	id, ok := parseInstanceID(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireCurrentOwnership(w, r, userID, id); !ok {
		return
	}
	serviceID, err := strconv.ParseInt(r.PathValue("service_id"), 10, 64)
	if err != nil || serviceID <= 0 {
		http.Error(w, "invalid service id", http.StatusBadRequest)
		return
	}
	var req replaceServiceACLRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	accessMode := normalizeServiceAccessMode(req.AccessMode)
	if accessMode == "" {
		http.Error(w, "invalid access_mode", http.StatusBadRequest)
		return
	}
	rules, err := normalizeRules(req.Rules)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := s.store.ReplaceInstanceServiceACL(r.Context(), userID, id, serviceID, accessMode, rules)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	httputil.WriteJSON(w, http.StatusOK, servicePolicyToResponse(updated))
}

func normalizePolicyScope(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case store.PolicyScopePeer:
		return store.PolicyScopePeer
	case store.PolicyScopeService:
		return store.PolicyScopeService
	default:
		return ""
	}
}

func normalizeServiceAccessMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case store.AccessModeInherit:
		return store.AccessModeInherit
	case store.AccessModePublic:
		return store.AccessModePublic
	case store.AccessModeRestricted:
		return store.AccessModeRestricted
	default:
		return ""
	}
}

func normalizeServiceEntries(in []replaceServicePolicyEntry) ([]store.InstanceService, error) {
	seen := map[string]struct{}{}
	out := make([]store.InstanceService, 0, len(in))
	for _, item := range in {
		name := strings.TrimSpace(item.ServiceName)
		if !validServiceName(name) {
			return nil, errors.New("invalid service_name")
		}
		if _, ok := seen[name]; ok {
			return nil, errors.New("duplicate service_name")
		}
		seen[name] = struct{}{}
		exposure := strings.ToLower(strings.TrimSpace(item.Exposure))
		switch exposure {
		case store.ExposurePermissionless, store.ExposureTrustedRegion, store.ExposureDisabled:
		default:
			return nil, errors.New("invalid exposure")
		}
		accessMode := normalizeServiceAccessMode(item.AccessMode)
		if accessMode == "" {
			return nil, errors.New("invalid access_mode")
		}
		if exposure == store.ExposureTrustedRegion && item.RegionID == nil {
			return nil, errors.New("trusted_region exposure requires region_id")
		}
		if exposure != store.ExposureTrustedRegion && item.RegionID != nil {
			return nil, errors.New("region_id is only allowed for trusted_region exposure")
		}
		rules, err := normalizeRules(item.Rules)
		if err != nil {
			return nil, err
		}
		out = append(out, store.InstanceService{
			ServiceName: name,
			Exposure:    exposure,
			RegionID:    item.RegionID,
			AccessMode:  accessMode,
			Rules:       rules,
		})
	}
	return out, nil
}

func buildInventory(obs mesh.PeerObservation) store.ServiceInventory {
	inventory := store.ServiceInventory{}
	if obs.ObservedAt.IsZero() {
		return inventory
	}
	inventory.ObservedAt = obs.ObservedAt.UTC()
	if _, ok := obs.Capabilities[servicePolicyCapability]; ok {
		inventory.SupportsPolicyV2 = true
	}
	counts := make(map[string]int)
	for _, observed := range obs.Services {
		counts[observed.Name]++
	}
	for name, count := range counts {
		inventory.Services = append(inventory.Services, store.ObservedService{Name: name, Count: count})
	}
	return inventory
}

func observedAtPtr(inventory store.ServiceInventory) *time.Time {
	if inventory.ObservedAt.IsZero() {
		return nil
	}
	ts := inventory.ObservedAt
	return &ts
}

func servicePoliciesToResponse(items []store.InstanceService) []servicePolicyResponse {
	out := make([]servicePolicyResponse, 0, len(items))
	for _, item := range items {
		out = append(out, servicePolicyToResponse(item))
	}
	return out
}

func servicePolicyToResponse(item store.InstanceService) servicePolicyResponse {
	rules := make([]aclRuleResponse, 0, len(item.Rules))
	for _, rule := range item.Rules {
		rules = append(rules, aclRuleResponse{Kind: rule.Kind, Value: rule.Value})
	}
	return servicePolicyResponse{
		ID:                    item.ID,
		ServiceName:           item.ServiceName,
		Exposure:              item.Exposure,
		RegionID:              item.RegionID,
		RegionSlug:            item.RegionSlug,
		AccessMode:            item.AccessMode,
		ServicePolicyRevision: item.ServicePolicyRevision,
		ObservedPresent:       item.ObservedPresent,
		ObservedLastSeenAt:    item.ObservedLastSeenAt,
		Rules:                 rules,
	}
}

func validServiceName(v string) bool {
	if v == "" || len(v) > 80 {
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
