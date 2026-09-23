package aclapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/store"
)

type storeStub struct {
	key               store.ActiveKey
	keyErr            error
	managed           []store.InstanceInfo
	managedErr        error
	identity          store.IdentityInfo
	identityErr       error
	walletSet         []string
	walletSetErr      error
	wallets           []store.WalletInfo
	walletsErr        error
	lookupKeyHash     string
	listedPeerIDs     []string
	identityAccountID string
	identityCalls     int
	walletSetCalls    int
	walletListCalls   int
}

func (s *storeStub) LookupActiveKey(_ context.Context, keyHash string) (store.ActiveKey, error) {
	s.lookupKeyHash = keyHash
	return s.key, s.keyErr
}
func (s *storeStub) ListManagedInstancesByPeerIDs(_ context.Context, peerIDs []string) ([]store.InstanceInfo, error) {
	s.listedPeerIDs = append([]string(nil), peerIDs...)
	return s.managed, s.managedErr
}
func (s *storeStub) GetIdentity(_ context.Context, accountID string) (store.IdentityInfo, error) {
	s.identityCalls++
	s.identityAccountID = accountID
	return s.identity, s.identityErr
}
func (s *storeStub) GetUserWalletSet(context.Context, string) ([]string, error) {
	s.walletSetCalls++
	return s.walletSet, s.walletSetErr
}
func (s *storeStub) ListWalletsByUser(context.Context, string) ([]store.WalletInfo, error) {
	s.walletListCalls++
	return s.wallets, s.walletsErr
}

type meshStub struct {
	observations map[string]mesh.PeerObservation
	err          error
	calls        int
	requested    []string
}

type nodeVerifierStub struct {
	claims nodecred.Claims
	err    error
}

func (n nodeVerifierStub) Verify(context.Context, string) (nodecred.Claims, error) {
	return n.claims, n.err
}

func (m *meshStub) LookupPeers(_ context.Context, peerIDs []string) (map[string]mesh.PeerObservation, error) {
	m.calls++
	m.requested = append([]string(nil), peerIDs...)
	if m.err != nil {
		return nil, m.err
	}
	return m.observations, nil
}

func newServiceForTest(store *storeStub, mesh *meshStub) *Service {
	svc := New(store, mesh, "internal-secret-token", time.Hour, time.Minute, 30*time.Second)
	svc.now = func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
	return svc
}

func TestHandlerRejectsUnknownFields(t *testing.T) {
	svc := newServiceForTest(&storeStub{}, &meshStub{})

	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate", strings.NewReader(`{"key_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","peer_ids":["peer-a"],"extra":true}`))
	req.Header.Set("Authorization", "Bearer internal-secret-token")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestHandlerRejectsUppercaseKeyHash(t *testing.T) {
	svc := newServiceForTest(&storeStub{}, &meshStub{})

	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate", strings.NewReader(`{"key_hash":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","peer_ids":["peer-a"]}`))
	req.Header.Set("Authorization", "Bearer internal-secret-token")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestHandlerRejectsWrongLengthInternalToken(t *testing.T) {
	svc := newServiceForTest(&storeStub{}, &meshStub{})

	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate", strings.NewReader(`{"key_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","peer_ids":["peer-a"]}`))
	req.Header.Set("Authorization", "Bearer short")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
	if got := rec.Header().Get(controlAuthFailureHeader); got != "true" {
		t.Fatalf("%s=%q, want true", controlAuthFailureHeader, got)
	}
}

func TestEvaluateAllowsWalletMatchWhenEmailSnapshotIsStale(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:      store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:  []store.InstanceInfo{{PeerID: "peer-a", AccountID: "user-owner", OwnerWallet: "owner-wallet", AccessMode: "restricted", Rules: []store.ACLRule{{Kind: "email_domain", Value: "example.com"}, {Kind: "wallet", Value: "wallet-a"}}}},
		identity: store.IdentityInfo{AccountID: accountID, EmailDomain: "example.com", EmailVerified: true, LastVerifiedAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)},
		walletSet: []string{
			"wallet-a",
		},
		wallets: []store.WalletInfo{{Wallet: "wallet-a", Primary: true}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v, want 200 nil", status, err)
	}
	if len(resp.AllowedPeerIDs) != 1 || resp.AllowedPeerIDs[0] != "peer-a" {
		t.Fatalf("allowed=%v, want peer-a", resp.AllowedPeerIDs)
	}
	if len(resp.Denied) != 0 {
		t.Fatalf("denied=%v, want none", resp.Denied)
	}
	if resp.PrimaryWallet != "wallet-a" {
		t.Fatalf("primary_wallet=%q, want wallet-a", resp.PrimaryWallet)
	}
}

func TestEvaluateAllowsApiKeyPrefixMatchWithoutEnrichment(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:     store.ActiveKey{KeyID: 7, UserID: &accountID, KeyPrefix: "sk-deadbeef"},
		managed: []store.InstanceInfo{{PeerID: "peer-a", AccountID: "user-owner", OwnerWallet: "owner-wallet", AccessMode: "restricted", Rules: []store.ACLRule{{Kind: "api_key", Value: "sk-deadbeef"}}}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v, want 200 nil", status, err)
	}
	if len(resp.AllowedPeerIDs) != 1 || resp.AllowedPeerIDs[0] != "peer-a" {
		t.Fatalf("allowed=%v, want peer-a", resp.AllowedPeerIDs)
	}
	if store.identityCalls != 0 || store.walletSetCalls != 0 {
		t.Fatalf("enrichment ran: identity=%d walletSet=%d, want none", store.identityCalls, store.walletSetCalls)
	}
}

func TestEvaluateDeniesApiKeyPrefixMismatch(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:     store.ActiveKey{KeyID: 7, UserID: &accountID, KeyPrefix: "sk-aaaaaaaa"},
		managed: []store.InstanceInfo{{PeerID: "peer-a", AccountID: "user-owner", OwnerWallet: "owner-wallet", AccessMode: "restricted", Rules: []store.ACLRule{{Kind: "api_key", Value: "sk-deadbeef"}}}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v, want 200 nil", status, err)
	}
	if len(resp.AllowedPeerIDs) != 0 {
		t.Fatalf("allowed=%v, want none", resp.AllowedPeerIDs)
	}
	if len(resp.Denied) != 1 || resp.Denied[0].PeerID != "peer-a" || resp.Denied[0].Reason != "no_match" {
		t.Fatalf("denied=%+v, want peer-a with no_match", resp.Denied)
	}
}

func TestEvaluateDeniesOwnershipMismatch(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:       store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:   []store.InstanceInfo{{PeerID: "peer-a", AccountID: "user-owner", OwnerWallet: "owner-wallet", AccessMode: "restricted"}},
		wallets:   []store.WalletInfo{{Wallet: "wallet-a", Primary: true}},
		walletSet: []string{},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "different-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v, want 200 nil", status, err)
	}
	if len(resp.Denied) != 1 || resp.Denied[0].Reason != "ownership_mismatch" {
		t.Fatalf("denied=%v, want ownership_mismatch", resp.Denied)
	}
}

func TestEvaluatePublicDoesNotDependOnIdentityLookup(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:         store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:     []store.InstanceInfo{{PeerID: "peer-a", OwnerWallet: "owner-wallet", AccessMode: "public"}},
		identityErr: errors.New("db down"),
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK || len(resp.AllowedPeerIDs) != 1 {
		t.Fatalf("status=%d err=%v allowed=%v, want public allow", status, err, resp.AllowedPeerIDs)
	}
	if store.identityCalls != 0 {
		t.Fatalf("identity calls=%d, want 0 for public decision", store.identityCalls)
	}
}

func TestEvaluatePublicDoesNotDependOnPrimaryWalletLookup(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:        store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:    []store.InstanceInfo{{PeerID: "peer-a", OwnerWallet: "owner-wallet", AccessMode: "public"}},
		identity:   store.IdentityInfo{AccountID: accountID},
		walletSet:  []string{"wallet-a"},
		walletsErr: errors.New("db down"),
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK || len(resp.AllowedPeerIDs) != 1 {
		t.Fatalf("status=%d err=%v allowed=%v, want public allow", status, err, resp.AllowedPeerIDs)
	}
	if resp.PrimaryWallet != "" {
		t.Fatalf("primary wallet=%q, want empty when optional lookup fails", resp.PrimaryWallet)
	}
}

func TestEvaluateOwnerDoesNotDependOnPrincipalEnrichment(t *testing.T) {
	accountID := "user-owner"
	store := &storeStub{
		key:          store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:      []store.InstanceInfo{{PeerID: "peer-a", AccountID: accountID, OwnerWallet: "owner-wallet", AccessMode: "restricted"}},
		identityErr:  errors.New("identity db down"),
		walletSetErr: errors.New("wallet db down"),
		walletsErr:   errors.New("primary wallet db down"),
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK || len(resp.AllowedPeerIDs) != 1 {
		t.Fatalf("status=%d err=%v allowed=%v, want owner allow", status, err, resp.AllowedPeerIDs)
	}
	if store.identityCalls != 0 || store.walletSetCalls != 0 {
		t.Fatalf("principal enrichment calls identity=%d wallets=%d, want 0", store.identityCalls, store.walletSetCalls)
	}
}

func TestEvaluateRestrictedEmailRuleReturns503WhenIdentityLookupFails(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:         store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:     []store.InstanceInfo{{PeerID: "peer-a", AccountID: "user-owner", OwnerWallet: "owner-wallet", AccessMode: "restricted", Rules: []store.ACLRule{{Kind: "email_domain", Value: "example.com"}}}},
		identityErr: errors.New("db down"),
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	_, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err == nil || status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d err=%v, want 503 service unavailable", status, err)
	}
}

func TestHandlerDedupesPeersAndLooksThemUpInOneBatch(t *testing.T) {
	accountID := "user-alice"
	store := &storeStub{
		key:      store.ActiveKey{KeyID: 7, UserID: &accountID},
		managed:  []store.InstanceInfo{{PeerID: "peer-a", OwnerWallet: "owner-wallet", AccessMode: "public"}, {PeerID: "peer-b", OwnerWallet: "owner-wallet", AccessMode: "public"}},
		identity: store.IdentityInfo{AccountID: accountID},
		wallets:  []store.WalletInfo{{Wallet: "wallet-a", Primary: true}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
		"peer-b": {PeerID: "peer-b", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate", strings.NewReader(`{"key_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","peer_ids":[" peer-b ","peer-a","peer-b"]}`))
	req.Header.Set("Authorization", "Bearer internal-secret-token")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if mesh.calls != 1 {
		t.Fatalf("lookup batch calls=%d, want 1", mesh.calls)
	}
	if got := strings.Join(mesh.requested, ","); got != "peer-a,peer-b" {
		t.Fatalf("requested peers=%q, want peer-a,peer-b", got)
	}
	var resp evaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := strings.Join(resp.AllowedPeerIDs, ","); got != "peer-a,peer-b" {
		t.Fatalf("allowed=%q, want peer-a,peer-b", got)
	}
}

func TestEvaluateV1DeniesManagedServicePolicyPeerWithoutServiceContext(t *testing.T) {
	store := &storeStub{
		key:     store.ActiveKey{KeyID: 9},
		managed: []store.InstanceInfo{{PeerID: "peer-a", PolicyScope: store.PolicyScopeService, OwnerWallet: "owner-wallet"}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-a"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if len(resp.Denied) != 1 || resp.Denied[0].Reason != "service_context_required" {
		t.Fatalf("denied=%v, want service_context_required", resp.Denied)
	}
}

func TestEvaluateV2PermissionlessServiceAllowsDeclaredPublicBinding(t *testing.T) {
	store := &storeStub{
		key: store.ActiveKey{KeyID: 11},
		managed: []store.InstanceInfo{{
			PeerID:      "peer-a",
			OwnerWallet: "owner-wallet",
			PolicyScope: store.PolicyScopeService,
			Services: []store.InstanceService{{
				ServiceName:           "embeddings-public",
				Exposure:              store.ExposurePermissionless,
				AccessMode:            store.AccessModePublic,
				ServicePolicyRevision: 3,
			}},
		}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {
			PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC),
			Services: []mesh.ServiceObservation{{Name: "embeddings-public"}},
		},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash:   strings.Repeat("a", 64),
		Partition: "permissionless",
		RouteKind: "service_ingress",
		Service:   "embeddings-public",
		PeerIDs:   []string{"peer-a"},
	}, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if len(resp.Decisions) != 1 || !resp.Decisions[0].Allowed || resp.Decisions[0].ServiceExposure != "permissionless" {
		t.Fatalf("decisions=%+v, want permissionless allow", resp.Decisions)
	}
}

func TestEvaluateV2PermissionlessRouteRejectsTrustedBinding(t *testing.T) {
	store := &storeStub{
		key: store.ActiveKey{KeyID: 12},
		managed: []store.InstanceInfo{{
			PeerID:      "peer-a",
			OwnerWallet: "owner-wallet",
			PolicyScope: store.PolicyScopeService,
			Membership:  &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 9},
			Services: []store.InstanceService{{
				ServiceName: "llm-private",
				Exposure:    store.ExposureTrustedRegion,
				AccessMode:  store.AccessModeRestricted,
			}},
		}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC), Services: []mesh.ServiceObservation{{Name: "llm-private"}}},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash:   strings.Repeat("a", 64),
		Partition: "permissionless",
		RouteKind: "service_ingress",
		Service:   "llm-private",
		PeerIDs:   []string{"peer-a"},
	}, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := resp.Decisions[0].Reason; got != "trusted_route_required" {
		t.Fatalf("reason=%q, want trusted_route_required", got)
	}
}

func TestEvaluateV2RejectsDuplicateLiveServiceName(t *testing.T) {
	store := &storeStub{
		key: store.ActiveKey{KeyID: 13},
		managed: []store.InstanceInfo{{
			PeerID:      "peer-a",
			OwnerWallet: "owner-wallet",
			PolicyScope: store.PolicyScopeService,
			Services:    []store.InstanceService{{ServiceName: "dup", Exposure: store.ExposurePermissionless, AccessMode: store.AccessModePublic}},
		}},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "owner-wallet", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC), Services: []mesh.ServiceObservation{{Name: "dup"}, {Name: "dup"}}},
	}}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash:   strings.Repeat("a", 64),
		Partition: "permissionless",
		RouteKind: "service_ingress",
		Service:   "dup",
		PeerIDs:   []string{"peer-a"},
	}, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := resp.Decisions[0].Reason; got != "duplicate_service_name" {
		t.Fatalf("reason=%q, want duplicate_service_name", got)
	}
}

func TestEvaluateV2TrustedWorkerRejectsUntrustedUpstream(t *testing.T) {
	claims := nodecred.Claims{Subject: "peer-a", Role: "worker", Region: "research-eu", MembershipRevision: 4}
	store := &storeStub{
		key: store.ActiveKey{KeyID: 14},
		managed: []store.InstanceInfo{
			{
				PeerID:      "peer-a",
				OwnerWallet: "wallet-a",
				PolicyScope: store.PolicyScopeService,
				Membership:  &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4},
				Services:    []store.InstanceService{{ServiceName: "llm-private", Exposure: store.ExposureTrustedRegion, AccessMode: store.AccessModePublic}},
			},
			{
				PeerID:      "peer-head",
				OwnerWallet: "wallet-head",
				PolicyScope: store.PolicyScopePeer,
				Membership:  &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "suspended", NodeRole: "head", MembershipRevision: 8},
			},
		},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a":    {PeerID: "peer-a", Wallet: "wallet-a", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC), Services: []mesh.ServiceObservation{{Name: "llm-private"}}},
		"peer-head": {PeerID: "peer-head", Wallet: "wallet-head", ObservedAt: time.Date(2026, 7, 29, 11, 59, 30, 0, time.UTC)},
	}}
	svc := NewWithNodeVerifier(store, mesh, "internal-secret-token", nodeVerifierStub{claims: claims}, time.Hour, time.Minute, 30*time.Second)
	svc.now = func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }

	resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash:        strings.Repeat("a", 64),
		Partition:      "trusted_region",
		Region:         "research-eu",
		RouteKind:      "worker",
		Service:        "llm-private",
		PeerIDs:        []string{"peer-a"},
		UpstreamPeerID: "peer-head",
	}, &claims)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := resp.Decisions[0].Reason; got != "untrusted_upstream" {
		t.Fatalf("reason=%q, want untrusted_upstream", got)
	}
}

func TestEvaluateV2TrustedServiceFailsClosedForPartitionRoleAndRegion(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Second)
	claims := nodecred.Claims{Subject: "peer-head", Role: "head", Region: "research-eu", MembershipRevision: 8}
	tests := []struct {
		name       string
		exposure   string
		membership store.RegionMembership
		wantReason string
	}{
		{
			name:       "partition mismatch",
			exposure:   store.ExposurePermissionless,
			membership: store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4},
			wantReason: "partition_mismatch",
		},
		{
			name:       "wrong target role",
			exposure:   store.ExposureTrustedRegion,
			membership: store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "head", MembershipRevision: 4},
			wantReason: "wrong_role",
		},
		{
			name:       "wrong target region",
			exposure:   store.ExposureTrustedRegion,
			membership: store.RegionMembership{RegionSlug: "research-us", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4},
			wantReason: "not_region_member",
		},
		{
			name:       "disabled target region",
			exposure:   store.ExposureTrustedRegion,
			membership: store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "disabled", Status: "active", NodeRole: "worker", MembershipRevision: 4},
			wantReason: "membership_revoked",
		},
		{
			name:       "expired active membership",
			exposure:   store.ExposureTrustedRegion,
			membership: store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4, ExpiresAt: &expiredAt},
			wantReason: "membership_expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storeBackend := &storeStub{
				key: store.ActiveKey{KeyID: 15},
				managed: []store.InstanceInfo{
					{
						PeerID: "peer-target", OwnerWallet: "wallet-target", PolicyScope: store.PolicyScopeService,
						Membership: &tt.membership,
						Services:   []store.InstanceService{{ServiceName: "llm", Exposure: tt.exposure, AccessMode: store.AccessModePublic}},
					},
					{
						PeerID: "peer-head", OwnerWallet: "wallet-head", PolicyScope: store.PolicyScopePeer,
						Membership: &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "head", MembershipRevision: 8},
					},
				},
			}
			meshBackend := &meshStub{observations: map[string]mesh.PeerObservation{
				"peer-target": {PeerID: "peer-target", Wallet: "wallet-target", ObservedAt: now.Add(-30 * time.Second), Services: []mesh.ServiceObservation{{Name: "llm"}}},
				"peer-head":   {PeerID: "peer-head", Wallet: "wallet-head", ObservedAt: now.Add(-30 * time.Second)},
			}}
			svc := NewWithNodeVerifier(storeBackend, meshBackend, "internal-secret-token", nodeVerifierStub{claims: claims}, time.Hour, time.Minute, 30*time.Second)
			svc.now = func() time.Time { return now }

			resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
				KeyHash: strings.Repeat("a", 64), Partition: "trusted_region", Region: "research-eu",
				RouteKind: "service_ingress", Service: "llm", PeerIDs: []string{"peer-target"},
			}, &claims)
			if err != nil || status != http.StatusOK {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if len(resp.Decisions) != 1 || resp.Decisions[0].Allowed || resp.Decisions[0].Reason != tt.wantReason {
				t.Fatalf("decisions=%+v, want denied %q", resp.Decisions, tt.wantReason)
			}
		})
	}
}

func TestEvaluateV2TrustedServiceDeniesUnmanagedSameNameCandidate(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	claims := nodecred.Claims{Subject: "peer-head", Role: "head", Region: "research-eu", MembershipRevision: 8}
	storeBackend := &storeStub{
		key: store.ActiveKey{KeyID: 16},
		managed: []store.InstanceInfo{
			{
				PeerID: "peer-managed", OwnerWallet: "wallet-managed", PolicyScope: store.PolicyScopeService,
				Membership: &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4},
				Services:   []store.InstanceService{{ServiceName: "llm", Exposure: store.ExposureTrustedRegion, AccessMode: store.AccessModePublic}},
			},
			{
				PeerID: "peer-head", OwnerWallet: "wallet-head", PolicyScope: store.PolicyScopePeer,
				Membership: &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "head", MembershipRevision: 8},
			},
		},
	}
	meshBackend := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-managed": {PeerID: "peer-managed", Wallet: "wallet-managed", ObservedAt: now.Add(-30 * time.Second), Services: []mesh.ServiceObservation{{Name: "llm"}}},
		"peer-head":    {PeerID: "peer-head", Wallet: "wallet-head", ObservedAt: now.Add(-30 * time.Second)},
	}}
	svc := NewWithNodeVerifier(storeBackend, meshBackend, "internal-secret-token", nodeVerifierStub{claims: claims}, time.Hour, time.Minute, 30*time.Second)
	svc.now = func() time.Time { return now }

	resp, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash: strings.Repeat("a", 64), Partition: "trusted_region", Region: "research-eu",
		RouteKind: "service_ingress", Service: "llm", PeerIDs: []string{"peer-managed", "peer-attacker"},
	}, &claims)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if len(resp.Decisions) != 2 || !resp.Decisions[0].Allowed || resp.Decisions[1].Allowed || resp.Decisions[1].Reason != "unmanaged" {
		t.Fatalf("decisions=%+v", resp.Decisions)
	}
}

func TestEvaluateUnmanagedPeersDoNotDependOnMeshLookup(t *testing.T) {
	store := &storeStub{key: store.ActiveKey{KeyID: 7}}
	mesh := &meshStub{err: errors.New("mesh unavailable")}
	svc := newServiceForTest(store, mesh)

	resp, status, err := svc.evaluate(context.Background(), strings.Repeat("a", 64), []string{"peer-unmanaged"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v, want 200 nil", status, err)
	}
	if len(resp.AllowedPeerIDs) != 1 || resp.AllowedPeerIDs[0] != "peer-unmanaged" {
		t.Fatalf("allowed=%v, want unmanaged peer", resp.AllowedPeerIDs)
	}
	if mesh.calls != 0 {
		t.Fatalf("mesh calls=%d, want 0 for unmanaged peers", mesh.calls)
	}
}

// TestEvaluateV2TrustedRejectsRoleMismatch verifies the DB-authoritative
// requester-role check (Fix #2): a node credential that asserts a role
// different from the membership's NodeRole is forged, regardless of any
// other matching field. The membership_revision check already rules out a
// legitimate re-role (that bumps the revision), so a mismatch here is
// conclusive and must be denied with 403, not trusted.
func TestEvaluateV2TrustedRejectsRoleMismatch(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	// JWT claims head, but the DB says this peer is a worker.
	claims := nodecred.Claims{Subject: "peer-a", Role: "head", Region: "research-eu", MembershipRevision: 4}
	store := &storeStub{
		key: store.ActiveKey{KeyID: 14},
		managed: []store.InstanceInfo{
			{
				PeerID:      "peer-a",
				OwnerWallet: "wallet-a",
				PolicyScope: store.PolicyScopePeer,
				Membership:  &store.RegionMembership{RegionSlug: "research-eu", RegionStatus: "active", Status: "active", NodeRole: "worker", MembershipRevision: 4},
			},
		},
	}
	mesh := &meshStub{observations: map[string]mesh.PeerObservation{
		"peer-a": {PeerID: "peer-a", Wallet: "wallet-a", ObservedAt: now.Add(-30 * time.Second)},
	}}
	svc := NewWithNodeVerifier(store, mesh, "internal-secret-token", nodeVerifierStub{claims: claims}, time.Hour, time.Minute, 30*time.Second)
	svc.now = func() time.Time { return now }

	_, status, err := svc.evaluateV2(context.Background(), evaluateV2Request{
		KeyHash:   strings.Repeat("a", 64),
		Partition: "trusted_region",
		Region:    "research-eu",
		RouteKind: "worker",
		Service:   "llm-private",
		PeerIDs:   []string{"peer-a"},
	}, &claims)
	if err == nil {
		t.Fatalf("expected error for role mismatch, got nil")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (role mismatch)", status)
	}
}

// TestEvaluatorRateLimitThrottlesByCaller verifies the per-caller token
// bucket on the v2 evaluator (Fix #3). A trusted caller driving live
// (uncached) evaluations above its configured burst must receive 429s
// rather than starving the control plane.
func TestEvaluatorRateLimitThrottlesByCaller(t *testing.T) {
	store := &storeStub{key: store.ActiveKey{KeyID: 1}}
	mesh := &meshStub{}
	svc := NewWithNodeVerifier(store, mesh, "internal-secret-token", nil, time.Hour, time.Minute, 30*time.Second)
	svc = svc.WithEvaluatorRateLimit(1, 2) // 1 rps, burst 2

	allowed := 0
	throttled := 0
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		body := `{"key_hash":"` + strings.Repeat("a", 64) + `","partition":"permissionless","route_kind":"service_ingress","service":"svc","peer_ids":["peer-x"]}`
		req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate-v2", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer internal-secret-token")
		svc.HandlerV2().ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			if rec.Header().Get("Retry-After") == "" {
				t.Fatalf("request %d: 429 response missing Retry-After header", i)
			}
			throttled++
		default:
			t.Fatalf("unexpected status=%d for request %d: %s", rec.Code, i, rec.Body.String())
		}
	}
	if allowed == 0 {
		t.Fatalf("all requests throttled; expected at least the burst")
	}
	if throttled == 0 {
		t.Fatalf("no requests throttled over burst+1; rate limit not applied (allowed=%d)", allowed)
	}
}

// TestEvaluatorRateLimitKeyedByCaller ensures permissionless and trusted
// callers get independent buckets (a flooded permissionless caller must not
// starve a trusted peer).
func TestEvaluatorRateLimitKeyedByCaller(t *testing.T) {
	store := &storeStub{key: store.ActiveKey{KeyID: 1}}
	mesh := &meshStub{}
	claims := nodecred.Claims{Subject: "peer-trusted", Role: "worker", Region: "research-eu", MembershipRevision: 4}
	svc := NewWithNodeVerifier(store, mesh, "internal-secret-token", nodeVerifierStub{claims: claims}, time.Hour, time.Minute, 30*time.Second)
	svc = svc.WithEvaluatorRateLimit(1, 1) // 1 rps, burst 1

	// Exhaust the permissionless caller's bucket.
	permBody := `{"key_hash":"` + strings.Repeat("a", 64) + `","partition":"permissionless","route_kind":"service_ingress","service":"svc","peer_ids":["peer-x"]}`
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate-v2", strings.NewReader(permBody))
		req.Header.Set("Authorization", "Bearer internal-secret-token")
		svc.HandlerV2().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("perm request %d status=%d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// A trusted caller should still get its own bucket, so the first
	// request must not be a 429 caused by the permissionless flood.
	// upstream_peer_id is required for route_kind=worker; without it the
	// request is rejected during validation (400) before reaching the
	// limiter, which would make the throttling assertions below vacuous.
	trustedBody := `{"key_hash":"` + strings.Repeat("b", 64) + `","partition":"trusted_region","region":"research-eu","route_kind":"worker","service":"svc","peer_ids":["peer-trusted"],"upstream_peer_id":"peer-head"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate-v2", strings.NewReader(trustedBody))
	req.Header.Set("Authorization", "Bearer trusted-token")
	svc.HandlerV2().ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("trusted caller throttled by permissionless flood; buckets are not independent (status=%d)", rec.Code)
	}

	// The trusted bucket must also be live: an immediate second request
	// exceeds the burst of 1 and is throttled, proving the limiter is
	// genuinely applied per caller and the previous assertion was not
	// vacuously satisfied by a disabled limiter.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/internal/acl/evaluate-v2", strings.NewReader(trustedBody))
	req.Header.Set("Authorization", "Bearer trusted-token")
	svc.HandlerV2().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second trusted request status=%d, want 429 (bucket live and independent)", rec.Code)
	}
}

// TestRateLimiterEvictsIdleBuckets ensures per-caller buckets cannot grow
// without bound: a caller idle longer than bucketIdleTTL is reaped on the
// next sweep, so a stream of one-shot identities (every issued API key hash
// creates a bucket) cannot leak memory for the process lifetime.
func TestRateLimiterEvictsIdleBuckets(t *testing.T) {
	rl := newRateLimiter(1, 1)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return now }

	if ok, _ := rl.allow("stale-caller"); !ok {
		t.Fatalf("first allow unexpectedly denied")
	}
	// Advance past the idle TTL; the next allow triggers a sweep.
	now = now.Add(bucketIdleTTL + bucketSweepInterval + time.Second)
	if ok, _ := rl.allow("fresh-caller"); !ok {
		t.Fatalf("allow for fresh caller unexpectedly denied")
	}
	if _, ok := rl.bkts["stale-caller"]; ok {
		t.Fatalf("idle bucket not evicted after %v", bucketIdleTTL)
	}
	if _, ok := rl.bkts["fresh-caller"]; !ok {
		t.Fatalf("active bucket wrongly evicted")
	}
}
