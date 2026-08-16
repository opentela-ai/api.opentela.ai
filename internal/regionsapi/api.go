package regionsapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

const (
	maxBodyBytes     = 16 << 10
	defaultInviteTTL = 15 * time.Minute
	maxAdmissionNote = 256
)

type regionStore interface {
	CreateRegion(ctx context.Context, in store.RegionInfo) (store.RegionInfo, error)
	ListRegionsByOwner(ctx context.Context, ownerAccountID string) ([]store.RegionInfo, error)
	GetRegionByIDForOwner(ctx context.Context, ownerAccountID string, regionID int64) (store.RegionInfo, error)
	UpdateRegion(ctx context.Context, ownerAccountID string, regionID int64, name, status string) (store.RegionInfo, error)
	DeleteRegion(ctx context.Context, ownerAccountID string, regionID int64) (bool, error)
	ListRegionMembershipsByOwner(ctx context.Context, ownerAccountID string, regionID int64) ([]store.RegionMembership, []store.RegionInvitation, error)
	CreateRegionInvitation(ctx context.Context, ownerAccountID string, regionID, instanceID int64, nodeRole, token string, expiresAt time.Time) (store.RegionInvitation, error)
	AcceptRegionInvitation(ctx context.Context, actorAccountID string, regionID, instanceID int64, token string, ownershipVerifiedAt time.Time) (store.RegionMembership, error)
	UpdateMembershipState(ctx context.Context, ownerAccountID string, regionID, instanceID int64, nodeRole, status string, expiresAt *time.Time, ownershipVerifiedAt *time.Time) (store.RegionMembership, error)
	ReleaseMembership(ctx context.Context, actorAccountID string, instanceID int64) (bool, error)
	CancelInvitationForOwner(ctx context.Context, ownerAccountID string, regionID, instanceID int64) (bool, error)
	GetInstanceByID(ctx context.Context, id int64) (store.InstanceInfo, error)
	GetInstanceByIDForUser(ctx context.Context, accountID string, id int64) (store.InstanceInfo, error)
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
}

type regionMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

type Service struct {
	store           regionStore
	mesh            regionMesh
	ownershipMaxAge time.Duration
	now             func() time.Time
}

func New(pg regionStore, meshClient regionMesh, ownershipMaxAge time.Duration) *Service {
	return &Service{store: pg, mesh: meshClient, ownershipMaxAge: ownershipMaxAge, now: time.Now}
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/regions", s.handleCreateRegion)
	mux.HandleFunc("GET /manage/regions", s.handleListRegions)
	mux.HandleFunc("GET /manage/regions/{region_id}", s.handleGetRegion)
	mux.HandleFunc("PATCH /manage/regions/{region_id}", s.handlePatchRegion)
	mux.HandleFunc("DELETE /manage/regions/{region_id}", s.handleDeleteRegion)
	mux.HandleFunc("POST /manage/regions/{region_id}/members", s.handleCreateMember)
	mux.HandleFunc("GET /manage/regions/{region_id}/members", s.handleListMembers)
	mux.HandleFunc("PATCH /manage/regions/{region_id}/members/{instance_id}", s.handlePatchMember)
	mux.HandleFunc("DELETE /manage/regions/{region_id}/members/{instance_id}", s.handleDeleteMember)
	return mux
}

type regionResponse struct {
	ID             int64                `json:"id"`
	Slug           string               `json:"slug"`
	Name           string               `json:"name"`
	Status         string               `json:"status"`
	RegionRevision int64                `json:"region_revision"`
	Members        []membershipResponse `json:"members"`
	Invitations    []invitationResponse `json:"invitations"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

type membershipResponse struct {
	InstanceID          int64      `json:"instance_id"`
	PeerID              string     `json:"peer_id"`
	Label               string     `json:"label"`
	RegionID            int64      `json:"region_id"`
	RegionSlug          string     `json:"region_slug"`
	RegionStatus        string     `json:"region_status"`
	NodeRole            string     `json:"node_role"`
	Status              string     `json:"status"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	OwnershipVerifiedAt *time.Time `json:"ownership_verified_at,omitempty"`
	MembershipRevision  int64      `json:"membership_revision"`
	TrustedServiceCount int        `json:"trusted_service_count"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type invitationResponse struct {
	ID          int64      `json:"id"`
	RegionID    int64      `json:"region_id"`
	RegionSlug  string     `json:"region_slug"`
	InstanceID  int64      `json:"instance_id"`
	PeerID      string     `json:"peer_id"`
	Label       string     `json:"label"`
	Status      string     `json:"status"`
	NodeRole    string     `json:"node_role"`
	ExpiresAt   time.Time  `json:"expires_at"`
	AcceptedAt  *time.Time `json:"accepted_at,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Token       string     `json:"acceptance_token,omitempty"`
}

type createRegionRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type patchRegionRequest struct {
	Name   *string `json:"name"`
	Status *string `json:"status"`
}

type createMemberRequest struct {
	InstanceID      int64      `json:"instance_id"`
	NodeRole        string     `json:"node_role"`
	ExpiresAt       *time.Time `json:"expires_at"`
	AdmissionReason string     `json:"admission_reason"`
	AutoAccept      bool       `json:"auto_accept"`
}

type patchMemberRequest struct {
	Action          string     `json:"action"`
	NodeRole        *string    `json:"node_role"`
	Status          *string    `json:"status"`
	ExpiresAt       *time.Time `json:"expires_at"`
	AcceptanceToken string     `json:"acceptance_token"`
}

func (s *Service) handleCreateRegion(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	var req createRegionRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	slug := normalizeSlug(req.Slug)
	if slug == "" || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid region payload", http.StatusBadRequest)
		return
	}
	created, err := s.store.CreateRegion(r.Context(), store.RegionInfo{
		Slug:           slug,
		Name:           strings.TrimSpace(req.Name),
		OwnerAccountID: userID,
		Status:         "active",
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			http.Error(w, "region slug already exists", http.StatusConflict)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, regionToResponse(created))
}

func (s *Service) handleListRegions(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	items, err := s.store.ListRegionsByOwner(r.Context(), userID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	out := make([]regionResponse, 0, len(items))
	for _, item := range items {
		region := regionToResponse(item)
		members, invitations, err := s.store.ListRegionMembershipsByOwner(r.Context(), userID, item.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		for _, member := range members {
			region.Members = append(region.Members, membershipToResponse(member))
		}
		for _, invitation := range invitations {
			region.Invitations = append(region.Invitations, invitationToResponse(invitation))
		}
		out = append(out, region)
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"regions": out})
}

func (s *Service) handleGetRegion(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return
	}
	item, err := s.store.GetRegionByIDForOwner(r.Context(), userID, regionID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, regionToResponse(item))
}

func (s *Service) handlePatchRegion(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetRegionByIDForOwner(r.Context(), userID, regionID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req patchRegionRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	name := current.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
	}
	status := current.Status
	if req.Status != nil {
		switch strings.ToLower(strings.TrimSpace(*req.Status)) {
		case "active", "disabled":
			status = strings.ToLower(strings.TrimSpace(*req.Status))
		default:
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
	}
	updated, err := s.store.UpdateRegion(r.Context(), userID, regionID, name, status)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, regionToResponse(updated))
}

func (s *Service) handleDeleteRegion(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return
	}
	changed, err := s.store.DeleteRegion(r.Context(), userID, regionID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			http.Error(w, "region still has memberships or bindings", http.StatusConflict)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if !changed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleCreateMember(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return
	}
	var req createMemberRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	nodeRole := normalizeNodeRole(req.NodeRole)
	if nodeRole == "" || req.InstanceID <= 0 || len(req.AdmissionReason) > maxAdmissionNote {
		http.Error(w, "invalid member payload", http.StatusBadRequest)
		return
	}
	inst, err := s.store.GetInstanceByIDForUser(r.Context(), userID, req.InstanceID)
	if err == nil && req.AutoAccept {
		membership, err := s.acceptMembership(r.Context(), userID, regionID, inst, nodeRole, req.ExpiresAt)
		if err == nil {
			httputil.WriteJSON(w, http.StatusCreated, membershipToResponse(membership))
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			if errors.Is(err, store.ErrConflict) {
				http.Error(w, "membership rejected", http.StatusConflict)
			} else {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
	}
	token, err := randomToken(24)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	expiresAt := s.now().UTC().Add(defaultInviteTTL)
	if req.ExpiresAt != nil {
		expiresAt = req.ExpiresAt.UTC()
	}
	invitation, err := s.store.CreateRegionInvitation(r.Context(), userID, regionID, req.InstanceID, nodeRole, token, expiresAt)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	resp := invitationToResponse(invitation)
	resp.Token = token
	httputil.WriteJSON(w, http.StatusCreated, resp)
}

func (s *Service) handleListMembers(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return
	}
	members, invitations, err := s.store.ListRegionMembershipsByOwner(r.Context(), userID, regionID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	memberOut := make([]membershipResponse, 0, len(members))
	for _, member := range members {
		memberOut = append(memberOut, membershipToResponse(member))
	}
	inviteOut := make([]invitationResponse, 0, len(invitations))
	for _, invitation := range invitations {
		inviteOut = append(inviteOut, invitationToResponse(invitation))
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"members": memberOut, "invitations": inviteOut})
}

func (s *Service) handlePatchMember(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, instanceID, ok := parseRegionAndInstanceID(w, r)
	if !ok {
		return
	}
	var req patchMemberRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "accept":
		inst, err := s.store.GetInstanceByIDForUser(r.Context(), userID, instanceID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			http.Error(w, "peer ownership currently unavailable", http.StatusConflict)
			return
		}
		membership, err := s.store.AcceptRegionInvitation(r.Context(), userID, regionID, instanceID, req.AcceptanceToken, obs.ObservedAt)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				http.Error(w, "not found", http.StatusNotFound)
			case errors.Is(err, store.ErrChallengeExpired):
				http.Error(w, "invitation expired", http.StatusConflict)
			case errors.Is(err, store.ErrRegionMigrationConflict):
				http.Error(w, "remove trusted service bindings from the current region before migrating", http.StatusConflict)
			default:
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		httputil.WriteJSON(w, http.StatusOK, membershipToResponse(membership))
	default:
		members, _, err := s.store.ListRegionMembershipsByOwner(r.Context(), userID, regionID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		current, found := findMembership(members, instanceID)
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		role := current.NodeRole
		if req.NodeRole != nil {
			role = normalizeNodeRole(*req.NodeRole)
			if role == "" {
				http.Error(w, "invalid node_role", http.StatusBadRequest)
				return
			}
		}
		status := current.Status
		if req.Status != nil {
			status = normalizeManagedMembershipStatus(*req.Status)
			if status == "" {
				http.Error(w, "invalid status", http.StatusBadRequest)
				return
			}
		} else {
			switch action {
			case "suspend":
				status = "suspended"
			case "reactivate":
				status = "active"
			case "revoke":
				status = "revoked"
			case "":
				if req.NodeRole == nil && req.ExpiresAt == nil {
					http.Error(w, "invalid action", http.StatusBadRequest)
					return
				}
			default:
				http.Error(w, "invalid action", http.StatusBadRequest)
				return
			}
		}

		expiresAt := current.ExpiresAt
		if req.ExpiresAt != nil {
			expiresAt = req.ExpiresAt
		} else if status == "active" && current.Status == "expired" {
			expiresAt = nil
		}
		if expiresAt != nil && !s.now().UTC().Before(expiresAt.UTC()) && status == "active" {
			http.Error(w, "active membership expiry must be in the future", http.StatusBadRequest)
			return
		}
		var ownershipVerifiedAt *time.Time
		if status == "active" && current.Status != "active" {
			inst, err := s.store.GetInstanceByID(r.Context(), instanceID)
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			obs, err := s.mesh.LookupPeer(r.Context(), inst.PeerID)
			if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
				http.Error(w, "peer ownership currently unavailable", http.StatusConflict)
				return
			}
			verifiedAt := obs.ObservedAt.UTC()
			ownershipVerifiedAt = &verifiedAt
		}
		updated, err := s.store.UpdateMembershipState(r.Context(), userID, regionID, instanceID, role, status, expiresAt, ownershipVerifiedAt)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		httputil.WriteJSON(w, http.StatusOK, membershipToResponse(updated))
	}
}

func findMembership(members []store.RegionMembership, instanceID int64) (store.RegionMembership, bool) {
	for _, membership := range members {
		if membership.InstanceID == instanceID {
			return membership, true
		}
	}
	return store.RegionMembership{}, false
}

func (s *Service) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	userID, _ := principal.UserID(r.Context())
	regionID, instanceID, ok := parseRegionAndInstanceID(w, r)
	if !ok {
		return
	}
	if changed, err := s.store.ReleaseMembership(r.Context(), userID, instanceID); err == nil {
		if changed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	} else if errors.Is(err, store.ErrConflict) {
		http.Error(w, "trusted service bindings must be removed first", http.StatusConflict)
		return
	}
	changed, err := s.store.CancelInvitationForOwner(r.Context(), userID, regionID, instanceID)
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

func (s *Service) acceptMembership(ctx context.Context, userID string, regionID int64, inst store.InstanceInfo, nodeRole string, expiresAt *time.Time) (store.RegionMembership, error) {
	obs, err := s.mesh.LookupPeer(ctx, inst.PeerID)
	if err != nil {
		return store.RegionMembership{}, err
	}
	if !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
		return store.RegionMembership{}, store.ErrConflict
	}
	token, err := randomToken(24)
	if err != nil {
		return store.RegionMembership{}, err
	}
	expiry := s.now().UTC().Add(defaultInviteTTL)
	if expiresAt != nil {
		expiry = expiresAt.UTC()
	}
	if _, err := s.store.CreateRegionInvitation(ctx, userID, regionID, inst.ID, nodeRole, token, expiry); err != nil && !errors.Is(err, store.ErrConflict) {
		return store.RegionMembership{}, err
	}
	return s.store.AcceptRegionInvitation(ctx, userID, regionID, inst.ID, token, obs.ObservedAt)
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

func regionToResponse(item store.RegionInfo) regionResponse {
	return regionResponse{
		ID:             item.ID,
		Slug:           item.Slug,
		Name:           item.Name,
		Status:         item.Status,
		RegionRevision: item.RegionRevision,
		Members:        []membershipResponse{},
		Invitations:    []invitationResponse{},
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
	}
}

func membershipToResponse(item store.RegionMembership) membershipResponse {
	return membershipResponse{
		InstanceID:          item.InstanceID,
		PeerID:              item.PeerID,
		Label:               item.Label,
		RegionID:            item.RegionID,
		RegionSlug:          item.RegionSlug,
		RegionStatus:        item.RegionStatus,
		NodeRole:            item.NodeRole,
		Status:              item.Status,
		ExpiresAt:           item.ExpiresAt,
		OwnershipVerifiedAt: item.OwnershipVerifiedAt,
		MembershipRevision:  item.MembershipRevision,
		TrustedServiceCount: item.TrustedServiceCount,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
	}
}

func invitationToResponse(item store.RegionInvitation) invitationResponse {
	return invitationResponse{
		ID:          item.ID,
		RegionID:    item.RegionID,
		RegionSlug:  item.RegionSlug,
		InstanceID:  item.InstanceID,
		PeerID:      item.PeerID,
		Label:       item.Label,
		Status:      item.Status,
		NodeRole:    item.NodeRole,
		ExpiresAt:   item.ExpiresAt,
		AcceptedAt:  item.AcceptedAt,
		CancelledAt: item.CancelledAt,
		CreatedAt:   item.CreatedAt,
		UpdatedAt:   item.UpdatedAt,
	}
}

func parseRegionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("region_id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid region id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func parseRegionAndInstanceID(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	regionID, ok := parseRegionID(w, r)
	if !ok {
		return 0, 0, false
	}
	instanceID, err := strconv.ParseInt(r.PathValue("instance_id"), 10, 64)
	if err != nil || instanceID <= 0 {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return 0, 0, false
	}
	return regionID, instanceID, true
}

func normalizeSlug(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) == 0 || len(v) > 64 {
		return ""
	}
	for i, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(v)-1:
		default:
			return ""
		}
	}
	return v
}

func normalizeNodeRole(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "worker":
		return "worker"
	case "head":
		return "head"
	case "combined":
		return "combined"
	default:
		return ""
	}
}

func normalizeManagedMembershipStatus(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "active":
		return "active"
	case "suspended":
		return "suspended"
	case "revoked":
		return "revoked"
	default:
		return ""
	}
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "conflict", http.StatusConflict)
	default:
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}
}
