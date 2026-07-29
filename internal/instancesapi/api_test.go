package instancesapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/store"
)

type storeStub struct {
	instanceByPeer     store.InstanceInfo
	instanceByPeerErr  error
	createIn           store.InstanceInfo
	createOut          store.InstanceInfo
	createErr          error
	reclaimOut         store.InstanceInfo
	reclaimErr         error
	instances          []store.InstanceInfo
	instancesErr       error
	identity           store.IdentityInfo
	identityErr        error
	instanceByID       store.InstanceInfo
	instanceByIDErr    error
	updateOut          store.InstanceInfo
	updateErr          error
	updateLabel        string
	updateMode         string
	replaceOut         store.InstanceInfo
	replaceErr         error
	deleteChanged      bool
	deleteErr          error
	wallets            []string
	walletsErr         error
	replacedRules      []store.ACLRule
	serviceInstance    store.InstanceInfo
	serviceInstanceErr error
	serviceACL         store.InstanceService
	serviceACLErr      error
}

func (s *storeStub) GetInstanceByPeerID(context.Context, string) (store.InstanceInfo, error) {
	return s.instanceByPeer, s.instanceByPeerErr
}
func (s *storeStub) CreateInstance(_ context.Context, in store.InstanceInfo) (store.InstanceInfo, error) {
	s.createIn = in
	return s.createOut, s.createErr
}
func (s *storeStub) ReclaimInstance(context.Context, int64, string, string, *string, *time.Time) (store.InstanceInfo, error) {
	return s.reclaimOut, s.reclaimErr
}
func (s *storeStub) ListInstancesByUser(context.Context, string) ([]store.InstanceInfo, error) {
	return s.instances, s.instancesErr
}
func (s *storeStub) GetIdentity(context.Context, string) (store.IdentityInfo, error) {
	return s.identity, s.identityErr
}
func (s *storeStub) GetInstanceByIDForUser(context.Context, string, int64) (store.InstanceInfo, error) {
	return s.instanceByID, s.instanceByIDErr
}
func (s *storeStub) UpdateInstanceMetadata(_ context.Context, _ string, _ int64, label, mode, _ string, _ *string, _ *time.Time) (store.InstanceInfo, error) {
	s.updateLabel = label
	s.updateMode = mode
	return s.updateOut, s.updateErr
}
func (s *storeStub) ReplaceInstanceACL(_ context.Context, _ string, _ int64, _ string, _ string, _ *string, _ *time.Time, rules []store.ACLRule) (store.InstanceInfo, error) {
	s.replacedRules = append([]store.ACLRule(nil), rules...)
	return s.replaceOut, s.replaceErr
}
func (s *storeStub) DeleteInstanceByIDForUser(context.Context, string, int64) (bool, error) {
	return s.deleteChanged, s.deleteErr
}
func (s *storeStub) GetUserWalletSet(context.Context, string) ([]string, error) {
	return s.wallets, s.walletsErr
}
func (s *storeStub) GetInstanceServicesForUser(context.Context, string, int64) (store.InstanceInfo, error) {
	return s.serviceInstance, s.serviceInstanceErr
}
func (s *storeStub) ReplaceInstanceServicePolicy(context.Context, string, int64, store.ReplaceServicePolicyInput) (store.InstanceInfo, error) {
	return s.serviceInstance, s.serviceInstanceErr
}
func (s *storeStub) ReplaceInstanceServiceACL(context.Context, string, int64, int64, string, []store.ACLRule) (store.InstanceService, error) {
	return s.serviceACL, s.serviceACLErr
}

type meshStub struct {
	observation mesh.PeerObservation
	lookupErr   error
	statuses    map[string]bool
	statusErr   error
	lookupCalls int
}

func (m *meshStub) LookupPeer(context.Context, string) (mesh.PeerObservation, error) {
	m.lookupCalls++
	return m.observation, m.lookupErr
}
func (m *meshStub) OnlineStatus(context.Context, []string) (map[string]bool, error) {
	return m.statuses, m.statusErr
}

type verifierStub struct{}

const validTestPeerID = "12D3KooWPHmsoT1AdLbLUzVDYTk3xx3jSPfFy3Y3FdPzYpbPLyrV"

func (verifierStub) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: "user-alice"}, nil
}

func authedRoutes(t *testing.T, svc *Service) http.Handler {
	t.Helper()
	return principal.Middleware(verifierStub{}, nil, func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	})(svc.Routes())
}

func TestHandleCreateRejectsMalformedPeerIDBeforeMeshLookup(t *testing.T) {
	store := &storeStub{}
	mesh := &meshStub{}
	svc := New(store, mesh, time.Hour, time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/manage/instances", bytes.NewBufferString(`{"peer_id":"bad id","label":"demo"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
	if mesh.lookupCalls != 0 {
		t.Fatalf("mesh lookup calls=%d, want 0", mesh.lookupCalls)
	}
}

func TestHandleReplaceACLRejectsUnknownFields(t *testing.T) {
	store := &storeStub{}
	mesh := &meshStub{}
	svc := New(store, mesh, time.Hour, time.Minute)

	req := httptest.NewRequest(http.MethodPut, "/manage/instances/7/acl", bytes.NewBufferString(`{"mode":"restricted","rules":[],"extra":true}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestHandleReplaceACLSerializesRulesWithLowercaseKeys(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC)
	store := &storeStub{
		instanceByID: store.InstanceInfo{
			ID:          7,
			AccountID:   "user-alice",
			PeerID:      "peer-a",
			Label:       "demo",
			OwnerWallet: "owner-wallet",
			AccessMode:  "restricted",
		},
		replaceOut: store.InstanceInfo{
			ID:                  7,
			AccountID:           "user-alice",
			PeerID:              "peer-a",
			Label:               "demo",
			OwnerWallet:         "owner-wallet",
			AccessMode:          "restricted",
			PolicyRevision:      4,
			OwnershipStatus:     "active",
			OwnershipObservedAt: &observedAt,
			Rules:               []store.ACLRule{{Kind: "email_domain", Value: "example.com"}},
		},
	}
	mesh := &meshStub{observation: mesh.PeerObservation{
		PeerID:     "peer-a",
		Wallet:     "owner-wallet",
		ObservedAt: observedAt,
	}}
	svc := New(store, mesh, time.Hour, time.Minute)
	svc.now = func() time.Time { return observedAt.Add(10 * time.Second) }

	req := httptest.NewRequest(http.MethodPut, "/manage/instances/7/acl", bytes.NewBufferString(`{"mode":"restricted","rules":[{"kind":"email_domain","value":"Example.COM"}]}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	rules, ok := body["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules=%T %v, want one rule", body["rules"], body["rules"])
	}
	rule := rules[0].(map[string]any)
	if _, ok := rule["kind"]; !ok {
		t.Fatalf("rule=%v, want lowercase kind field", rule)
	}
	if _, ok := rule["Kind"]; ok {
		t.Fatalf("rule=%v, want no exported Kind field", rule)
	}
	if store.replacedRules[0].Value != "example.com" {
		t.Fatalf("normalized rules=%v, want lowercase example.com", store.replacedRules)
	}
}

func TestHandlePatchReturnsConflictWhenOwnershipMismatches(t *testing.T) {
	store := &storeStub{
		instanceByID: store.InstanceInfo{
			ID:          7,
			AccountID:   "user-alice",
			PeerID:      "peer-a",
			Label:       "demo",
			OwnerWallet: "owner-wallet",
			AccessMode:  "restricted",
		},
		updateOut: store.InstanceInfo{
			ID:              7,
			OwnershipStatus: "mismatch",
		},
	}
	mesh := &meshStub{observation: mesh.PeerObservation{
		PeerID:     "peer-a",
		Wallet:     "different-wallet",
		ObservedAt: time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC),
	}}
	svc := New(store, mesh, time.Hour, time.Minute)

	req := httptest.NewRequest(http.MethodPatch, "/manage/instances/7", bytes.NewBufferString(`{"label":"demo","mode":"restricted"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestHandlePatchAllowsLabelOnlyAndPreservesMode(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC)
	store := &storeStub{
		instanceByID: store.InstanceInfo{
			ID:          7,
			AccountID:   "user-alice",
			PeerID:      "peer-a",
			Label:       "old",
			OwnerWallet: "owner-wallet",
			AccessMode:  "restricted",
		},
		updateOut: store.InstanceInfo{
			ID:              7,
			PeerID:          "peer-a",
			Label:           "new",
			OwnerWallet:     "owner-wallet",
			AccessMode:      "restricted",
			OwnershipStatus: "active",
		},
	}
	mesh := &meshStub{observation: mesh.PeerObservation{
		PeerID:     "peer-a",
		Wallet:     "owner-wallet",
		ObservedAt: observedAt,
	}}
	svc := New(store, mesh, time.Hour, time.Minute)
	svc.now = func() time.Time { return observedAt.Add(10 * time.Second) }

	req := httptest.NewRequest(http.MethodPatch, "/manage/instances/7", bytes.NewBufferString(`{"label":"new"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if store.updateLabel != "new" || store.updateMode != "restricted" {
		t.Fatalf("update label/mode=%q/%q, want new/restricted", store.updateLabel, store.updateMode)
	}
}

func TestHandlePatchAllowsModeOnlyAndPreservesLabel(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC)
	store := &storeStub{
		instanceByID: store.InstanceInfo{
			ID:          7,
			AccountID:   "user-alice",
			PeerID:      "peer-a",
			Label:       "keep-me",
			OwnerWallet: "owner-wallet",
			AccessMode:  "restricted",
		},
		updateOut: store.InstanceInfo{
			ID:              7,
			PeerID:          "peer-a",
			Label:           "keep-me",
			OwnerWallet:     "owner-wallet",
			AccessMode:      "public",
			OwnershipStatus: "active",
		},
	}
	mesh := &meshStub{observation: mesh.PeerObservation{
		PeerID:     "peer-a",
		Wallet:     "owner-wallet",
		ObservedAt: observedAt,
	}}
	svc := New(store, mesh, time.Hour, time.Minute)
	svc.now = func() time.Time { return observedAt.Add(10 * time.Second) }

	req := httptest.NewRequest(http.MethodPatch, "/manage/instances/7", bytes.NewBufferString(`{"mode":"public"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if store.updateLabel != "keep-me" || store.updateMode != "public" {
		t.Fatalf("update label/mode=%q/%q, want keep-me/public", store.updateLabel, store.updateMode)
	}
}

func TestHandleCreateReclaimsWhenAttestedWalletChangesToCallerWallet(t *testing.T) {
	observedAt := time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC)
	store := &storeStub{
		wallets: []string{"wallet-b"},
		instanceByPeer: store.InstanceInfo{
			ID:          9,
			AccountID:   "user-old-owner",
			PeerID:      "peer-a",
			OwnerWallet: "wallet-a",
			AccessMode:  "restricted",
		},
		reclaimOut: store.InstanceInfo{
			ID:                  9,
			AccountID:           "user-alice",
			PeerID:              "peer-a",
			OwnerWallet:         "wallet-b",
			AccessMode:          "restricted",
			OwnershipStatus:     "active",
			OwnershipObservedAt: &observedAt,
		},
	}
	mesh := &meshStub{observation: mesh.PeerObservation{
		PeerID:     "peer-a",
		Wallet:     "wallet-b",
		ObservedAt: observedAt,
	}}
	svc := New(store, mesh, time.Hour, time.Minute)
	svc.now = func() time.Time { return observedAt.Add(10 * time.Second) }

	req := httptest.NewRequest(http.MethodPost, "/manage/instances", bytes.NewBufferString(`{"peer_id":"`+validTestPeerID+`","label":"demo"}`))
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	authedRoutes(t, svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
}

func TestValidPeerIDRejectsControlAndNonASCIICharacters(t *testing.T) {
	if validPeerID("peer id") {
		t.Fatal("validPeerID accepted whitespace")
	}
	if validPeerID("peer-\u2603") {
		t.Fatal("validPeerID accepted non-ASCII")
	}
	if validPeerID("peer-123") {
		t.Fatal("validPeerID accepted ASCII text that is not a multihash")
	}
	if !validPeerID(validTestPeerID) {
		t.Fatal("validPeerID rejected canonical Ed25519 peer ID")
	}
	if !validPeerID("QmSxh8s3UqmSXBa9SLLREKGAQ6DYCmaeCHeBDdzpJDgn45") {
		t.Fatal("validPeerID rejected canonical legacy SHA-256 peer ID")
	}
}
