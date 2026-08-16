package peers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/store"
)

// --- stubs ---

type stubPolicy struct {
	instances []store.InstanceInfo
	err       error
	calls     atomic.Int64
}

func (s *stubPolicy) ListManagedInstancesByPeerIDs(_ context.Context, _ []string) ([]store.InstanceInfo, error) {
	s.calls.Add(1)
	return s.instances, s.err
}

func tableServer(t *testing.T, table map[string]Peer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(table)
	}))
}

func makePeerWithModels(connected bool, models ...string) Peer {
	svc := []PeerService{{Name: "llm", IdentityGroup: append([]string{}, models...)}}
	return Peer{Connected: connected, Service: svc}
}

// --- tests ---

func TestSnapshotNilPolicyFails(t *testing.T) {
	srv := tableServer(t, map[string]Peer{"p": makePeerWithModels(true, "all")})
	defer srv.Close()
	s := New(mustURL(t, srv.URL), time.Minute, nil)
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("got %v, want ErrPolicyUnavailable", err)
	}
}

func TestSnapshotPolicyErrorFails(t *testing.T) {
	srv := tableServer(t, map[string]Peer{"p": makePeerWithModels(true, "all")})
	defer srv.Close()
	s := New(mustURL(t, srv.URL), time.Minute, &stubPolicy{err: errors.New("boom")})
	if _, err := s.Snapshot(context.Background()); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("got %v, want ErrPolicyUnavailable", err)
	}
}

func TestSnapshotNoInstanceKeepsAllServices(t *testing.T) {
	srv := tableServer(t, map[string]Peer{
		"p1": makePeerWithModels(true, "llama3.1-70b", "all"),
	})
	defer srv.Close()
	s := New(mustURL(t, srv.URL), time.Minute, &stubPolicy{instances: nil})
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := snap.Entries["p1"]
	if !ok {
		t.Fatal("peer p1 missing")
	}
	if len(e.Peer.Service[0].IdentityGroup) != 2 {
		t.Fatalf("expected all services kept for unmanaged peer, got %d", len(e.Peer.Service[0].IdentityGroup))
	}
}

func TestSnapshotServiceScopeFiltersToPermissionless(t *testing.T) {
	srv := tableServer(t, map[string]Peer{
		"p1": {Connected: true, Service: []PeerService{
			{Name: "llm", IdentityGroup: []string{"model=llama3.1-70b"}},
			{Name: "private-llm", IdentityGroup: []string{"all"}},
		}},
	})
	defer srv.Close()
	policy := &stubPolicy{instances: []store.InstanceInfo{{
		PeerID:      "p1",
		PolicyScope: store.PolicyScopeService,
		Services: []store.InstanceService{
			{ServiceName: "llm", Exposure: store.ExposurePermissionless},
			{ServiceName: "private-llm", Exposure: store.ExposureTrustedRegion},
		},
	}}}
	s := New(mustURL(t, srv.URL), time.Minute, policy)
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := snap.Entries["p1"]
	if len(e.Peer.Service) != 1 {
		t.Fatalf("expected 1 service (permissionless only), got %d", len(e.Peer.Service))
	}
	if e.Peer.Service[0].Name != "llm" {
		t.Fatalf("expected llm, got %s", e.Peer.Service[0].Name)
	}
}

func TestSnapshotTrustedRegionDropsAllServices(t *testing.T) {
	srv := tableServer(t, map[string]Peer{
		"p1": {Connected: true, Service: []PeerService{
			{Name: "llm", IdentityGroup: []string{"all"}},
		}},
	})
	defer srv.Close()
	membership := &store.RegionMembership{}
	policy := &stubPolicy{instances: []store.InstanceInfo{{
		PeerID:      "p1",
		PolicyScope: store.PolicyScopePeer,
		Membership:  membership,
	}}}
	s := New(mustURL(t, srv.URL), time.Minute, policy)
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := snap.Entries["p1"]
	if len(e.Peer.Service) != 0 {
		t.Fatalf("trusted-region peer should have zero services, got %d", len(e.Peer.Service))
	}
}

func TestSnapshotCache(t *testing.T) {
	srv := tableServer(t, map[string]Peer{"p1": makePeerWithModels(true, "all")})
	defer srv.Close()
	policy := &stubPolicy{}
	s := New(mustURL(t, srv.URL), 50*time.Millisecond, policy)

	if _, err := s.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if policy.calls.Load() != 1 {
		t.Fatalf("expected 1 policy call, got %d", policy.calls.Load())
	}
	// Cached read: no new policy call.
	if _, err := s.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if policy.calls.Load() != 1 {
		t.Fatalf("expected 1 policy call (cached), got %d", policy.calls.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := s.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if policy.calls.Load() != 2 {
		t.Fatalf("expected 2 policy calls after TTL, got %d", policy.calls.Load())
	}
}

func TestSnapshotConcurrentSafe(t *testing.T) {
	srv := tableServer(t, map[string]Peer{"p1": makePeerWithModels(true, "all")})
	defer srv.Close()
	s := New(mustURL(t, srv.URL), time.Minute, &stubPolicy{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Snapshot(context.Background()); err != nil {
				t.Errorf("snapshot: %v", err)
			}
		}()
	}
	wg.Wait()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
