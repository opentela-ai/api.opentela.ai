package billinggate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/account"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/peers"
	"github.com/opentela-ai/api/internal/store"
)

// --- stubs ---

type stubStore struct {
	credit     billing.AccountCredit
	creditErr  error
	asks       []billing.Ask
	asksErr    error
	reserved   *billing.Reservation
	reserveErr error
	ensured    bool
}

func (s *stubStore) EnsureAccountCredit(_ context.Context, _ string) error {
	s.ensured = true
	return nil
}
func (s *stubStore) AccountCredit(_ context.Context, _ string) (billing.AccountCredit, error) {
	return s.credit, s.creditErr
}
func (s *stubStore) SetAccountCaps(_ context.Context, _ string, _ billing.Caps) (billing.AccountCredit, error) {
	return s.credit, nil
}
func (s *stubStore) LiveAsks(_ context.Context, _, _ string, _ time.Time) ([]billing.Ask, error) {
	return s.asks, s.asksErr
}
func (s *stubStore) ReserveBilling(_ context.Context, req billing.Reservation) (billing.AccountCredit, error) {
	if s.reserveErr != nil {
		return billing.AccountCredit{}, s.reserveErr
	}
	s.reserved = &req
	return billing.AccountCredit{AccountID: req.BuyerAccountID, CreditRaw: 1000, ReservedRaw: int64(len(req.Quotes))}, nil
}

type stubSnap struct {
	snap *peers.Snapshot
	err  error
}

func (s *stubSnap) Snapshot(_ context.Context) (*peers.Snapshot, error) {
	return s.snap, s.err
}

// stubSnap also needs to satisfy *peers.Service — but the gate takes
// *peers.Service, so we inject via a field override instead (see newGate).

// --- helpers ---

func withAccount(r *http.Request, accountID string) *http.Request {
	ctx := account.WithID(r.Context(), accountID)
	return r.WithContext(ctx)
}

func makePeer(service, model string, connected bool) peers.Peer {
	return peers.Peer{
		Connected: connected,
		Service: []peers.PeerService{{
			Name:          service,
			IdentityGroup: []string{"model=" + model, "all"},
		}},
	}
}

func newGate(t *testing.T, mode config.BillingMode, st *stubStore, snap *peers.Snapshot) *Service {
	t.Helper()
	s := New(st, nil, mode, 1024, 0)
	// Inject a snapshot function that returns the prebuilt snapshot.
	s.getSnap = func(context.Context) (*peers.Snapshot, error) {
		return snap, nil
	}
	return s
}

func postGate(t *testing.T, s *Service, body, accountID string) *httptest.ResponseRecorder {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := r.Header.Get(allowedPeersHeader)
		w.Header().Set("X-Allowed-Peers", allowed)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxied"))
	})
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if accountID != "" {
		req = withAccount(req, accountID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- tests ---

func TestGateOffModeIsPassThrough(t *testing.T) {
	s := New(&stubStore{}, nil, config.BillingOff, 1024, 0)
	called := false
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"x","max_tokens":1}`))
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("off mode must pass through: called=%v code=%d", called, rec.Code)
	}
	if got := req.Header.Get(allowedPeersHeader); got != "" {
		t.Fatalf("off mode must not set allowed-peers, got %q", got)
	}
}

func TestGateEnforceReservesAndForwards(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "llama3.1-70b", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "seller-A", OwnerWallet: "wallet-A"}},
		"peer-B": {Peer: makePeer("llm", "llama3.1-70b", true), Instance: &store.InstanceInfo{PeerID: "peer-B", AccountID: "seller-B", OwnerWallet: "wallet-B"}},
	}}
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000},
		asks: []billing.Ask{
			{PeerID: "peer-A", Service: "llm", Model: "llama3.1-70b", InputPerMillion: 1000, CachedInputPerMillion: 200, OutputPerMillion: 3000, Revision: 5},
			{PeerID: "peer-B", Service: "llm", Model: "llama3.1-70b", InputPerMillion: 800, CachedInputPerMillion: 150, OutputPerMillion: 2500, Revision: 3},
		},
	}
	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"llama3.1-70b","max_tokens":64}`, "buyer-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.reserved == nil {
		t.Fatal("expected reservation")
	}
	if st.reserved.BuyerAccountID != "buyer-1" {
		t.Fatalf("buyer=%s", st.reserved.BuyerAccountID)
	}
	// The cheaper peer (B at 800) should be first in allowed-peers.
	allowed := rec.Header().Get("X-Allowed-Peers")
	if !strings.HasPrefix(allowed, "peer-B") {
		t.Fatalf("allowed-peers = %q, want peer-B first", allowed)
	}
	// Both peers are in the allowed set.
	if !strings.Contains(allowed, "peer-A") {
		t.Fatalf("allowed-peers = %q, missing peer-A", allowed)
	}
}

func TestGateEnforceRejectsUnaffordablePeer(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000,
			MaxInputPerMillion: int64Ptr(500), MaxCachedInputPerMillion: int64Ptr(500), MaxOutputPerMillion: int64Ptr(500)},
		asks: []billing.Ask{
			{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 1000, CachedInputPerMillion: 200, OutputPerMillion: 3000, Revision: 1},
		},
	}
	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":64}`, "buyer-1")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d, want 402 price_above_max; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "price_above_max" {
		t.Fatalf("error=%q, want price_above_max", body["error"])
	}
}

func TestGateEnforceRejectsNoAccount(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true)},
	}}
	st := &stubStore{credit: billing.AccountCredit{CreditRaw: 1000}}
	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":64}`, "") // no account

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d, want 402 billing_account_required; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGateEnforceRejectsInsufficientCredit(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{
		credit:     billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1, ReservedRaw: 1},
		asks:       []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 1000, OutputPerMillion: 3000, Revision: 1}},
		reserveErr: billing.ErrInsufficientCredit,
	}
	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":64}`, "buyer-1")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("code=%d, want 402 insufficient_credit", rec.Code)
	}
}

func TestGateEnforceRejectsUnsupportedRoute(t *testing.T) {
	s := newGate(t, config.BillingEnforce, &stubStore{}, &peers.Snapshot{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.Middleware(inner)
	// Generative route with no output ceiling and no operator cap.
	s.output = 0
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400 billing_unsupported_route; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGateObserveForwardsWithoutReserving(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{
		credit:     billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000},
		asks:       []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 1000, OutputPerMillion: 3000, Revision: 1}},
		reserveErr: errors.New("should not be called"),
	}
	s := newGate(t, config.BillingObserve, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":64}`, "buyer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("observe must forward: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st.reserved != nil {
		t.Fatal("observe must not persist a reservation")
	}
	// Observe must NOT stamp the allowed-peers list — it forwards the
	// request unchanged so it cannot influence routing.
	if got := rec.Header().Get("X-Allowed-Peers"); got != "" {
		t.Fatalf("observe must not stamp allowed-peers, got %q", got)
	}
}

func TestGateObserveForwardsEvenWhenUnaffordable(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000,
			MaxInputPerMillion: int64Ptr(100)},
		asks: []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 1000, OutputPerMillion: 3000, Revision: 1}},
	}
	s := newGate(t, config.BillingObserve, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":64}`, "buyer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("observe must not reject unaffordable: code=%d", rec.Code)
	}
	// No allowed-peers since none are affordable.
	if got := rec.Header().Get("X-Allowed-Peers"); got != "" {
		t.Fatalf("observe unaffordable should not set allowed-peers, got %q", got)
	}
}

func TestGatePerRequestCapsCannotWidenAccount(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	// The account cap is 500. A looser request header must not raise it.
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000, MaxInputPerMillion: int64Ptr(500)},
		asks:   []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 1000, OutputPerMillion: 100, Revision: 1}},
	}
	s := newGate(t, config.BillingEnforce, st, snap)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":64}`))
	req.Header.Set("X-Max-Input-Price-Per-Million", "2000")
	req = withAccount(req, "buyer-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("looser request cap must not widen account cap: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGateAllowedPeersOverwritesClientHeader(t *testing.T) {
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1_000_000},
		asks: []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 100, OutputPerMillion: 100, Revision: 1}}}
	s := newGate(t, config.BillingEnforce, st, snap)

	var captured string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get(allowedPeersHeader)
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":64}`))
	req.Header.Set(allowedPeersHeader, "evil-peer,client-injected")
	req = withAccount(req, "buyer-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if captured != "peer-A" {
		t.Fatalf("allowed-peers = %q, want peer-A (client value must be overwritten)", captured)
	}
}

func TestGateCappedAt128Peers(t *testing.T) {
	entries := make(map[string]peers.Entry, 200)
	asks := make([]billing.Ask, 0, 200)
	for i := 0; i < 200; i++ {
		pid := "peer-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		entries[pid] = peers.Entry{Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: pid, AccountID: "s", OwnerWallet: "w"}}
		asks = append(asks, billing.Ask{PeerID: pid, Service: "llm", Model: "m1", InputPerMillion: int64(100 + i), OutputPerMillion: 100, Revision: 1})
	}
	st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1 << 62}}
	s := newGate(t, config.BillingEnforce, st, &peers.Snapshot{Entries: entries})
	st.asks = asks

	var captured string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get(allowedPeersHeader)
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":1}`))
	req = withAccount(req, "buyer-1")
	httptest.NewRecorder()
	h.ServeHTTP(httptest.NewRecorder(), req)
	count := strings.Count(captured, ",") + 1
	if count > maxAllowedPeers {
		t.Fatalf("allowed-peers has %d entries, max %d", count, maxAllowedPeers)
	}
	if count != maxAllowedPeers {
		t.Fatalf("allowed-peers has %d entries, want %d (200 peers, capped)", count, maxAllowedPeers)
	}
}

func TestGateUnpricedPeerIsAffordable(t *testing.T) {
	// A peer that serves but never publishes is eligible at a zero quote —
	// free to the buyer, within any cap.
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-free": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-free", AccountID: "s", OwnerWallet: "w"}},
	}}
	st := &stubStore{
		credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 100, MaxInputPerMillion: int64Ptr(50)},
		asks:   nil, // no published asks
	}
	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":1}`, "buyer-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("unpriced peer should be free/affordable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("X-Allowed-Peers"), "peer-free") {
		t.Fatal("unpriced peer should be in allowed-peers")
	}
}

func TestGateNonInferenceRoutePassesThrough(t *testing.T) {
	s := newGate(t, config.BillingEnforce, &stubStore{}, &peers.Snapshot{})
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/v1/service/llm/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("non-inference route must pass through: called=%v code=%d", called, rec.Code)
	}
}

func TestGateRejectsNegativePriceCap(t *testing.T) {
	// A negative cap is a malformed request in every mode: the gate never
	// silently drops it (treating it as unlimited would let a peer the
	// buyer explicitly rejected through).
	for _, mode := range []config.BillingMode{config.BillingEnforce, config.BillingObserve} {
		snap := &peers.Snapshot{Entries: map[string]peers.Entry{
			"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
		}}
		st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1 << 62},
			asks: []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 100, OutputPerMillion: 100, Revision: 1}}}
		s := newGate(t, mode, st, snap)
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		h := s.Middleware(inner)
		req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":1}`))
		req.Header.Set("X-Max-Input-Price-Per-Million", "-5")
		req = withAccount(req, "buyer-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("mode=%v: negative cap code=%d, want 400", mode, rec.Code)
		}
		if st.reserved != nil {
			t.Fatalf("mode=%v: reserved on malformed cap", mode)
		}
	}
}

func TestGateZeroPriceCapAllowsFreePeersOnly(t *testing.T) {
	// X-Max-Input-Price-Per-Million: 0 means "free peers only". A zero-rate
	// peer is affordable; a positive-rate peer is not.
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-free": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-free", AccountID: "s", OwnerWallet: "w"}},
		"peer-paid": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-paid", AccountID: "s", OwnerWallet: "w"}},
	}}
	st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1 << 62},
		asks: []billing.Ask{
			{PeerID: "peer-free", Service: "llm", Model: "m1", InputPerMillion: 0, OutputPerMillion: 0, Revision: 1},
			{PeerID: "peer-paid", Service: "llm", Model: "m1", InputPerMillion: 100, OutputPerMillion: 100, Revision: 1},
		}}
	s := newGate(t, config.BillingEnforce, st, snap)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":1}`))
	req.Header.Set("X-Max-Input-Price-Per-Million", "0")
	req = withAccount(req, "buyer-1")
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Allowed-Peers", r.Header.Get(allowedPeersHeader))
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("zero cap should still allow the free peer: code=%d body=%s", rec.Code, rec.Body.String())
	}
	allowed := rec.Header().Get("X-Allowed-Peers")
	if !strings.Contains(allowed, "peer-free") || strings.Contains(allowed, "peer-paid") {
		t.Fatalf("zero cap = free peers only; got %q", allowed)
	}
}

func TestGateNoProviderReturnsServiceUnavailable(t *testing.T) {
	// No billable provider serves the requested model. Enforce rejects with
	// a provider-availability response (503), observe forwards unchanged.
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-nobill": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-nobill"}}, // serves but not billable
	}}
	st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1 << 62},
		asks: []billing.Ask{{PeerID: "peer-nobill", Service: "llm", Model: "m1", InputPerMillion: 100, OutputPerMillion: 100, Revision: 1}}}

	s := newGate(t, config.BillingEnforce, st, snap)
	rec := postGate(t, s, `{"model":"m1","max_tokens":1}`, "buyer-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no billable provider: code=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "billing_duplicate") {
		t.Fatalf("must not be a duplicate-request error: %s", rec.Body.String())
	}

	// Observe forwards unchanged.
	sObs := newGate(t, config.BillingObserve, st, snap)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := sObs.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":1}`))
	req = withAccount(req, "buyer-1")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("observe must forward when no provider: code=%d", rec2.Code)
	}
}

func TestGateStampsRequestIDInContext(t *testing.T) {
	// The reservation id is stamped into the forwarded request context so the
	// Step 4 response hook can settle or release exactly once.
	snap := &peers.Snapshot{Entries: map[string]peers.Entry{
		"peer-A": {Peer: makePeer("llm", "m1", true), Instance: &store.InstanceInfo{PeerID: "peer-A", AccountID: "s-A", OwnerWallet: "w-A"}},
	}}
	st := &stubStore{credit: billing.AccountCredit{AccountID: "buyer-1", CreditRaw: 1 << 62},
		asks: []billing.Ask{{PeerID: "peer-A", Service: "llm", Model: "m1", InputPerMillion: 100, OutputPerMillion: 100, Revision: 1}}}
	s := newGate(t, config.BillingEnforce, st, snap)
	var ctxID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := billing.RequestID(r.Context()); ok {
			ctxID = id
		}
		w.WriteHeader(http.StatusOK)
	})
	h := s.Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", strings.NewReader(`{"model":"m1","max_tokens":1}`))
	req = withAccount(req, "buyer-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ctxID == "" {
		t.Fatal("no request id in forwarded context")
	}
	if st.reserved == nil || st.reserved.RequestID != ctxID {
		t.Fatalf("context id %q != reservation %v", ctxID, st.reserved)
	}
}

func int64Ptr(v int64) *int64 { return &v }
