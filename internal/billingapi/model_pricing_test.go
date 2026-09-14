package billingapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

// Additional stubStore knobs for the console-managed pricing surface.
type modelCapsStub struct {
	capsSheet    []billing.ModelCaps
	capsSheetErr error
	savedSheet   []billing.ModelCaps
	capsIn       string // captured accountID

	askCfg     map[string][]billing.Ask
	cfgErr     error
	savedPeer  string
	savedAsks  []billing.Ask
	instByPeer map[string]store.InstanceInfo
	instErr    error
	published  map[string][]billing.Ask // ReplaceAsks captures
	publishTTL time.Duration
	publishErr error
}

func (s *stubStore) ModelCapsForAccount(_ context.Context, accountID string) ([]billing.ModelCaps, error) {
	if s.capsSheetErr != nil {
		return nil, s.capsSheetErr
	}
	s.capsIn = accountID
	if s.savedSheet != nil {
		return s.savedSheet, nil
	}
	return s.capsSheet, nil
}

func (s *stubStore) ReplaceModelCaps(_ context.Context, _ string, rows []billing.ModelCaps) error {
	s.savedSheet = rows
	return nil
}

func (s *stubStore) AskConfigForPeers(_ context.Context, peerIDs []string) (map[string][]billing.Ask, error) {
	if s.cfgErr != nil {
		return nil, s.cfgErr
	}
	out := make(map[string][]billing.Ask, len(peerIDs))
	for _, id := range peerIDs {
		if asks, ok := s.askCfg[id]; ok {
			out[id] = asks
		}
	}
	return out, nil
}

func (s *stubStore) ReplaceAskConfig(_ context.Context, peerID string, asks []billing.Ask) error {
	s.savedPeer = peerID
	s.savedAsks = asks
	if s.askCfg == nil {
		s.askCfg = map[string][]billing.Ask{}
	}
	if len(asks) == 0 {
		delete(s.askCfg, peerID)
	} else {
		s.askCfg[peerID] = asks
	}
	return nil
}

func (s *stubStore) GetInstanceByPeerID(_ context.Context, peerID string) (store.InstanceInfo, error) {
	if s.instErr != nil {
		return store.InstanceInfo{}, s.instErr
	}
	if inst, ok := s.instByPeer[peerID]; ok {
		return inst, nil
	}
	return store.InstanceInfo{}, store.ErrNotFound
}

func (s *stubStore) ReplaceAsks(_ context.Context, peerID string, asks []billing.Ask, ttl time.Duration) (int64, error) {
	if s.publishErr != nil {
		return 0, s.publishErr
	}
	if s.published == nil {
		s.published = map[string][]billing.Ask{}
	}
	s.published[peerID] = asks
	s.publishTTL = ttl
	return int64(len(asks)), nil
}

func newPricingTestService(st *stubStore) *Service {
	return New(st, config.BillingEnforce, "ATA", "Wallet", "Mint", "Prog", 9, true, &stubMesh{peers: map[string]mesh.PeerObservation{}})
}

func TestModelCapsSheetRoundTrip(t *testing.T) {
	st := &stubStore{
		capsSheet: []billing.ModelCaps{
			{Service: "llm", Model: "m1", OutputPerMillion: p64(999)},
		},
	}
	svc := newPricingTestService(st)

	// GET returns the sheet.
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodGet, "/manage/billing/preferences/models", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sheet modelCapsSheetRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &sheet); err != nil {
		t.Fatal(err)
	}
	if len(sheet.Models) != 1 || sheet.Models[0].Model != "m1" || sheet.Models[0].OutputPerMillion.v == nil || *sheet.Models[0].OutputPerMillion.v != 999 {
		t.Fatalf("sheet = %+v", sheet.Models)
	}

	// PUT replaces it; nulls stay null (inherit flat caps).
	body := `{"models":[{"service":"llm","model":"m2","input_per_million":500,"cached_input_per_million":null,"output_per_million":0}]}`
	rec = httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/preferences/models", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("put code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(st.savedSheet) != 1 || st.savedSheet[0].Model != "m2" {
		t.Fatalf("saved = %+v", st.savedSheet)
	}
	r := st.savedSheet[0]
	if r.InputPerMillion == nil || *r.InputPerMillion != 500 || r.CachedInputPerMillion != nil || r.OutputPerMillion == nil || *r.OutputPerMillion != 0 {
		t.Fatalf("tiers wrong: %+v", r)
	}
	if st.capsIn != "acct-1" {
		t.Fatalf("caps scoped to %q", st.capsIn)
	}

	// Duplicates and empty models are rejected.
	for _, bad := range []string{
		`{"models":[{"service":"llm","model":"m2"},{"service":"llm","model":"m2"}]}`,
		`{"models":[{"service":"","model":"m2"}]}`,
		`{"models":[{"service":"llm"}]}`,
	} {
		rec = httptest.NewRecorder()
		authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/preferences/models", bad))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad sheet %q: code=%d want 400", bad, rec.Code)
		}
	}
}

func TestAskConfigPutPublishesImmediately(t *testing.T) {
	st := &stubStore{
		instByPeer: map[string]store.InstanceInfo{
			"peer-live": {AccountID: "acct-1", PeerID: "peer-live", OwnerWallet: "WalletA"},
		},
	}
	m := &stubMesh{peers: map[string]mesh.PeerObservation{
		"peer-live": {
			PeerID: "peer-live", Wallet: "WalletA", Online: true,
			ObservedAt: time.Now().UTC(),
			Services:   []mesh.ServiceObservation{{Name: "llm", IdentityGroups: []string{"model=m1"}}},
		},
	}}
	svc := New(st, config.BillingEnforce, "ATA", "Wallet", "Mint", "Prog", 9, true, m)

	body := `{"asks":[{"service":"llm","model":"m1","input_per_million":100,"cached_input_per_million":30,"output_per_million":300}]}`
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/asks-config/peer-live", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got askConfigPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Published {
		t.Fatalf("expected published, got %+v (err=%v)", got, got.PublishError)
	}
	if len(st.savedAsks) != 1 || st.savedAsks[0].Model != "m1" {
		t.Fatalf("config saved = %+v", st.savedAsks)
	}
	pub := st.published["peer-live"]
	if len(pub) != 1 || pub[0].InputPerMillion != 100 {
		t.Fatalf("market publish = %+v", pub)
	}
	if st.publishTTL != 5*time.Minute {
		t.Fatalf("publish TTL = %v, want 5m", st.publishTTL)
	}
}

func TestAskConfigPutStoresWhenPeerOffline(t *testing.T) {
	// The instance is owned and billable but the mesh lookup fails: config
	// must be stored (the refresher publishes when the peer comes live),
	// and the response says published=false with the reason.
	st := &stubStore{
		instByPeer: map[string]store.InstanceInfo{
			"peer-dark": {AccountID: "acct-1", PeerID: "peer-dark", OwnerWallet: "WalletA"},
		},
	}
	svc := newPricingTestService(st) // empty mesh: lookup fails

	body := `{"asks":[{"service":"llm","model":"m1","input_per_million":1,"cached_input_per_million":0,"output_per_million":1}]}`
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/asks-config/peer-dark", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got askConfigPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Published || got.PublishError == nil {
		t.Fatalf("expected published=false with reason, got %+v", got)
	}
	if len(st.savedAsks) != 1 {
		t.Fatalf("config must be stored even when publish is deferred: %+v", st.savedAsks)
	}
}

func TestAskConfigPutRejectsForeignPeerAndBadAsks(t *testing.T) {
	st := &stubStore{
		instByPeer: map[string]store.InstanceInfo{
			"peer-foreign": {AccountID: "acct-other", PeerID: "peer-foreign", OwnerWallet: "Other"},
		},
	}
	svc := newPricingTestService(st)

	// A peer owned by another account must 404 (existence is not disclosure).
	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/asks-config/peer-foreign", `{"asks":[]}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign peer put: code=%d want 404", rec.Code)
	}
	// DELETE likewise.
	rec = httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodDelete, "/manage/billing/asks-config/peer-foreign", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign peer delete: code=%d want 404", rec.Code)
	}

	// Negative rates, duplicates and empty routes are rejected before any
	// store write.
	st2 := &stubStore{instByPeer: st.instByPeer}
	st2.instByPeer["peer-live"] = store.InstanceInfo{AccountID: "acct-1", PeerID: "peer-live", OwnerWallet: "WalletA"}
	svc2 := newPricingTestService(st2)
	for _, bad := range []string{
		`{"asks":[{"service":"llm","model":"m1","input_per_million":-1,"cached_input_per_million":0,"output_per_million":0}]}`,
		`{"asks":[{"service":"llm","model":"m1"},{"service":"llm","model":"m1"}]}`,
		`{"asks":[{"service":"","model":"m1"}]}`,
	} {
		rec = httptest.NewRecorder()
		authed(t, svc2, "acct-1").ServeHTTP(rec, areq(http.MethodPut, "/manage/billing/asks-config/peer-live", bad))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad asks %q: code=%d want 400", bad, rec.Code)
		}
	}
	if st2.savedPeer != "" {
		t.Fatal("rejected configs must not be stored")
	}
}

func TestAskConfigDeleteClearsConfig(t *testing.T) {
	st := &stubStore{
		instByPeer: map[string]store.InstanceInfo{
			"peer-live": {AccountID: "acct-1", PeerID: "peer-live", OwnerWallet: "WalletA"},
		},
		askCfg: map[string][]billing.Ask{
			"peer-live": {{PeerID: "peer-live", Service: "llm", Model: "m1", InputPerMillion: 1, CachedInputPerMillion: 1, OutputPerMillion: 1, UpdatedAt: time.Now().UTC()}},
		},
	}
	svc := newPricingTestService(st)

	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodDelete, "/manage/billing/asks-config/peer-live", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got askConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Asks) != 0 {
		t.Fatalf("cleared config must be empty, got %+v", got.Asks)
	}
	if st.savedPeer != "peer-live" || len(st.savedAsks) != 0 {
		t.Fatalf("expected empty ReplaceAskConfig on %q, got %q %+v", "peer-live", st.savedPeer, st.savedAsks)
	}
}

func TestAskConfigListScopesToOwnedPeers(t *testing.T) {
	st := &stubStore{
		instances: []store.InstanceInfo{
			{AccountID: "acct-1", PeerID: "peer-mine"},
			{AccountID: "acct-other", PeerID: "peer-theirs"},
		},
		askCfg: map[string][]billing.Ask{
			"peer-mine":   {{PeerID: "peer-mine", Service: "llm", Model: "m1", InputPerMillion: 5, CachedInputPerMillion: 5, OutputPerMillion: 5}},
			"peer-theirs": {{PeerID: "peer-theirs", Service: "llm", Model: "m9", InputPerMillion: 9, CachedInputPerMillion: 9, OutputPerMillion: 9}},
		},
	}
	svc := newPricingTestService(st)

	rec := httptest.NewRecorder()
	authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodGet, "/manage/billing/asks-config", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got asksConfigListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || got.Peers[0].PeerID != "peer-mine" {
		t.Fatalf("peers = %+v, want only peer-mine", got.Peers)
	}
}

func TestPricingEndpointsOffMode(t *testing.T) {
	st := &stubStore{}
	svc := New(st, config.BillingOff, "ATA", "Wallet", "Mint", "Prog", 9, true, &stubMesh{peers: map[string]mesh.PeerObservation{}})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/manage/billing/preferences/models", `{"models":[]}`},
		{http.MethodPut, "/manage/billing/asks-config/peer-x", `{"asks":[]}`},
		{http.MethodDelete, "/manage/billing/asks-config/peer-x", ""},
	} {
		rec := httptest.NewRecorder()
		authed(t, svc, "acct-1").ServeHTTP(rec, areq(tc.method, tc.path, tc.body))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s %s in off mode: code=%d want 409", tc.method, tc.path, rec.Code)
		}
	}
	// Off-mode GETs still serve well-formed empty payloads.
	for _, path := range []string{"/manage/billing/preferences/models", "/manage/billing/asks-config"} {
		rec := httptest.NewRecorder()
		authed(t, svc, "acct-1").ServeHTTP(rec, areq(http.MethodGet, path, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s in off mode: code=%d want 200", path, rec.Code)
		}
	}
}

// The model-pricing handlers share the requireAccount guard: no account
// context → 401.
func TestPricingEndpointsRequireAccount(t *testing.T) {
	st := &stubStore{}
	svc := newPricingTestService(st)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/manage/billing/preferences/models"},
		{http.MethodPut, "/manage/billing/preferences/models"},
		{http.MethodGet, "/manage/billing/asks-config"},
		{http.MethodPut, "/manage/billing/asks-config/p"},
		{http.MethodDelete, "/manage/billing/asks-config/p"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		svc.Routes().ServeHTTP(rec, req) // no auth middleware
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated: code=%d want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func p64(v int64) *int64 { return &v }

// areq builds a request carrying the bearer header principal.Middleware reads.
func areq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer token")
	return r
}

var _ = errors.New // keep errors imported if assertions above change
