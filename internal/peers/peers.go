// Package peers fetches the upstream mesh node table and filters it through
// the instance-ACL policy store, producing a Snapshot that the public catalog
// (internal/catalog) and the billing gate (internal/billinggate) both consume.
//
// The snapshot carries, per live peer: the filtered services it advertises
// (permissionless only — trusted-region services are never leaked to anonymous
// callers) and the owning instance info (seller account id, owner wallet) that
// the billing gate resolves into quote snapshots. A cache may accelerate the
// read, but a cached read must never authorize or move a balance: only the
// row-locked transaction in store.ReserveBilling does.
package peers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/store"
)

// PeerService is the subset of an upstream service entry this package reads.
// (Mirrors catalog.PeerService so neither package imports the other for a
// pure data shape.)
type PeerService struct {
	Name          string   `json:"name"`
	IdentityGroup []string `json:"identity_group"`
}

// Peer is the subset of an upstream node-table entry this package reads.
type Peer struct {
	Connected bool          `json:"connected"`
	Service   []PeerService `json:"service"`
}

// Entry pairs a filtered peer with its owning instance, if any. The billing
// gate resolves the seller account id and owner wallet from Instance; the
// catalog uses only Peer.
type Entry struct {
	Peer     Peer
	Instance *store.InstanceInfo
}

// Snapshot is a policy-filtered view of the upstream node table at a point in
// time. Entries is keyed by peer id; Expiry is when the server-assigned ask
// TTL would next expire (informational).
type Snapshot struct {
	Entries map[string]Entry
	Fetched time.Time
}

// PolicyStore is the minimal contract the snapshot needs to filter peers
// against instance policy and to resolve seller identity. It is satisfied by
// store.Postgres via ListManagedInstancesByPeerIDs.
type PolicyStore interface {
	ListManagedInstancesByPeerIDs(ctx context.Context, peerIDs []string) ([]store.InstanceInfo, error)
}

// Service is the minimal contract the snapshot needs to filter peers against
// instance policy and to resolve seller identity.
type Service struct {
	upstream *url.URL
	client   *http.Client
	ttl      time.Duration
	policy   PolicyStore

	mu      sync.Mutex
	cached  *Snapshot
	fetched time.Time
}

// New returns a Service reading the node table from upstream, serving each
// result for up to ttl. A non-nil policy store is required so trusted-region
// services are never leaked: filterTable drops everything that is not
// permissionless and fails closed (503) when the store is unreachable.
func New(upstream *url.URL, ttl time.Duration, policy PolicyStore) *Service {
	return &Service{
		upstream: upstream,
		client:   &http.Client{Timeout: 10 * time.Second},
		ttl:      ttl,
		policy:   policy,
	}
}

// Snapshot returns the cached policy-filtered table if fresh, otherwise fetches
// a new one. A nil policy store yields ErrPolicyUnavailable: without one, the
// permissionless/trusted classification is indeterminate and the raw upstream
// table would expose trusted-region services.
func (s *Service) Snapshot(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.fetched) < s.ttl {
		snap := s.cached
		s.mu.Unlock()
		return snap, nil
	}
	s.mu.Unlock()

	table, err := s.fetchTable(ctx)
	if err != nil {
		return nil, err
	}
	if s.policy == nil {
		return nil, ErrPolicyUnavailable
	}
	managed, err := s.policy.ListManagedInstancesByPeerIDs(ctx, peerIDs(table))
	if err != nil {
		return nil, ErrPolicyUnavailable
	}
	managedByPeer := make(map[string]*store.InstanceInfo, len(managed))
	for i := range managed {
		m := managed[i] // take address of loop-local copy
		managedByPeer[m.PeerID] = &m
	}

	entries := make(map[string]Entry, len(table))
	for peerID, peer := range table {
		inst := managedByPeer[peerID]
		switch {
		case inst == nil:
			entries[peerID] = Entry{Peer: peer}
		case inst.PolicyScope == store.PolicyScopeService:
			allowed := make([]PeerService, 0, len(peer.Service))
			for _, svc := range peer.Service {
				if _, ok := findPermissionlessService(inst.Services, svc.Name); ok {
					allowed = append(allowed, svc)
				}
			}
			peer.Service = allowed
			entries[peerID] = Entry{Peer: peer, Instance: inst}
		default:
			if inst.Membership == nil {
				entries[peerID] = Entry{Peer: peer, Instance: inst}
				continue
			}
			// Trusted-region instance: drop all services so it never appears
			// in the public catalog or the billing-eligible set.
			peer.Service = nil
			entries[peerID] = Entry{Peer: peer, Instance: inst}
		}
	}

	snap := &Snapshot{Entries: entries, Fetched: time.Now().UTC()}
	s.mu.Lock()
	s.cached, s.fetched = snap, snap.Fetched
	s.mu.Unlock()
	return snap, nil
}

func peerIDs(table map[string]Peer) []string {
	ids := make([]string, 0, len(table))
	for id := range table {
		ids = append(ids, id)
	}
	return ids
}

func findPermissionlessService(services []store.InstanceService, name string) (store.InstanceService, bool) {
	for _, svc := range services {
		if svc.ServiceName == name && svc.Exposure == store.ExposurePermissionless {
			return svc, true
		}
	}
	return store.InstanceService{}, false
}

func (s *Service) fetchTable(ctx context.Context) (map[string]Peer, error) {
	target := s.upstream.JoinPath("v1", "dnt", "table")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errUpstream{status: resp.StatusCode}
	}
	var table map[string]Peer
	if err := json.NewDecoder(resp.Body).Decode(&table); err != nil {
		return nil, err
	}
	return table, nil
}

// ErrPolicyUnavailable means the policy store is not configured or unreachable,
// so the permissionless/trusted classification is indeterminate.
var ErrPolicyUnavailable = errors.New("peers: policy unavailable")

type errUpstream struct{ status int }

func (e errUpstream) Error() string { return "peers: upstream " + http.StatusText(e.status) }
