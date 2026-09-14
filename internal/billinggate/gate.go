// Package billinggate wraps the inference proxy with the pre-request market
// gate: it inspects the request (internal/gate), resolves the buyer's account
// and per-request caps, fetches the live peer snapshot (internal/peers) and
// live asks (peer_asks), intersects them with the requested model, filters to
// the affordable set, computes the conservative reserve, and persists the
// reservation — all before forwarding. The 128 cheapest affordable peers are
// stamped as X-Otela-Allowed-Peers so the mesh head never routes to a peer
// the buyer cannot afford.
//
// In observe mode the gate runs the same resolution and would-reserve/would-
// charge math but never rejects a request or moves a balance, so operators can
// verify cap enforcement without disrupting traffic. In enforce mode it is the
// production posture.
package billinggate

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/account"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/gate"
	"github.com/opentela-ai/api/internal/peers"
)

// maxAllowedPeers bounds the X-Otela-Allowed-Peers header so a large eligible
// set does not inflate request size.
const maxAllowedPeers = 128

// allowedPeersHeader is overwritten on every forwarded request; the mesh head
// intersects it with model/ACL/trust/health candidates before load balancing
// and on every retry.
const allowedPeersHeader = "X-Otela-Allowed-Peers"

// Service is the billing gate. It wraps an inner http.Handler (the proxy) and
// runs the gate on every metered inference route. A zero Store or zero Mode is
// a no-op wrapper (off).
type Service struct {
	store  BillingStore
	snap   *peers.Service
	mode   config.BillingMode
	output int // BILLING_OUTPUT_TOKEN_MAX; zero means no operator cap
	feeBps int
	// requirePricedPeer is the BILLING_REQUIRE_PRICED_PEER posture: when set,
	// a peer with no live ask is excluded from the affordable set for any
	// (service, model) that has at least one published ask, instead of being
	// eligible at a zero quote (free). Routes with no live asks keep the
	// original behavior so a cold market still boots.
	requirePricedPeer bool
	now               func() time.Time
	// getSnap is an injection point for tests; in production it is nil and the
	// gate uses s.snap.Snapshot.
	getSnap func(context.Context) (*peers.Snapshot, error)
}

// BillingStore is the subset of the store the gate needs. It is satisfied by
// *store.Postgres.
type BillingStore interface {
	EnsureAccountCredit(ctx context.Context, accountID string) error
	AccountCredit(ctx context.Context, accountID string) (billing.AccountCredit, error)
	SetAccountCaps(ctx context.Context, accountID string, caps billing.Caps) (billing.AccountCredit, error)
	LiveAsks(ctx context.Context, service, model string, now time.Time) ([]billing.Ask, error)
	ReserveBilling(ctx context.Context, req billing.Reservation) (billing.AccountCredit, error)
}

// New returns a Service. mode == BillingOff is a pass-through wrapper.
// requirePricedPeer toggles the BILLING_REQUIRE_PRICED_PEER posture (see the
// Service field comment).
func New(store BillingStore, snap *peers.Service, mode config.BillingMode, outputMax, feeBps int, requirePricedPeer bool) *Service {
	return &Service{
		store: store, snap: snap, mode: mode, output: outputMax,
		feeBps: feeBps, requirePricedPeer: requirePricedPeer, now: time.Now,
	}
}

// Middleware wraps inner with the billing gate. When mode is off, inner is
// returned unchanged so existing behavior is preserved byte-for-byte.
func (s *Service) Middleware(inner http.Handler) http.Handler {
	if s.mode == config.BillingOff {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, inner)
	})
}

// serve runs the gate for one request. It never silently serves a paid request
// free: when the route is unsupported or no affordable peer exists, it rejects
// with the appropriate status (observe mode records but still forwards).
func (s *Service) serve(w http.ResponseWriter, r *http.Request, inner http.Handler) {
	plan, req := gate.Inspect(r, gate.Options{OutputMax: s.output})

	// The buyer's account id was threaded by auth.Middleware. Without one the
	// gate cannot charge — reject in enforce, forward in observe.
	buyer, hasAccount := account.ID(r.Context())

	// Non-metered routes (e.g. GET /v1/service/{svc}/v1/models, /healthz) pass
	// through. The gate runs only on supported inference routes.
	if !plan.Supported {
		if s.mode == config.BillingEnforce && plan.Generative {
			// A generative route the gate cannot bound (no output ceiling) is
			// rejected before any GPU work happens.
			reject(w, r, http.StatusBadRequest, "billing_unsupported_route")
			return
		}
		// Observe mode or non-inference route: forward unchanged.
		inner.ServeHTTP(w, req)
		return
	}

	// Parse the per-request price caps up front: a negative or non-integer
	// value is a malformed request (400) in every mode, and zero is a valid
	// cap meaning "free peers only" (it must not be silently dropped).
	reqCaps, err := parseRequestCaps(req)
	if err != nil {
		reject(w, r, http.StatusBadRequest, "invalid_price_cap")
		return
	}

	// Resolve the buyer's effective caps (account defaults + per-request).
	caps, err := s.resolveCaps(r.Context(), buyer, hasAccount, reqCaps)
	if err != nil {
		if s.mode == config.BillingEnforce {
			reject(w, r, http.StatusPaymentRequired, "billing_account_required")
			return
		}
		inner.ServeHTTP(w, req)
		return
	}

	// Resolve affordable live peers for the requested model.
	quotes, err := s.resolveQuotes(r.Context(), plan, caps)
	if err != nil {
		if s.mode == config.BillingEnforce {
			// No affordable peer, or the market is unavailable.
			if errors.Is(err, billing.ErrPriceAboveMax) {
				reject(w, r, http.StatusPaymentRequired, "price_above_max")
			} else {
				reject(w, r, http.StatusServiceUnavailable, "billing_unavailable")
			}
			return
		}
		// Observe: forward the request unchanged.
		inner.ServeHTTP(w, req)
		return
	}

	// No billable provider serves this model. Enforce rejects with a
	// provider-availability response; observe forwards unchanged.
	if len(quotes) == 0 {
		if s.mode == config.BillingEnforce {
			reject(w, r, http.StatusServiceUnavailable, "no_provider")
			return
		}
		inner.ServeHTTP(w, req)
		return
	}

	// Reserve the conservative maximum cost. In observe mode the reservation
	// is computed but not persisted (balances never move).
	if s.mode == config.BillingEnforce {
		if !hasAccount {
			reject(w, r, http.StatusPaymentRequired, "billing_account_required")
			return
		}
		reqID, err := s.reserve(r.Context(), buyer, plan, caps, quotes)
		if err != nil {
			if errors.Is(err, billing.ErrInsufficientCredit) {
				reject(w, r, http.StatusPaymentRequired, "insufficient_credit")
			} else if errors.Is(err, billing.ErrConflict) {
				// Duplicate request id — the client retried with the same id.
				reject(w, r, http.StatusConflict, "billing_duplicate_request")
			} else {
				reject(w, r, http.StatusServiceUnavailable, "billing_unavailable")
			}
			return
		}
		// Stamp the reservation id so the response hook (Step 4) can settle or
		// release exactly once. The context travels with the request through the
		// proxy to the response.
		req = req.WithContext(billing.WithRequestID(req.Context(), reqID))
		setAllowedPeers(req, quotes)
	}

	inner.ServeHTTP(w, req)
}

// resolveCaps merges the buyer's stored account caps with any per-request
// header overrides (X-Max-Input-Per-Million, etc.) parsed up front.
func (s *Service) resolveCaps(ctx context.Context, buyer string, hasAccount bool, reqCaps billing.Caps) (billing.Caps, error) {
	if !hasAccount {
		return billing.Caps{}, billing.ErrBillingAccountRequired
	}
	// Ensure the row exists so a first-time buyer has account caps to read.
	if err := s.store.EnsureAccountCredit(ctx, buyer); err != nil {
		return billing.Caps{}, err
	}
	acct, err := s.store.AccountCredit(ctx, buyer)
	if err != nil {
		return billing.Caps{}, err
	}
	return billing.MergeCaps(acctCaps(acct), reqCaps), nil
}

func acctCaps(a billing.AccountCredit) billing.Caps {
	return billing.Caps{
		InputPerMillion:       a.MaxInputPerMillion,
		CachedInputPerMillion: a.MaxCachedInputPerMillion,
		OutputPerMillion:      a.MaxOutputPerMillion,
	}
}

// parseRequestCaps reads the three optional X-Max-*-Per-Million headers. A
// missing header leaves that dimension nil (unlimited for this request). Zero
// is a valid cap (free peers only); a negative or non-integer value is a
// malformed request (the caller rejects with 400).
func parseRequestCaps(r *http.Request) (billing.Caps, error) {
	var caps billing.Caps
	var err error
	if caps.InputPerMillion, err = parseCap(r, "X-Max-Input-Price-Per-Million"); err != nil {
		return billing.Caps{}, err
	}
	if caps.CachedInputPerMillion, err = parseCap(r, "X-Max-Cached-Input-Price-Per-Million"); err != nil {
		return billing.Caps{}, err
	}
	if caps.OutputPerMillion, err = parseCap(r, "X-Max-Output-Price-Per-Million"); err != nil {
		return billing.Caps{}, err
	}
	return caps, nil
}

// parseCap parses one price-cap header. An empty header is unlimited (nil).
// Zero is a valid cap (only peers priced at zero are affordable). A negative
// or non-integer value is an error.
func parseCap(r *http.Request, name string) (*int64, error) {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s: not an integer", name)
	}
	if n < 0 {
		return nil, fmt.Errorf("%s: negative cap", name)
	}
	return &n, nil
}

// resolveQuotes fetches the live peer snapshot and the live asks for the
// requested model, intersects them, and filters to the affordable set. Returns
// the cheapest-eligible quotes (already sorted by the store) capped at
// maxAllowedPeers. An empty result with at least one live ask means no peer is
// affordable → ErrPriceAboveMax.
func (s *Service) resolveQuotes(ctx context.Context, plan gate.Plan, caps billing.Caps) ([]billing.EligiblePeerQuote, error) {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	asks, err := s.store.LiveAsks(ctx, plan.Service, plan.Model, s.now().UTC())
	if err != nil {
		return nil, err
	}

	// Build a quote for each live peer. A peer with no published ask is
	// eligible at a zero quote (free to the buyer, nothing to the seller).
	askByPeer := make(map[string]billing.Ask, len(asks))
	for _, a := range asks {
		askByPeer[a.PeerID] = a
	}

	var quotes []billing.EligiblePeerQuote
	for peerID, entry := range snap.Entries {
		// The peer must advertise the requested service+model to be eligible.
		if !peerServesModel(entry.Peer, plan.Service, plan.Model) {
			continue
		}
		var sellerAccount, ownerWallet string
		if entry.Instance != nil {
			sellerAccount = entry.Instance.AccountID
			ownerWallet = entry.Instance.OwnerWallet
		}
		// Only billable, payable providers join the routing set — a peer the
		// buyer can never settle with is not a real option. This is the same
		// predicate the pricing API uses for publication, so the market view,
		// the routing constraint, and settlement all agree on who is a seller.
		if !billing.BillableProvider(sellerAccount, ownerWallet) {
			continue
		}
		a, priced := askByPeer[peerID]
		q := billing.EligiblePeerQuote{PeerID: peerID, SellerAccountID: sellerAccount, OwnerWallet: ownerWallet}
		if priced {
			q.Revision = a.Revision
			q.InputPerMillion = a.InputPerMillion
			q.CachedInputPerMillion = a.CachedInputPerMillion
			q.OutputPerMillion = a.OutputPerMillion
		}
		// An unpriced peer is eligible at a zero quote by default (no price,
		// the request is free). When the operator requires priced peers and
		// the market has priced this route at all, an unpublished peer is
		// excluded instead — a seller who never published (or whose asks
		// expired) must not silently serve free traffic once priced peers
		// exist. Routes with no live asks keep the original behavior so a
		// cold market still boots.
		if s.requirePricedPeer && !priced && len(asks) > 0 {
			continue
		}
		quotes = append(quotes, q)
	}

	// Filter to affordable: a peer is affordable only when all three rates are
	// within their corresponding caps.
	var affordable []billing.EligiblePeerQuote
	for _, q := range quotes {
		if billing.Affordable(q, caps) {
			affordable = append(affordable, q)
		}
	}

	// Sort cheapest input rate first (the store sorts LiveAsks this way; for
	// zero-quote peers and mixed sets we do it here too).
	sortQuotes(affordable)

	// If there were peers serving the model but none are affordable, the buyer's
	// max is below the market.
	if len(affordable) == 0 && len(quotes) > 0 {
		return nil, billing.ErrPriceAboveMax
	}
	if len(affordable) > maxAllowedPeers {
		affordable = affordable[:maxAllowedPeers]
	}
	return affordable, nil
}

// peerServesModel reports whether the peer advertises the (service, model)
// via its "model=" identity groups.
func peerServesModel(p peers.Peer, service, model string) bool {
	modelTag := "model=" + model
	for _, svc := range p.Service {
		if svc.Name != service {
			continue
		}
		for _, g := range svc.IdentityGroup {
			if g == modelTag {
				return true
			}
		}
	}
	return false
}

// snapshot returns the current policy-filtered peer table. In production it
// delegates to the shared peers.Service; tests inject getSnap.
func (s *Service) snapshot(ctx context.Context) (*peers.Snapshot, error) {
	if s.getSnap != nil {
		return s.getSnap(ctx)
	}
	return s.snap.Snapshot(ctx)
}

// reserve persists the conservative reservation under a row lock on the
// buyer's account_credits. It returns the request id so the caller can stamp
// it into the request context for the response hook (Step 4).
func (s *Service) reserve(ctx context.Context, buyer string, plan gate.Plan, caps billing.Caps, quotes []billing.EligiblePeerQuote) (string, error) {
	reqID, err := requestID()
	if err != nil {
		return "", err
	}
	_, err = s.store.ReserveBilling(ctx, billing.Reservation{
		RequestID:      reqID,
		BuyerAccountID: buyer,
		Service:        plan.Service,
		Model:          plan.Model,
		Caps:           caps,
		Quotes:         quotes,
		InputCeil:      plan.InputCeiling,
		OutputCeil:     plan.OutputCeiling,
	})
	return reqID, err
}

// setAllowedPeers stamps the cheapest affordable peer IDs on the forwarded
// request, overwriting any client-supplied value.
func setAllowedPeers(r *http.Request, quotes []billing.EligiblePeerQuote) {
	if len(quotes) == 0 {
		r.Header.Del(allowedPeersHeader)
		return
	}
	ids := make([]string, len(quotes))
	for i, q := range quotes {
		ids[i] = q.PeerID
	}
	r.Header.Set(allowedPeersHeader, strings.Join(ids, ","))
}

// reject writes a JSON error body with the billing error type.
func reject(w http.ResponseWriter, r *http.Request, status int, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":  typ,
		"method": r.Method,
		"path":   r.URL.Path,
	})
}

// requestID generates a short random id for the billing request.
func requestID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("billing gate: request id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sortQuotes sorts quotes cheapest input rate first, then by peer id for
// determinism.
func sortQuotes(q []billing.EligiblePeerQuote) {
	// Simple insertion sort — the list is capped at 128 and often tiny.
	for i := 1; i < len(q); i++ {
		for j := i; j > 0; j-- {
			if q[j].InputPerMillion < q[j-1].InputPerMillion ||
				(q[j].InputPerMillion == q[j-1].InputPerMillion && q[j].PeerID < q[j-1].PeerID) {
				q[j], q[j-1] = q[j-1], q[j]
			} else {
				break
			}
		}
	}
}
