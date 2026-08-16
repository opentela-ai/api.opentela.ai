// Package pricingapi serves POST /internal/pricing, the seller ask-publication
// endpoint. A seller authenticates with a pricing-scoped node credential
// (internal/nodecred.PricingAudience, distinct from the ACL credential so
// expanding seller eligibility cannot weaken trusted-region ACL), and the
// handler performs a full replacement of that peer's asks against the peer's
// own live advertised service/model pairs before persisting them with a
// server-assigned TTL.
package pricingapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/store"
)

// askTTL is the server-assigned expiry. Peers republish on a shorter cadence
// (every two minutes) so asks never go stale while the peer is live.
const askTTL = 5 * time.Minute

// maxBody bounds a pricing POST (256 asks × ~120 bytes ≈ 31 KiB).
const maxBody = 256 << 10

type pricingStore interface {
	ReplaceAsks(ctx context.Context, peerID string, asks []billing.Ask, ttl time.Duration) (int64, error)
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
}

type pricingMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

// AllowEntry is one (service, models) pair the mesh serves, kept for the
// existing constructor surface.
type AllowEntry struct {
	Name   string
	Models []string
}

// Allowlist is retained for constructor compatibility with the existing server
// wiring. Publication truth is enforced against the seller's own live
// advertisement, not a catalog-wide service list.
type Allowlist interface {
	Services(ctx context.Context) ([]AllowEntry, error)
}

// Service handles seller ask publication.
type Service struct {
	store    pricingStore
	mesh     pricingMesh
	verifier nodeVerifier
	allow    Allowlist
	now      func() time.Time
	// ownershipMaxAge bounds how old a peer observation may be while still
	// satisfying "observed wallet still matches"; zero disables the check.
	ownershipMaxAge time.Duration
}

type nodeVerifier interface {
	Verify(ctx context.Context, raw string) (nodecred.Claims, error)
}

// askRequest is the JSON body of POST /internal/pricing. An empty list clears
// all of the peer's asks.
type askRequest struct {
	Asks []billing.Ask `json:"asks"`
}

// askResponse echoes the persisted revision and expiry.
type askResponse struct {
	Revision  int64     `json:"revision"`
	ExpiresAt time.Time `json:"expires_at"`
}

// New returns a Service. verifier must be configured with
// nodecred.PricingAudience; pg provides instance lookup and ask persistence.
// allow is retained for the existing constructor surface.
func New(pg pricingStore, meshClient pricingMesh, verifier nodeVerifier, allow Allowlist, ownershipMaxAge time.Duration) *Service {
	return &Service{
		store: pg, mesh: meshClient, verifier: verifier, allow: allow,
		now: time.Now, ownershipMaxAge: ownershipMaxAge,
	}
}

// Handler returns the POST /internal/pricing HTTP handler.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("X-Otela-Control-Auth-Failed", "true")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req askRequest
		if err := httputil.DecodeStrict(w, r, maxBody, &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		// The seller must own an active registered instance whose observed
		// wallet still matches before its asks are accepted.
		obs, status, err := s.resolveSeller(r.Context(), claims.Subject)
		if err != nil {
			http.Error(w, "trusted region unavailable", status)
			return
		}

		// Validate the asks against the seller's own current advertisement.
		if err := s.validateAgainstObservation(req.Asks, obs.Services); err != nil {
			httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		revision, err := s.store.ReplaceAsks(r.Context(), claims.Subject, req.Asks, askTTL)
		if err != nil {
			status, msg := classifyStoreErr(err)
			httputil.WriteJSON(w, status, map[string]string{"error": msg})
			return
		}

		// The store assigns one fresh revision across all rows (0 when the
		// asks were cleared); echo it so the seller can detect a stale republish.
		httputil.WriteJSON(w, http.StatusOK, askResponse{
			Revision:  revision,
			ExpiresAt: s.now().UTC().Add(askTTL),
		})
	})
}

// authenticate extracts and verifies the pricing-scoped Bearer credential.
func (s *Service) authenticate(r *http.Request) (nodecred.Claims, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return nodecred.Claims{}, false
	}
	claims, err := s.verifier.Verify(r.Context(), strings.TrimSpace(h[len(prefix):]))
	if err != nil || claims.Audience != nodecred.PricingAudience {
		return nodecred.Claims{}, false
	}
	return claims, true
}

// resolveSeller loads the peer's instance and confirms it is a billable,
// payable provider — a registered instance with a credit account and an
// owner wallet whose live observation still matches — before its asks are
// accepted. This is the same predicate the billing gate uses to route
// (billing.BillableProvider + peers snapshot), so a peer can only publish
// prices for routes the market will honour. Trusted-region membership is
// NOT required: the marketplace is for permissionless providers, and
// requiring active membership would admit peers whose services are dropped
// from the routing snapshot while rejecting the billable ones the gate can
// actually settle.
func (s *Service) resolveSeller(ctx context.Context, peerID string) (mesh.PeerObservation, int, error) {
	inst, err := s.store.GetInstanceByPeerID(ctx, peerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return mesh.PeerObservation{}, http.StatusNotFound, err
		}
		return mesh.PeerObservation{}, http.StatusServiceUnavailable, err
	}
	if !billing.BillableProvider(inst.AccountID, inst.OwnerWallet) {
		return mesh.PeerObservation{}, http.StatusConflict, errors.New("seller not billable")
	}
	obs, err := s.mesh.LookupPeer(ctx, peerID)
	if err != nil {
		return mesh.PeerObservation{}, http.StatusServiceUnavailable, err
	}
	if !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
		return mesh.PeerObservation{}, http.StatusConflict, errors.New("observed wallet mismatch")
	}
	return obs, 0, nil
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}

// validateAgainstObservation rejects unknown services/models, duplicate
// (service, model) pairs, negative rates, and more than 256 entries. The
// store enforces the structural limits again; this validates the seller's own
// live advertisement so a peer cannot publish prices for routes it does not
// currently serve.
func (s *Service) validateAgainstObservation(asks []billing.Ask, services []mesh.ServiceObservation) error {
	if len(asks) > 256 {
		return errors.New("too many asks (max 256)")
	}
	known := knownModelsByService(services)
	seen := make(map[string]bool, len(asks))
	for _, a := range asks {
		if a.Service == "" || a.Model == "" {
			return errors.New("empty service or model")
		}
		key := a.Service + "\x00" + a.Model
		if seen[key] {
			return errors.New("duplicate ask: " + a.Service + "/" + a.Model)
		}
		seen[key] = true
		if a.InputPerMillion < 0 || a.CachedInputPerMillion < 0 || a.OutputPerMillion < 0 {
			return errors.New("negative rate")
		}
		models, ok := known[a.Service]
		if !ok || !models[a.Model] {
			return errors.New("unknown service or model: " + a.Service + "/" + a.Model)
		}
	}
	return nil
}

func knownModelsByService(services []mesh.ServiceObservation) map[string]map[string]bool {
	known := make(map[string]map[string]bool, len(services))
	for _, svc := range services {
		if svc.Name == "" {
			continue
		}
		models, ok := known[svc.Name]
		if !ok {
			models = make(map[string]bool)
			known[svc.Name] = models
		}
		for _, group := range svc.IdentityGroups {
			if len(group) <= len(modelPrefix) || group[:len(modelPrefix)] != modelPrefix {
				continue
			}
			models[group[len(modelPrefix):]] = true
		}
	}
	return known
}

const modelPrefix = "model="

func classifyStoreErr(err error) (int, string) {
	switch {
	case errors.Is(err, billing.ErrConflict):
		return http.StatusConflict, "invalid asks"
	default:
		return http.StatusServiceUnavailable, "service unavailable"
	}
}

// CatalogAllowlist adapts a function returning []AllowEntry into the
// Allowlist interface, so main.go can build the allowlist from the shared
// peers snapshot + catalog.Summarise without pricingapi depending on either.
type CatalogAllowlist func(ctx context.Context) ([]AllowEntry, error)

func (f CatalogAllowlist) Services(ctx context.Context) ([]AllowEntry, error) {
	return f(ctx)
}
