package pricingapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/store"
)

type stubStore struct {
	replacedAsks []billing.Ask
	replaceErr   error
	inst         store.InstanceInfo
	instErr      error
}

func (s *stubStore) ReplaceAsks(_ context.Context, _ string, asks []billing.Ask, _ time.Duration) (int64, error) {
	if s.replaceErr != nil {
		return 0, s.replaceErr
	}
	s.replacedAsks = asks
	if len(asks) == 0 {
		return 0, nil
	}
	return 1, nil
}
func (s *stubStore) GetInstanceByPeerID(_ context.Context, _ string) (store.InstanceInfo, error) {
	return s.inst, s.instErr
}

type stubMesh struct {
	obs mesh.PeerObservation
	err error
}

func (m *stubMesh) LookupPeer(_ context.Context, _ string) (mesh.PeerObservation, error) {
	return m.obs, m.err
}

type stubVerifier struct {
	claims nodecred.Claims
	err    error
}

func (v *stubVerifier) Verify(_ context.Context, _ string) (nodecred.Claims, error) {
	return v.claims, v.err
}

type stubAllowlist struct {
	svcs []AllowEntry
	err  error
}

func (a *stubAllowlist) Services(_ context.Context) ([]AllowEntry, error) {
	return a.svcs, a.err
}

func activeMembershipPtr() *store.RegionMembership {
	return &store.RegionMembership{Status: "active", RegionStatus: "active"}
}

func newSVC(t *testing.T, st *stubStore, mx *stubMesh, v *stubVerifier, a *stubAllowlist) *Service {
	t.Helper()
	return New(st, mx, v, a, time.Minute)
}

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/pricing", r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPricingReplacesAsks(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "peer-seller", AccountID: "acct", OwnerWallet: "wallet-A", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "wallet-A", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=llama3.1-70b"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "peer-seller", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"llama3.1-70b"}}}}
	svc := newSVC(t, st, mx, v, a)

	body := `{"asks":[{"service":"llm","model":"llama3.1-70b","input_per_million":1200,"cached_input_per_million":300,"output_per_million":3600}]}`
	rec := post(t, svc.Handler(), "tok", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(st.replacedAsks) != 1 || st.replacedAsks[0].Model != "llama3.1-70b" {
		t.Fatalf("replacedAsks=%+v", st.replacedAsks)
	}
}

func TestPricingRejectsWrongAudience(t *testing.T) {
	v := &stubVerifier{claims: nodecred.Claims{Subject: "peer-seller", Audience: nodecred.Audience}} // ACL audience
	svc := newSVC(t, &stubStore{}, &stubMesh{}, v, &stubAllowlist{})
	rec := post(t, svc.Handler(), "tok", `{"asks":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestPricingRejectsUnauthenticated(t *testing.T) {
	v := &stubVerifier{err: errors.New("nope")}
	svc := newSVC(t, &stubStore{}, &stubMesh{}, v, &stubAllowlist{})
	rec := post(t, svc.Handler(), "", `{"asks":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}

func TestPricingRejectsWalletMismatch(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", OwnerWallet: "wallet-A", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{Wallet: "wallet-B", ObservedAt: time.Now()}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	svc := newSVC(t, st, mx, v, &stubAllowlist{})
	rec := post(t, svc.Handler(), "tok", `{"asks":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestPricingRejectsNotBillable(t *testing.T) {
	// An instance with no credit account / owner wallet is neither a billable
	// seller nor a routable one; publication is rejected with 409.
	for _, inst := range []store.InstanceInfo{
		{PeerID: "p"},                    // no account, no wallet
		{PeerID: "p", AccountID: "acct"}, // account but no wallet
		{PeerID: "p", OwnerWallet: "w"},  // wallet but no account
		{PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr()}, // billable; control
	} {
		svc := newSVC(t, &stubStore{inst: inst}, &stubMesh{obs: mesh.PeerObservation{Wallet: "w", ObservedAt: time.Now()}},
			&stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}, &stubAllowlist{})
		rec := post(t, svc.Handler(), "tok", `{"asks":[]}`)
		if inst.AccountID != "" && inst.OwnerWallet != "" {
			if rec.Code != http.StatusOK {
				t.Fatalf("billable inst=%+v: code=%d, want 200", inst, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("inst=%+v: code=%d, want 409", inst, rec.Code)
		}
	}
}

func TestPricingDoesNotRequireTrustedRegionMembership(t *testing.T) {
	// A billable, payable provider without any trusted-region membership can
	// publish — the marketplace is for permissionless providers.
	st := &stubStore{inst: store.InstanceInfo{PeerID: "p", AccountID: "acct", OwnerWallet: "w"}}
	svc := newSVC(t, st, &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
	}},
		&stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}},
		&stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"m1"}}}})
	rec := post(t, svc.Handler(), "tok", `{"asks":[{"service":"llm","model":"m1","input_per_million":1,"cached_input_per_million":1,"output_per_million":1}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPricingRejectsUnknownServiceModel(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=llama3.1-70b"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"llama3.1-70b"}}}}
	svc := newSVC(t, st, mx, v, a)

	rec := post(t, svc.Handler(), "tok",
		`{"asks":[{"service":"llm","model":"unknown-model","input_per_million":100,"cached_input_per_million":100,"output_per_million":100}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPricingRejectsDuplicateAsk(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"m1"}}}}
	svc := newSVC(t, st, mx, v, a)

	rec := post(t, svc.Handler(), "tok",
		`{"asks":[{"service":"llm","model":"m1","input_per_million":1,"cached_input_per_million":1,"output_per_million":1},{"service":"llm","model":"m1","input_per_million":2,"cached_input_per_million":2,"output_per_million":2}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPricingClearsAsksOnEmptyBody(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{Wallet: "w", ObservedAt: time.Now()}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	svc := newSVC(t, st, mx, v, &stubAllowlist{})
	rec := post(t, svc.Handler(), "tok", `{"asks":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(st.replacedAsks) != 0 {
		t.Fatalf("expected 0 asks, got %d", len(st.replacedAsks))
	}
}

func TestPricingEchoesPersistedRevision(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	st.replaceErr = nil // stub returns revision 1 (see ReplaceAsks)
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"m1"}}}}
	svc := newSVC(t, st, mx, v, a)
	rec := post(t, svc.Handler(), "tok", `{"asks":[{"service":"llm","model":"m1","input_per_million":1,"cached_input_per_million":1,"output_per_million":1}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp askResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Revision != 1 {
		t.Fatalf("revision=%d, want 1 (persisted, not 0)", resp.Revision)
	}
}

func TestPricingRejectsNegativeRate(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"m1"}}}}
	svc := newSVC(t, st, mx, v, a)
	rec := post(t, svc.Handler(), "tok",
		`{"asks":[{"service":"llm","model":"m1","input_per_million":-1,"cached_input_per_million":1,"output_per_million":1}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestPricingRejectsStaleObservation(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{Wallet: "w", ObservedAt: time.Now().Add(-10 * time.Minute)}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	svc := newSVC(t, st, mx, v, &stubAllowlist{}) // ownershipMaxAge = 1 min
	rec := post(t, svc.Handler(), "tok", `{"asks":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (stale obs)", rec.Code)
	}
}

func TestPricingStoreConflictReturns409(t *testing.T) {
	st := &stubStore{
		inst:       store.InstanceInfo{PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr()},
		replaceErr: billing.ErrConflict,
	}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"m1"}}}}
	svc := newSVC(t, st, mx, v, a)
	rec := post(t, svc.Handler(), "tok", `{"asks":[{"service":"llm","model":"m1","input_per_million":1,"cached_input_per_million":1,"output_per_million":1}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestPricingRejectsTooManyAsks(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "p", AccountID: "acct", OwnerWallet: "w", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "w", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=maa"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "p", Audience: nodecred.PricingAudience}}
	// allowlist with 300 models so the per-entry check passes
	models := make([]string, 300)
	for i := range models {
		models[i] = "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: models}}}
	svc := newSVC(t, st, mx, v, a)
	// Build 300 asks
	var asks []billing.Ask
	for i := 0; i < 300; i++ {
		asks = append(asks, billing.Ask{Service: "llm", Model: models[i], InputPerMillion: 1})
	}
	body, _ := json.Marshal(askRequest{Asks: asks})
	rec := post(t, svc.Handler(), "tok", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 (>256)", rec.Code)
	}
}

func TestPricingAcceptsSellerAdvertisedModelWithoutGlobalAllowlist(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "peer-seller", AccountID: "acct", OwnerWallet: "wallet-A", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "wallet-A", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=llama3.1-70b"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "peer-seller", Audience: nodecred.PricingAudience}}
	svc := newSVC(t, st, mx, v, &stubAllowlist{})

	body := `{"asks":[{"service":"llm","model":"llama3.1-70b","input_per_million":1200,"cached_input_per_million":300,"output_per_million":3600}]}`
	rec := post(t, svc.Handler(), "tok", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPricingRejectsModelNotAdvertisedBySeller(t *testing.T) {
	st := &stubStore{inst: store.InstanceInfo{
		PeerID: "peer-seller", AccountID: "acct", OwnerWallet: "wallet-A", Membership: activeMembershipPtr(),
	}}
	mx := &stubMesh{obs: mesh.PeerObservation{
		Wallet: "wallet-A", ObservedAt: time.Now(),
		Services: []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=llama3.1-70b"}}},
	}}
	v := &stubVerifier{claims: nodecred.Claims{Subject: "peer-seller", Audience: nodecred.PricingAudience}}
	a := &stubAllowlist{svcs: []AllowEntry{{Name: "llm", Models: []string{"llama3.1-70b", "Qwen/Qwen3-8B"}}}}
	svc := newSVC(t, st, mx, v, a)

	body := `{"asks":[{"service":"llm","model":"Qwen/Qwen3-8B","input_per_million":1200,"cached_input_per_million":300,"output_per_million":3600}]}`
	rec := post(t, svc.Handler(), "tok", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
