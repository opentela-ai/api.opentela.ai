package regionsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

type regionStoreStub struct {
	regionStore
	regions          []store.RegionInfo
	members          map[int64][]store.RegionMembership
	invitations      map[int64][]store.RegionInvitation
	instance         store.InstanceInfo
	updated          store.RegionMembership
	updatedRole      string
	updatedStatus    string
	updatedExpiry    *time.Time
	updatedOwnership *time.Time
	err              error
	acceptErr        error
}

func (s *regionStoreStub) ListRegionsByOwner(context.Context, string) ([]store.RegionInfo, error) {
	return s.regions, s.err
}

func (s *regionStoreStub) ListRegionMembershipsByOwner(_ context.Context, _ string, regionID int64) ([]store.RegionMembership, []store.RegionInvitation, error) {
	return s.members[regionID], s.invitations[regionID], s.err
}

func (s *regionStoreStub) GetInstanceByID(context.Context, int64) (store.InstanceInfo, error) {
	return s.instance, s.err
}

func (s *regionStoreStub) GetInstanceByIDForUser(context.Context, string, int64) (store.InstanceInfo, error) {
	return s.instance, s.err
}

func (s *regionStoreStub) AcceptRegionInvitation(_ context.Context, _ string, _, _ int64, _ string, _ time.Time) (store.RegionMembership, error) {
	if s.acceptErr != nil {
		return store.RegionMembership{}, s.acceptErr
	}
	return s.updated, nil
}

func (s *regionStoreStub) UpdateMembershipState(_ context.Context, _ string, _, _ int64, role, status string, expiresAt *time.Time, ownershipVerifiedAt *time.Time) (store.RegionMembership, error) {
	s.updatedRole = role
	s.updatedStatus = status
	s.updatedExpiry = expiresAt
	s.updatedOwnership = ownershipVerifiedAt
	updated := s.updated
	updated.NodeRole = role
	updated.Status = status
	updated.ExpiresAt = expiresAt
	updated.OwnershipVerifiedAt = ownershipVerifiedAt
	return updated, s.err
}

type regionMeshStub struct {
	observation mesh.PeerObservation
	err         error
}

func (s regionMeshStub) LookupPeer(context.Context, string) (mesh.PeerObservation, error) {
	return s.observation, s.err
}

func TestListRegionsIncludesMembersAndInvitations(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pg := &regionStoreStub{
		regions: []store.RegionInfo{{ID: 8, Slug: "trusted-eu", Name: "Trusted EU", Status: "active", RegionRevision: 5, CreatedAt: now, UpdatedAt: now}},
		members: map[int64][]store.RegionMembership{8: {{
			InstanceID: 77, PeerID: "peer-worker", Label: "Zurich GPU", RegionID: 8, RegionSlug: "trusted-eu", RegionStatus: "active", NodeRole: "worker", Status: "active", MembershipRevision: 2, TrustedServiceCount: 1,
		}}},
		invitations: map[int64][]store.RegionInvitation{8: {{
			ID: 101, InstanceID: 99, PeerID: "peer-head", Label: "Bern head", RegionID: 8, RegionSlug: "trusted-eu", NodeRole: "head", Status: "pending", ExpiresAt: now.Add(time.Hour),
		}}},
	}

	req := httptest.NewRequest(http.MethodGet, "/manage/regions", nil)
	rec := httptest.NewRecorder()
	New(pg, nil, 0).Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var response struct {
		Regions []regionResponse `json:"regions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Regions) != 1 || len(response.Regions[0].Members) != 1 || len(response.Regions[0].Invitations) != 1 {
		t.Fatalf("response=%+v", response)
	}
	if response.Regions[0].Members[0].InstanceID != 77 || response.Regions[0].Members[0].TrustedServiceCount != 1 || response.Regions[0].Invitations[0].InstanceID != 99 {
		t.Fatalf("response=%+v", response)
	}
}

func TestPatchMemberPreservesOmittedRoleStatusAndExpiry(t *testing.T) {
	expiresAt := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	pg := &regionStoreStub{
		members: map[int64][]store.RegionMembership{8: {{
			InstanceID: 77, RegionID: 8, NodeRole: "head", Status: "active", ExpiresAt: &expiresAt,
		}}},
		updated: store.RegionMembership{InstanceID: 77, RegionID: 8},
	}
	req := httptest.NewRequest(http.MethodPatch, "/manage/regions/8/members/77", strings.NewReader(`{"node_role":"combined"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	New(pg, nil, 0).Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if pg.updatedRole != "combined" || pg.updatedStatus != "active" || pg.updatedExpiry == nil || !pg.updatedExpiry.Equal(expiresAt) {
		t.Fatalf("updated role=%q status=%q expiry=%v", pg.updatedRole, pg.updatedStatus, pg.updatedExpiry)
	}
}

func TestPatchMemberReactivationRequiresFreshMatchingOwnership(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	newStore := func() *regionStoreStub {
		return &regionStoreStub{
			members: map[int64][]store.RegionMembership{8: {{
				InstanceID: 77, RegionID: 8, NodeRole: "worker", Status: "suspended",
			}}},
			instance: store.InstanceInfo{ID: 77, PeerID: "peer-worker", OwnerWallet: "wallet-owner"},
			updated:  store.RegionMembership{InstanceID: 77, RegionID: 8},
		}
	}

	t.Run("mismatch denied", func(t *testing.T) {
		pg := newStore()
		svc := New(pg, regionMeshStub{observation: mesh.PeerObservation{PeerID: "peer-worker", Wallet: "wallet-attacker", ObservedAt: now}}, time.Minute)
		svc.now = func() time.Time { return now }
		req := httptest.NewRequest(http.MethodPatch, "/manage/regions/8/members/77", strings.NewReader(`{"status":"active"}`))
		rec := httptest.NewRecorder()
		svc.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || pg.updatedStatus != "" {
			t.Fatalf("status=%d updated=%q", rec.Code, pg.updatedStatus)
		}
	})

	t.Run("fresh match allowed", func(t *testing.T) {
		pg := newStore()
		svc := New(pg, regionMeshStub{observation: mesh.PeerObservation{PeerID: "peer-worker", Wallet: "wallet-owner", ObservedAt: now.Add(-30 * time.Second)}}, time.Minute)
		svc.now = func() time.Time { return now }
		req := httptest.NewRequest(http.MethodPatch, "/manage/regions/8/members/77", strings.NewReader(`{"status":"active"}`))
		rec := httptest.NewRecorder()
		svc.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || pg.updatedStatus != "active" || pg.updatedOwnership == nil || !pg.updatedOwnership.Equal(now.Add(-30*time.Second)) {
			t.Fatalf("status=%d updated=%q ownership=%v", rec.Code, pg.updatedStatus, pg.updatedOwnership)
		}
	})
}

// TestAcceptInvitationMapsMigrationConflict verifies that a region migration
// blocked by existing trusted bindings surfaces as a 409 with an actionable
// message, not an opaque 503.
func TestAcceptInvitationMapsMigrationConflict(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pg := &regionStoreStub{
		instance:  store.InstanceInfo{ID: 77, PeerID: "peer-worker", OwnerWallet: "wallet-owner"},
		updated:   store.RegionMembership{InstanceID: 77, RegionID: 8},
		acceptErr: store.ErrRegionMigrationConflict,
	}
	svc := New(pg, regionMeshStub{observation: mesh.PeerObservation{PeerID: "peer-worker", Wallet: "wallet-owner", ObservedAt: now.Add(-30 * time.Second)}}, time.Minute)
	svc.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPatch, "/manage/regions/8/members/77", strings.NewReader(`{"action":"accept","acceptance_token":"tok"}`))
	rec := httptest.NewRecorder()
	svc.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "trusted service bindings") {
		t.Fatalf("body=%q, want actionable migration message", rec.Body.String())
	}
}
