// Package settlement finalizes a billing reservation against the exact usage
// parsed from the proxied response. It runs inside the perf response hook (no
// second body parse): when the body stream ends, the perf package hands the
// parsed [perf.SettleUsage] to a [Settler.Callback], which settles or releases
// the reservation exactly once.
//
// A request is settled only when it carried a complete, authoritative usage
// record on a 2xx response. Every other outcome — non-2xx, client abort,
// missing/empty usage, or an unknown serving peer — releases the reservation
// without charging, so a reserved request can never be served free of charge
// nor double-charged. All spend is finalized inside the store's row-locked
// transaction; the settler never queues balances in memory.
package settlement

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/perf"
)

// Store is the minimal persistence contract settlement needs. It is satisfied
// by *store.Postgres; a subset keeps the package focused on finalize/release
// and lets tests inject an in-memory fake.
type Store interface {
	SettleBilling(ctx context.Context, requestID, servedPeerID string, usage billing.Usage, feeBps int, now time.Time) (billing.Request, error)
	ReleaseBilling(ctx context.Context, requestID, reason string, now time.Time) (billing.Request, error)
}

// nowFunc is the clock settlement observes; overridable in tests.
type nowFunc func() time.Time

// Settler finalizes reserved billing requests against the usage parsed by the
// perf hook. It is safe for concurrent use: one callback fires per proxied
// response, and each finalize is idempotent at the store level (UNIQUE(ref,
// leg) on the ledger; settled/released state is terminal).
type Settler struct {
	store  Store
	feeBps int
	now    nowFunc
	log    func(format string, args ...any)
}

// New returns a Settler that charges feeBps (basis points, 0–10000) of every
// settled cost to the treasury account. now may be nil (real time).
func New(store Store, feeBps int, now time.Time) *Settler {
	s := &Settler{
		store: store,
		now:   time.Now,
		log:   log.Printf,
	}
	s.SetFeeBps(feeBps)
	if !now.IsZero() {
		s.now = func() time.Time { return now }
	}
	return s
}

// SetFeeBps updates the routing fee in basis points. 0 disables the treasury
// leg. Out of range is clamped.
func (s *Settler) SetFeeBps(bps int) {
	if bps < 0 {
		bps = 0
	}
	if bps > 10000 {
		bps = 10000
	}
	s.feeBps = bps
}

// Callback is the perf.HookWithSettle sink. It resolves the reservation id
// stamped by the gate (billing.WithRequestID, carried through the proxy on the
// outbound request's context), then settles on a complete 2xx usage record or
// releases otherwise. Responses without a reservation id (not a billing-gated
// request, e.g. observe-mode forwards) are skipped.
//
// SettleBilling resolves the served peer against the immutable quote snapshot
// and releases (rather than charges) an unknown or zero-price peer, so the
// settler only needs to distinguish "chargeable" from "release".
func (s *Settler) Callback(resp *http.Response, u perf.SettleUsage) {
	if resp == nil || resp.Request == nil {
		return
	}
	requestID, ok := billing.RequestID(resp.Request.Context())
	if !ok || requestID == "" {
		// No reservation was made (BILLING_MODE=off, observe, or a
		// non-metered route). Nothing to finalize.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := s.now()
	switch {
	case !u.Complete:
		// Non-2xx, client abort, or no authoritative usage: release the
		// reservation without charging. The reason surfaces in the
		// billing_requests row and in reconciliation.
		reason := releaseReason(u)
		if _, err := s.store.ReleaseBilling(ctx, requestID, reason, now); err != nil && !errors.Is(err, billing.ErrNotFound) {
			s.log("settlement: release %s: %v", requestID, err)
		}
	default:
		usage := billing.Usage{
			// SettleUsage already normalizes the provider dialect into
			// billable regular input plus cached input.
			InputTokens:       u.InputTokens,
			CachedInputTokens: u.CachedInputTokens,
			OutputTokens:      u.OutputTokens,
		}
		if _, err := s.store.SettleBilling(ctx, requestID, u.ServedPeerID, usage, s.feeBps, now); err != nil && !errors.Is(err, billing.ErrNotFound) {
			s.log("settlement: settle %s: %v", requestID, err)
		}
	}
}

// releaseReason classifies a non-complete response for the billing_requests
// release_reason column and reconciliation.
func releaseReason(u perf.SettleUsage) string {
	switch {
	case u.ClientAbort:
		return "client_abort"
	case u.Status == 0:
		return "no_response"
	case u.Status >= 400:
		return "non_2xx"
	default:
		// 2xx but no usage: streaming without include_usage, or a usage-less
		// route the gate could not bound. Release rather than guess a charge.
		return "no_usage"
	}
}

// Sweeper runs the stale-reservation recovery loop. Reserved requests that
// outlive the configured lifetime (a sign the response hook never ran, e.g. a
// crashed proxy) are released with reason "swept_stale" using
// FOR UPDATE SKIP LOCKED, so multiple API replicas can run the loop
// concurrently without double-releasing or contention.
type Sweeper struct {
	store    SweeperStore
	interval time.Duration // how often to sweep
	age      time.Duration // a request is stale once reserved_at < now-age
	limit    int           // max releases per sweep
	now      func() time.Time
	log      func(format string, args ...any)
}

// SweeperStore is the minimal contract the recovery loop needs.
type SweeperStore interface {
	SweepStaleReservations(ctx context.Context, before time.Time, limit int) ([]string, error)
}

// NewSweeper returns a recovery worker. interval is the sweep cadence; age is
// how old a reserved request must be before it is reclaimed (set well above
// the max request lifetime so in-flight responses are not reclaimed). limit
// caps releases per sweep.
func NewSweeper(store SweeperStore, interval, age time.Duration, limit int) *Sweeper {
	if interval <= 0 {
		interval = time.Minute
	}
	if age <= 0 {
		age = 10 * time.Minute
	}
	switch {
	case limit <= 0:
		limit = 100
	case limit > 1000:
		limit = 1000
	}
	return &Sweeper{
		store: store, interval: interval, age: age, limit: limit,
		now: time.Now, log: log.Printf,
	}
}

// Run sweeps stale reservations until ctx is cancelled. It ticks on the
// configured interval and logs each sweep's count. Safe to run in multiple
// replicas.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := s.now().Add(-s.age)
			released, err := s.store.SweepStaleReservations(ctx, before, s.limit)
			if err != nil {
				s.log("settlement: sweep: %v", err)
				continue
			}
			if len(released) > 0 {
				s.log("settlement: swept %d stale reservations", len(released))
			}
		}
	}
}
