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

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/peers"
)

// AskSource is the live-ask feed the catalog distils into the `market`
// array. It is satisfied by *store.Postgres (via its LiveAsks) and by the
// billing gate's BillingStore; the catalog accepts it as an interface so the
// public price surface and the gate never disagree about who is quoting.
type AskSource interface {
	LiveAsks(ctx context.Context, service, model string, now time.Time) ([]billing.Ask, error)
}

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

// Response is the JSON body served to callers. Market is non-nil only when
// the handler is wired with a live-ask source (billing active); otherwise it
// is omitted so callers see no prices at all.
type Response struct {
	Services []Service     `json:"services"`
	Market   []MarketEntry `json:"market,omitempty"`
}

// MarketEntry is one row of the price discovery surface: for each (service,
// model) the mesh serves, the minimum, median, and maximum published rate per
// million tokens, plus how many providers are quoting. A provider count of
// zero means the model is served but nobody has published an ask yet
// (requests would 503 `no_provider` under enforcement).
type MarketEntry struct {
	Service     string      `json:"service"`
	Model       string      `json:"model"`
	Quoters     int         `json:"quoters"`
	Input       priceTriple `json:"input_per_million"`
	CachedInput priceTriple `json:"cached_input_per_million"`
	Output      priceTriple `json:"output_per_million"`
}

// priceTriple is the min/median/max published rate for one tier.
type priceTriple struct {
	Min    int64 `json:"min"`
	Median int64 `json:"median"`
	Max    int64 `json:"max"`
}

// Model is one entry in the OpenAI-shaped model list served at
// /v1/service/{service}/v1/models. ID is the served-model alias (the value of
// a peer's "model=" identity group); the list is scoped to the single service
// named in the request path. Created is the catalogue's last refresh time — a
// stable "as of" stamp, not a per-model creation date the mesh does not know.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the OpenAI-shaped {"object":"list","data":[...]} response.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// PeerService is the subset of an upstream service entry this package reads.
// It is an alias for peers.PeerService so the catalog and the billing gate
// share one shape without importing each other.
type PeerService = peers.PeerService

// Peer is the subset of an upstream node-table entry this package reads.
// Everything else in that payload is deliberately dropped.
type Peer = peers.Peer

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
// endpoint cannot be used to hammer the node. The policy-filtered snapshot is
// shared with the billing gate (internal/peers): this handler summarizes it for
// the public catalog, the gate resolves seller identity from it.
type Handler struct {
	snap *peers.Service
	ttl  time.Duration
	asks AskSource // nil → the `market` array is omitted

	mu         sync.Mutex
	cached     []Service
	cachedSnap *peers.Snapshot
	fetched    time.Time
}

// WithAsks wires a live-ask source so /v1/services also serves a `market`
// array (price discovery). It is chainable; without it the response keeps the
// original shape (services only, no prices).
func (h *Handler) WithAsks(asks AskSource) *Handler {
	h.asks = asks
	return h
}

// NewWithPolicies returns a Handler that owns its own peer snapshot. Most
// production callers should use NewWithSnapshot with a shared *peers.Service
// so the catalog and the billing gate read from one source of truth; this
// constructor remains for tests that exercise the catalog in isolation.
//
// policy is accepted as peers.PolicyStore so existing callers (catalog tests
// with a local stub, main.go with *store.Postgres) satisfy it without change:
// the stub implements ListManagedInstancesByPeerIDs directly.
func NewWithPolicies(upstream *url.URL, ttl time.Duration, policy peers.PolicyStore) *Handler {
	return NewWithSnapshot(peers.New(upstream, ttl, policy), ttl)
}

// NewWithSnapshot returns a Handler reading from an externally-owned, shared
// *peers.Service so the public catalogue and the billing gate observe the
// exact same policy-filtered providers. ttl bounds the catalogue's own
// distillation cache (a separate concern from the peer snapshot's TTL).
func NewWithSnapshot(snap *peers.Service, ttl time.Duration) *Handler {
	return &Handler{snap: snap, ttl: ttl}
}

// ServeHTTP dispatches by route. GET /v1/services — registered without a
// {service} path value — serves the distilled catalogue. GET /v1/service/
// {service}/v1/models serves an OpenAI-shaped model list scoped to that one
// service, so OpenAI-compatible clients that hard-code GET {base}/models get a
// valid list instead of the upstream's "no provider found" 503 (the mesh route
// is for inference, not listing). Both responses share the same upstream
// fetch, cache, and policy filtering; the caller (internal/server) decides per
// route whether the API-key middleware is applied.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if svc := r.PathValue("service"); svc != "" {
		h.serveModels(w, r, svc)
		return
	}
	h.serveCatalogue(w, r)
}

func (h *Handler) serveCatalogue(w http.ResponseWriter, r *http.Request) {
	services, snap, err := h.view(r.Context())
	if err != nil {
		writeCatalogueError(w, err)
		return
	}
	resp := Response{Services: services}
	if h.asks != nil {
		market, err := h.market(r.Context(), services, snap)
		if err != nil {
			writeCatalogueError(w, err)
			return
		}
		resp.Market = market
	}
	writeJSON(w, http.StatusOK, resp)
}

// serveModels writes an OpenAI-shaped model list for the named service. An
// unknown service yields an empty list (still 200): the OpenAI model-list
// contract is a list, not a lookup, and some clients reject a 404 here.
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, service string) {
	services, _, err := h.view(r.Context())
	if err != nil {
		writeCatalogueError(w, err)
		return
	}
	h.mu.Lock()
	created := h.fetched.Unix()
	h.mu.Unlock()
	if created < 0 {
		created = 0
	}
	writeJSON(w, http.StatusOK, ModelList{
		Object: "list",
		Data:   modelsForService(services, service, created),
	})
}

// market distils live asks into one price-discovery row per (service, model)
// the mesh serves. For each row it queries the ask source, drops expired
// quotes, and reports the min/median/max of the three published rates plus
// how many providers are quoting. Models that are served but have no live
// ask still appear, with quoters=0 and zeroed triples, so the UI can show
// "not yet priced" rather than silently omitting the route.
// ErrAskUnavailable is returned by market when the live-ask source fails;
// the catalogue surfaces it as 503 (service outage) rather than the 502 used
// for upstream mesh errors.
var ErrAskUnavailable = errors.New("catalogue: ask source unavailable")

func (h *Handler) market(ctx context.Context, services []Service, snap *peers.Snapshot) ([]MarketEntry, error) {
	now := time.Now().UTC()
	var out []MarketEntry
	for _, svc := range services {
		for _, model := range svc.Models {
			asks, err := h.asks.LiveAsks(ctx, svc.Name, model, now)
			if err != nil {
				return nil, ErrAskUnavailable
			}
			out = append(out, marketTriple(svc.Name, model, asks, snap, now))
		}
	}
	return out, nil
}

// marketTriple reduces one (service, model)'s live asks to a MarketEntry,
// dropping any quote whose ExpiresAt has passed or whose peer is not in the
// current billable/routable snapshot for that route.
func marketTriple(service, model string, asks []billing.Ask, snap *peers.Snapshot, now time.Time) MarketEntry {
	live := make([]billing.Ask, 0, len(asks))
	for _, a := range asks {
		if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(now) {
			continue
		}
		if snap == nil || !peerCanQuote(snap, a.PeerID, service, model) {
			continue
		}
		live = append(live, a)
	}
	if len(live) == 0 {
		return MarketEntry{Service: service, Model: model}
	}
	inputs := rates(live, func(a billing.Ask) int64 { return a.InputPerMillion })
	cached := rates(live, func(a billing.Ask) int64 { return a.CachedInputPerMillion })
	outputs := rates(live, func(a billing.Ask) int64 { return a.OutputPerMillion })
	return MarketEntry{
		Service:     service,
		Model:       model,
		Quoters:     len(live),
		Input:       triple(inputs),
		CachedInput: triple(cached),
		Output:      triple(outputs),
	}
}

// rates collects one rate from each live ask and sorts ascending.
func rates(asks []billing.Ask, pick func(billing.Ask) int64) []int64 {
	out := make([]int64, len(asks))
	for i, a := range asks {
		out[i] = pick(a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// triple returns the min/median/max of a sorted-ascending slice.
func triple(sorted []int64) priceTriple {
	n := len(sorted)
	if n == 0 {
		return priceTriple{}
	}
	med := sorted[n/2]
	if n%2 == 0 {
		med = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return priceTriple{Min: sorted[0], Median: med, Max: sorted[n-1]}
}

// modelsForService returns one OpenAI Model entry per served-model alias the
// named service advertises. The mesh reports models as "model=<alias>"
// identity groups; Summarise already dedupes and sorts them, so this just
// reshapes that list.
func modelsForService(services []Service, service string, created int64) []Model {
	for _, svc := range services {
		if svc.Name != service {
			continue
		}
		out := make([]Model, 0, len(svc.Models))
		for _, id := range svc.Models {
			out = append(out, Model{
				ID:      id,
				Object:  "model",
				Created: created,
				OwnedBy: "opentela",
			})
		}
		return out
	}
	return nil
}

// writeCatalogueError maps an upstream or policy failure to the same status as
// the public catalogue: a control-plane outage (policy lookup down) is 503;
// anything else is 502. Shared by the catalogue and the model list, which use
// the same upstream fetch.
func writeCatalogueError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, peers.ErrPolicyUnavailable) || errors.Is(err, ErrAskUnavailable) {
		// Once policy filtering is enabled, an unavailable policy store makes
		// the permissionless/trusted classification indeterminate. Fail closed
		// as a control-plane outage rather than presenting unfiltered mesh data.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]string{"error": "catalogue unavailable"})
}

// ServicesForPricing returns the distilled service list for the pricing
// API's allowlist, without exposing the catalog's internal cache state. The
// pricing package converts []Service → []AllowEntry.
func (h *Handler) ServicesForPricing(ctx context.Context) ([]Service, error) {
	services, _, err := h.view(ctx)
	return services, err
}

func (h *Handler) services(ctx context.Context) ([]Service, error) {
	services, _, err := h.view(ctx)
	return services, err
}

func (h *Handler) view(ctx context.Context) ([]Service, *peers.Snapshot, error) {
	h.mu.Lock()
	if h.cached != nil && h.cachedSnap != nil && time.Since(h.fetched) < h.ttl {
		cached := h.cached
		snap := h.cachedSnap
		h.mu.Unlock()
		return cached, snap, nil
	}
	h.mu.Unlock()

	snap, err := h.snap.Snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	table := make(map[string]Peer, len(snap.Entries))
	for id, e := range snap.Entries {
		table[id] = e.Peer
	}
	services := Summarise(table)

	h.mu.Lock()
	h.cached, h.cachedSnap, h.fetched = services, snap, time.Now()
	h.mu.Unlock()
	return services, snap, nil
}

func peerCanQuote(snap *peers.Snapshot, peerID, service, model string) bool {
	entry, ok := snap.Entries[peerID]
	if !ok || entry.Instance == nil {
		return false
	}
	if !billing.BillableProvider(entry.Instance.AccountID, entry.Instance.OwnerWallet) {
		return false
	}
	modelTag := modelPrefix + model
	for _, svc := range entry.Peer.Service {
		if svc.Name != service {
			continue
		}
		for _, group := range svc.IdentityGroup {
			if group == modelTag {
				return true
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
