// Package catalog serves a distilled, public view of what the mesh is serving.
//
// The upstream node table answers "what can I call?", but it also carries
// operator wallet keys, peer addresses and hardware inventories — none of which
// a caller needs for that question. This package reduces it to service names,
// the models they answer for, and how many providers are online, so the raw
// table can stay behind the API key.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/store"
)

// Service is one row of the public catalogue.
type Service struct {
	Name string `json:"name"`
	// Models the service answers for, taken from its "model=" identity groups.
	Models []string `json:"models"`
	// IdentityGroups as advertised, e.g. ["all"] for a catch-all service.
	IdentityGroups []string `json:"identity_groups"`
	Providers      int      `json:"providers"`
	Online         int      `json:"online"`
}

// Response is the JSON body served to callers.
type Response struct {
	Services []Service `json:"services"`
}

// PeerService is the subset of an upstream service entry this package reads.
type PeerService struct {
	Name          string   `json:"name"`
	IdentityGroup []string `json:"identity_group"`
}

// Peer is the subset of an upstream node-table entry this package reads.
// Everything else in that payload is deliberately dropped.
type Peer struct {
	Connected bool          `json:"connected"`
	Service   []PeerService `json:"service"`
}

const modelPrefix = "model="

// Summarise collapses the per-peer node table into one row per service.
// Peers that publish no services (relays, head nodes) contribute nothing.
func Summarise(table map[string]Peer) []Service {
	type acc struct {
		models    map[string]struct{}
		groups    map[string]struct{}
		providers int
		online    int
	}
	byName := map[string]*acc{}

	for _, peer := range table {
		for _, svc := range peer.Service {
			if svc.Name == "" {
				continue
			}
			entry, ok := byName[svc.Name]
			if !ok {
				entry = &acc{models: map[string]struct{}{}, groups: map[string]struct{}{}}
				byName[svc.Name] = entry
			}
			entry.providers++
			if peer.Connected {
				entry.online++
			}
			for _, group := range svc.IdentityGroup {
				if group == "" {
					continue
				}
				entry.groups[group] = struct{}{}
				if len(group) > len(modelPrefix) && group[:len(modelPrefix)] == modelPrefix {
					entry.models[group[len(modelPrefix):]] = struct{}{}
				}
			}
		}
	}

	out := make([]Service, 0, len(byName))
	for name, entry := range byName {
		out = append(out, Service{
			Name:           name,
			Models:         sortedKeys(entry.models),
			IdentityGroups: sortedKeys(entry.groups),
			Providers:      entry.providers,
			Online:         entry.online,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Handler serves the distilled catalogue, caching the upstream read so a public
// endpoint cannot be used to hammer the node.
type Handler struct {
	upstream *url.URL
	client   *http.Client
	ttl      time.Duration
	policy   policyStore

	mu      sync.Mutex
	cached  []Service
	fetched time.Time
}

type policyStore interface {
	ListManagedInstancesByPeerIDs(ctx context.Context, peerIDs []string) ([]store.InstanceInfo, error)
}

// New returns a Handler reading the node table from upstream, serving each
// result for up to ttl.
func New(upstream *url.URL, ttl time.Duration) *Handler {
	return NewWithPolicies(upstream, ttl, nil)
}

func NewWithPolicies(upstream *url.URL, ttl time.Duration, policy policyStore) *Handler {
	return &Handler{
		upstream: upstream,
		client:   &http.Client{Timeout: 10 * time.Second},
		ttl:      ttl,
		policy:   policy,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	services, err := h.services(r.Context())
	if err != nil {
		status := http.StatusBadGateway
		var policyErr errPolicyUnavailable
		if errors.As(err, &policyErr) {
			// Once policy filtering is enabled, an unavailable policy store makes
			// the permissionless/trusted classification indeterminate. Fail
			// closed as a control-plane outage rather than presenting unfiltered
			// mesh data as a public catalogue.
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]string{"error": "catalogue unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, Response{Services: services})
}

func (h *Handler) services(ctx context.Context) ([]Service, error) {
	h.mu.Lock()
	if h.cached != nil && time.Since(h.fetched) < h.ttl {
		cached := h.cached
		h.mu.Unlock()
		return cached, nil
	}
	h.mu.Unlock()

	table, err := h.fetchTable(ctx)
	if err != nil {
		return nil, err
	}
	if h.policy != nil {
		table, err = h.filterTable(ctx, table)
		if err != nil {
			return nil, errPolicyUnavailable{err: err}
		}
	}
	services := Summarise(table)

	h.mu.Lock()
	h.cached, h.fetched = services, time.Now()
	h.mu.Unlock()
	return services, nil
}

func (h *Handler) fetchTable(ctx context.Context) (map[string]Peer, error) {
	target := h.upstream.JoinPath("v1", "dnt", "table")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
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

func (h *Handler) filterTable(ctx context.Context, table map[string]Peer) (map[string]Peer, error) {
	peerIDs := make([]string, 0, len(table))
	for peerID := range table {
		peerIDs = append(peerIDs, peerID)
	}
	managed, err := h.policy.ListManagedInstancesByPeerIDs(ctx, peerIDs)
	if err != nil {
		return nil, err
	}
	managedByPeer := make(map[string]store.InstanceInfo, len(managed))
	for _, inst := range managed {
		managedByPeer[inst.PeerID] = inst
	}
	filtered := make(map[string]Peer, len(table))
	for peerID, peer := range table {
		inst, ok := managedByPeer[peerID]
		if !ok {
			filtered[peerID] = peer
			continue
		}
		switch inst.PolicyScope {
		case store.PolicyScopeService:
			allowed := make([]PeerService, 0, len(peer.Service))
			for _, svc := range peer.Service {
				if service, ok := findPermissionlessService(inst.Services, svc.Name); ok {
					_ = service
					allowed = append(allowed, svc)
				}
			}
			peer.Service = allowed
			filtered[peerID] = peer
		default:
			if inst.Membership == nil {
				filtered[peerID] = peer
				continue
			}
			peer.Service = nil
			filtered[peerID] = peer
		}
	}
	return filtered, nil
}

func findPermissionlessService(services []store.InstanceService, name string) (store.InstanceService, bool) {
	for _, svc := range services {
		if svc.ServiceName == name && svc.Exposure == store.ExposurePermissionless {
			return svc, true
		}
	}
	return store.InstanceService{}, false
}

type errUpstream struct{ status int }

func (e errUpstream) Error() string { return http.StatusText(e.status) }

type errPolicyUnavailable struct{ err error }

func (e errPolicyUnavailable) Error() string { return e.err.Error() }
func (e errPolicyUnavailable) Unwrap() error { return e.err }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
