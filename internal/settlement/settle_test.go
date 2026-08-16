package settlement

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/perf"
)

type fakeStore struct {
	settled      []settleCall
	released     []releaseCall
	settleErr    error
	releaseErr   error
	settleResult billing.Request
}

type settleCall struct {
	requestID  string
	servedPeer string
	usage      billing.Usage
	feeBps     int
	now        time.Time
}

type releaseCall struct {
	requestID string
	reason    string
	now       time.Time
}

func (f *fakeStore) SettleBilling(ctx context.Context, requestID, servedPeerID string, usage billing.Usage, feeBps int, now time.Time) (billing.Request, error) {
	f.settled = append(f.settled, settleCall{requestID, servedPeerID, usage, feeBps, now})
	if f.settleErr != nil {
		return billing.Request{}, f.settleErr
	}
	return f.settleResult, nil
}

func (f *fakeStore) ReleaseBilling(ctx context.Context, requestID, reason string, now time.Time) (billing.Request, error) {
	f.released = append(f.released, releaseCall{requestID, reason, now})
	if f.releaseErr != nil {
		return billing.Request{}, f.releaseErr
	}
	return billing.Request{}, nil
}

func newSettler(t *testing.T, store *fakeStore) *Settler {
	t.Helper()
	return New(store, 500, time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
}

func reqWithReservation(id string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/service/llm/v1/chat/completions", nil)
	if id != "" {
		r = r.WithContext(billing.WithRequestID(r.Context(), id))
	}
	return r
}

// respWith wraps an inbound request (which carries the billing reservation id
// in context) in a response, mirroring how the reverse proxy threads the
// gate-stamped context onto resp.Request. The response status is irrelevant
// here: the settler reads u.Status from SettleUsage, not the response.
func respWith(req *http.Request) *http.Response {
	return &http.Response{StatusCode: 200, Request: req}
}

func TestSettlerSettlesComplete2xx(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	u := perf.SettleUsage{
		ServedPeerID: "peer-A", Status: 200, InputTokens: 800,
		CachedInputTokens: 200, OutputTokens: 50, Complete: true,
	}
	s.Callback(respWith(reqWithReservation("req-1")), u)

	if len(store.settled) != 1 || len(store.released) != 0 {
		t.Fatalf("settled=%d released=%d, want 1/0", len(store.settled), len(store.released))
	}
	c := store.settled[0]
	if c.requestID != "req-1" || c.servedPeer != "peer-A" {
		t.Errorf("call = %+v", c)
	}
	if c.usage.InputTokens != 800 || c.usage.CachedInputTokens != 200 || c.usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want Input=800 Cached=200 Output=50", c.usage)
	}
	if c.feeBps != 500 {
		t.Errorf("feeBps = %d, want 500", c.feeBps)
	}
}

func TestSettlerReleasesNon2xx(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	s.Callback(respWith(reqWithReservation("req-2")), perf.SettleUsage{Status: 500, ServedPeerID: "p"})

	if len(store.released) != 1 || len(store.settled) != 0 {
		t.Fatalf("settled=%d released=%d, want 0/1", len(store.settled), len(store.released))
	}
	if store.released[0].reason != "non_2xx" {
		t.Errorf("reason = %q, want non_2xx", store.released[0].reason)
	}
}

func TestSettlerReleasesClientAbort(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	s.Callback(respWith(reqWithReservation("req-3")), perf.SettleUsage{Status: 200, ClientAbort: true})

	if len(store.released) != 1 || store.released[0].reason != "client_abort" {
		t.Fatalf("released=%v reason=%q", store.released, store.released[0].reason)
	}
}

func TestSettlerReleasesNoUsage(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	// 2xx but zero tokens (streaming without include_usage).
	s.Callback(respWith(reqWithReservation("req-4")), perf.SettleUsage{Status: 200, Complete: false})

	if len(store.released) != 1 || store.released[0].reason != "no_usage" {
		t.Fatalf("released=%v reason=%q", store.released, store.released[0].reason)
	}
}

func TestSettlerSkipsWithoutReservation(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	// No request id in context (observe mode or non-metered route).
	s.Callback(respWith(reqWithReservation("")), perf.SettleUsage{Status: 200, InputTokens: 10, OutputTokens: 5, Complete: true})

	if len(store.settled) != 0 || len(store.released) != 0 {
		t.Fatalf("settled=%d released=%d, want 0/0", len(store.settled), len(store.released))
	}
}

func TestSettlerSettleUnknownPeerReleases(t *testing.T) {
	// SettleBilling itself releases an unknown peer (it resolves against the
	// snapshot), so the settler just calls SettleBilling and lets the store
	// decide. Here we verify the settler doesn't double-call release.
	store := &fakeStore{settleResult: billing.Request{State: billing.StateReleased}}
	s := newSettler(t, store)
	s.Callback(respWith(reqWithReservation("req-5")), perf.SettleUsage{
		ServedPeerID: "ghost", Status: 200, InputTokens: 10, OutputTokens: 5, Complete: true,
	})
	if len(store.settled) != 1 || len(store.released) != 0 {
		t.Fatalf("settled=%d released=%d, want 1/0 (store decides)", len(store.settled), len(store.released))
	}
}

func TestSettlerIdempotentNotFoundNotLogged(t *testing.T) {
	// If the reservation was already swept by the recovery worker, the store
	// returns ErrNotFound; the settler must not treat that as a hard error.
	store := &fakeStore{settleErr: billing.ErrNotFound}
	s := newSettler(t, store)
	logged := false
	s.log = func(string, ...any) { logged = true }
	s.Callback(respWith(reqWithReservation("req-6")), perf.SettleUsage{
		ServedPeerID: "p", Status: 200, InputTokens: 10, OutputTokens: 5, Complete: true,
	})
	if logged {
		t.Error("ErrNotFound was logged; it should be silently absorbed")
	}
}

func TestSettlerLogsOtherErrors(t *testing.T) {
	store := &fakeStore{releaseErr: errors.New("db down")}
	s := newSettler(t, store)
	logged := ""
	s.log = func(format string, args ...any) { logged = format }
	s.Callback(respWith(reqWithReservation("req-7")), perf.SettleUsage{Status: 500})
	if logged == "" {
		t.Error("expected a log line for a non-NotFound error")
	}
}

func TestSettlerNilRequest(t *testing.T) {
	store := &fakeStore{}
	s := newSettler(t, store)
	s.Callback(nil, perf.SettleUsage{Status: 200, Complete: true})
	if len(store.settled) != 0 || len(store.released) != 0 {
		t.Fatal("nil request must be a no-op")
	}
}

func TestSettlerFeeBpsClamp(t *testing.T) {
	store := &fakeStore{}
	s := New(store, -5, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if s.feeBps != 0 {
		t.Errorf("negative feeBps clamped to %d, want 0", s.feeBps)
	}
	s.SetFeeBps(99999)
	if s.feeBps != 10000 {
		t.Errorf("overflow feeBps clamped to %d, want 10000", s.feeBps)
	}
}

var _ Store = (billing.BillingStore)(nil)

type fakeSweeperStore struct {
	before time.Time
	limit  int
	called int
	err    error
}

func (f *fakeSweeperStore) SweepStaleReservations(ctx context.Context, before time.Time, limit int) ([]string, error) {
	f.called++
	f.before = before
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return []string{"a", "b"}, nil
}

func TestSweeperDefaults(t *testing.T) {
	s := NewSweeper(&fakeSweeperStore{}, 0, 0, 0)
	if s.interval != time.Minute || s.age != 10*time.Minute || s.limit != 100 {
		t.Fatalf("defaults = %v/%v/%d", s.interval, s.age, s.limit)
	}
}

func TestSweeperClampsLimit(t *testing.T) {
	s := NewSweeper(&fakeSweeperStore{}, time.Second, time.Second, 99999)
	if s.limit != 1000 {
		t.Fatalf("limit = %d, want 1000", s.limit)
	}
}
