// Package askrefresh republishes the seller's durable ask config
// (peer_ask_config, console-editable) into the TTL'd market table
// (peer_asks) while the peer's live mesh observation matches the owner.
//
// A seller who manages prices from the console runs no node-side publisher:
// this loop IS their republish cadence. Liveness is not assumed — each
// cycle applies the same predicate as POST /internal/pricing and the billing
// gate (registered instance, BillableProvider, fresh observation, matching
// owner wallet), so a stale config can only keep market rows alive for a
// peer the gate would actually settle with.
package askrefresh

import (
	"context"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

const (
	// DefaultInterval is the republish cadence — half the ask TTL, matching
	// the reference node-side publisher (cmd/askpublish).
	DefaultInterval = 2 * time.Minute
	// DefaultTTL is the server-assigned market expiry for refreshed asks.
	DefaultTTL = 5 * time.Minute
)

type storeIface interface {
	AllAskConfigs(ctx context.Context) (map[string][]billing.Ask, error)
	GetInstanceByPeerID(ctx context.Context, peerID string) (store.InstanceInfo, error)
	ReplaceAsks(ctx context.Context, peerID string, asks []billing.Ask, ttl time.Duration) (int64, error)
}

type meshIface interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

// Service periodically republishes console-configured asks.
type Service struct {
	store storeIface
	mesh  meshIface
	logf  func(format string, args ...any)
	now   func() time.Time
	// interval is the republish cadence; ttl the market expiry written.
	interval time.Duration
	ttl      time.Duration
	// ownershipMaxAge bounds how old an observation may be while still
	// counting as "the live mesh sees the right owner"; zero disables the
	// age check (matching pricingapi's constructor surface).
	ownershipMaxAge time.Duration
}

// New returns a refresher. ttl/interval <= 0 fall back to the defaults.
func New(store storeIface, meshClient meshIface, logf func(string, ...any)) *Service {
	return &Service{
		store:    store,
		mesh:     meshClient,
		logf:     logf,
		now:      time.Now,
		interval: DefaultInterval,
		ttl:      DefaultTTL,
	}
}

// SetOwnershipMaxAge bounds observation freshness for the ownership check.
func (s *Service) SetOwnershipMaxAge(d time.Duration) { s.ownershipMaxAge = d }

// Run blocks until ctx is cancelled, refreshing on every tick.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshOnce(ctx)
		}
	}
}

// refreshOnce republishes every peer's configured asks whose live mesh
// observation matches the owner wallet. Failures are logged and skipped —
// the next cycle retries; market rows for skipped peers simply expire,
// which is the correct failure mode (no ask, no traffic).
func (s *Service) refreshOnce(ctx context.Context) {
	configs, err := s.store.AllAskConfigs(ctx)
	if err != nil {
		if s.logf != nil {
			s.logf("askrefresh: load configs: %v", err)
		}
		return
	}
	for peerID, asks := range configs {
		if len(asks) == 0 {
			continue
		}
		inst, err := s.store.GetInstanceByPeerID(ctx, peerID)
		if err != nil {
			continue // unregistered (deleted instance): nothing to publish
		}
		if !billing.BillableProvider(inst.AccountID, inst.OwnerWallet) {
			continue
		}
		obs, err := s.mesh.LookupPeer(ctx, peerID)
		if err != nil || !s.observationFresh(obs) || obs.Wallet != inst.OwnerWallet {
			continue // not live or ownership changed: let market rows expire
		}
		if _, err := s.store.ReplaceAsks(ctx, peerID, asks, s.ttl); err != nil {
			if s.logf != nil {
				s.logf("askrefresh: republish %s: %v", peerID, err)
			}
		}
	}
}

func (s *Service) observationFresh(obs mesh.PeerObservation) bool {
	if s.ownershipMaxAge <= 0 {
		return true
	}
	return s.now().UTC().Sub(obs.ObservedAt) <= s.ownershipMaxAge
}
