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
