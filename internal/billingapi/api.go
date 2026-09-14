// Package billingapi serves the per-account management endpoints that back
// the wallet page and the catalog market view (Step 6):
//
//   - GET    /manage/billing            — balance, caps, deposit instructions, mode
//   - PATCH  /manage/billing/preferences — set the three buyer caps
//   - GET    /manage/billing/ledger     — cursor-paginated immutable ledger
//   - GET    /manage/billing/deposits   — cursor-paginated credited deposits
//   - GET    /manage/billing/asks       — seller pricing surface: live asks and
//     unpriced advertised routes per owned instance
//
// The endpoints are JWT-authenticated by the surrounding manage router
// (principal.Middleware); the owning account id is the Neon subject, with an
// API-key fallback via account.ID. They never accept a balance, cap, or
// account id from the request body or the chain — only the three caps are
// client-set, and they are nullable (nil = unlimited on that axis).
package billingapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/account"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/httputil"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/pricingapi"
	"github.com/opentela-ai/api/internal/principal"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/store"
)

// billingStore is the subset of the billing store the management endpoints
// need. It is satisfied by *store.Postgres.
type billingStore interface {
	EnsureAccountCredit(ctx context.Context, accountID string) error
	AccountCredit(ctx context.Context, accountID string) (billing.AccountCredit, error)
	SetAccountCaps(ctx context.Context, accountID string, caps billing.Caps) (billing.AccountCredit, error)
	ListLedger(ctx context.Context, accountID string, cursor *billing.LedgerCursor, limit int) ([]billing.LedgerEntry, *billing.LedgerCursor, error)
	ListDepositEvents(ctx context.Context, accountID string, cursor *billing.DepositCursor, limit int) ([]billing.DepositEvent, *billing.DepositCursor, error)
	PrimaryWalletForAccount(ctx context.Context, accountID string) (string, bool, error)
	// Withdrawals (Step 7). ReserveWithdrawal debits available credit and
	// returns the durable record; the withdrawal worker (off the request
	// path) signs and broadcasts. Withdrawal/ListWithdrawals scope reads to
	// the owning account.
	ReserveWithdrawal(ctx context.Context, req billing.WithdrawalRequest, now time.Time) (billing.Withdrawal, error)
	Withdrawal(ctx context.Context, accountID string, id int64) (billing.Withdrawal, error)
	ListWithdrawals(ctx context.Context, accountID string, cursor *billing.WithdrawalCursor, limit int) ([]billing.Withdrawal, *billing.WithdrawalCursor, error)
	// Seller pricing surface (GET /manage/billing/asks): the account's own
	// registered instances and their live asks.
	ListInstancesByUser(ctx context.Context, accountID string) ([]store.InstanceInfo, error)
	LiveAsksByPeers(ctx context.Context, peerIDs []string, now time.Time) ([]billing.Ask, error)
}

// SellerMesh resolves a peer's live advertisement (served services/models)
// so the pricing panel can compute unpriced routes. Satisfied by *mesh.Client.
type SellerMesh interface {
	LookupPeer(ctx context.Context, peerID string) (mesh.PeerObservation, error)
}

// Service serves the billing management routes. The treasury ATA, mint,
// decimals, and token program are derived once from configuration and surfaced
// as deposit instructions so the wallet page can build a transfer to the
// treasury without trusting client-supplied values.
type Service struct {
	store              billingStore
	mode               config.BillingMode
	treasuryATA        string
	mint               string
	tokenProgram       string
	decimals           int
	withdrawalsEnabled bool
	mesh               SellerMesh
	ledgerLimit        int
	depositLimit       int
	now                func() time.Time
}

// New builds a Service. treasuryATA is the derived associated token account
// (solana.AssociatedTokenAddress) the deposit watcher polls; mint, decimals,
// and tokenProgram describe the OTELA token so the UI can render a valid
// transfer and a human-readable amount.
func New(store billingStore, mode config.BillingMode, treasuryATA, mint, tokenProgram string, decimals int, withdrawalsEnabled bool, meshClient SellerMesh) *Service {
	return &Service{
		store:              store,
		mode:               mode,
		treasuryATA:        treasuryATA,
		mint:               mint,
		tokenProgram:       tokenProgram,
		decimals:           decimals,
		withdrawalsEnabled: withdrawalsEnabled,
		mesh:               meshClient,
		ledgerLimit:        50,
		depositLimit:       50,
		now:                time.Now,
	}
}

// Routes returns the mounted billing management routes. The caller wraps this
// with the shared manage router's JWT/CORS middleware.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manage/billing", s.handleState)
	mux.HandleFunc("PATCH /manage/billing/preferences", s.handlePreferences)
	mux.HandleFunc("GET /manage/billing/ledger", s.handleLedger)
	mux.HandleFunc("GET /manage/billing/deposits", s.handleDeposits)
	mux.HandleFunc("GET /manage/billing/asks", s.handleAsks)
	mux.HandleFunc("POST /manage/billing/withdrawals", s.handleWithdrawalsCreate)
	mux.HandleFunc("GET /manage/billing/withdrawals", s.handleWithdrawalsList)
	mux.HandleFunc("GET /manage/billing/withdrawals/{id}", s.handleWithdrawal)
	return mux
}

// --- account resolution ---

// accountID resolves the owning account from the request: the Neon JWT subject
// (manage page) preferred, with the API-key owner (account.ID) as a fallback.
// ok is false for unauthenticated requests; the surrounding middleware rejects
// those before we get here, but the guard keeps the handlers defensive.
func (s *Service) accountID(r *http.Request) (string, bool) {
	if id, ok := principal.UserID(r.Context()); ok {
		return id, true
	}
	return account.ID(r.Context())
}

func (s *Service) requireAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := s.accountID(r)
	if !ok {
		http.Error(w, "billing account required", http.StatusUnauthorized)
		return "", false
	}
	return id, true
}

// --- responses ---

// nullableInt is rendered as an integer or JSON null (never omitted), so the
// UI can distinguish "no cap" (null) from "cap is zero" (free peers only).
type nullableInt struct {
	v *int64
}

func (n nullableInt) MarshalJSON() ([]byte, error) {
	if n.v == nil {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatInt(*n.v, 10)), nil
}

func nullableIntFrom(p *int64) nullableInt { return nullableInt{v: p} }

// nullableIntFromInt lifts an *int (token counts) into the same wire shape.
func nullableIntFromInt(p *int) nullableInt {
	if p == nil {
		return nullableInt{v: nil}
	}
	v := int64(*p)
	return nullableInt{v: &v}
}

// Int returns the wrapped value and whether one is present. Tests use it to
// avoid reaching into the unexported field.
func (n nullableInt) Int() (int64, bool) {
	if n.v == nil {
		return 0, false
	}
	return *n.v, true
}

// rawInt64 is the JSON boundary for token-denominated money. Raw monetary
// values are encoded as decimal strings so JavaScript clients never lose
// precision above Number.MAX_SAFE_INTEGER. The decoder also accepts legacy
// integer JSON numbers during rollout.
type rawInt64 int64

func (v rawInt64) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(strconv.FormatInt(int64(v), 10))), nil
}

func (v *rawInt64) UnmarshalJSON(data []byte) error {
	raw := string(data)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			return err
		}
		raw = unquoted
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return err
	}
	*v = rawInt64(parsed)
	return nil
}

// UnmarshalJSON accepts an integer or JSON null. A non-number is an error so
// malformed caps are rejected at the boundary rather than silently coerced.
func (n *nullableInt) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		n.v = nil
		return nil
	}
	v, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}
	n.v = &v
	return nil
}

// ledgerEntryJSON is the wire shape for one immutable balance movement.
// delta_raw is negative for consumption/withdrawal and positive for
// earnings/deposits/fees. price/token columns are present only when the
// movement carried them (usage/earn); a null field means "not applicable".
type ledgerEntryJSON struct {
	ID                    int64       `json:"id"`
	DeltaRaw              rawInt64    `json:"delta_raw"`
	Source                string      `json:"source"`
	Leg                   string      `json:"leg"`
	Counterparty          string      `json:"counterparty"`
	Ref                   string      `json:"ref"`
	Model                 string      `json:"model,omitempty"`
	InputPerMillion       nullableInt `json:"input_per_million"`
	CachedInputPerMillion nullableInt `json:"cached_input_per_million"`
	OutputPerMillion      nullableInt `json:"output_per_million"`
	InputTokens           nullableInt `json:"input_tokens"`
	CachedInputTokens     nullableInt `json:"cached_input_tokens"`
	OutputTokens          nullableInt `json:"output_tokens"`
	CreatedAt             time.Time   `json:"created_at"`
}

type ledgerPage struct {
	Entries []ledgerEntryJSON `json:"entries"`
	Next    string            `json:"next,omitempty"`
}

type depositEntryJSON struct {
	TransactionSignature string     `json:"transaction_signature"`
	InstructionIndex     int        `json:"instruction_index"`
	Slot                 int64      `json:"slot"`
	FromWallet           string     `json:"from_wallet"`
	AmountRaw            rawInt64   `json:"amount_raw"`
	AssignmentState      string     `json:"assignment_state"`
	CreditedAt           *time.Time `json:"credited_at,omitempty"`
	SeenAt               time.Time  `json:"seen_at"`
}

type depositPage struct {
	Entries []depositEntryJSON `json:"entries"`
	Next    string             `json:"next,omitempty"`
}

// stateResponse is the GET /manage/billing body. DepositInstructions is
// non-null only when billing is active and a treasury is configured, so the
// wallet page can decide whether to render the deposit panel.
type stateResponse struct {
	Mode                string       `json:"mode"`
	Balance             balanceJSON  `json:"balance"`
	Caps                capsJSON     `json:"caps"`
	Deposits            depositInstr `json:"deposits"`
	WithdrawalsEnabled  bool         `json:"withdrawals_enabled"`
	PrimaryLinkedWallet *string      `json:"primary_linked_wallet"`
}

type balanceJSON struct {
	CreditRaw   rawInt64 `json:"credit_raw"`
	ReservedRaw rawInt64 `json:"reserved_raw"`
	// AvailableRaw is credit_raw - reserved_raw, the spendable balance. The
	// same value is the combined withdrawable balance until withdrawals land
	// (Step 7); the UI surfaces it once.
	AvailableRaw rawInt64  `json:"available_raw"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type capsJSON struct {
	InputPerMillion       nullableInt `json:"input_per_million"`
	CachedInputPerMillion nullableInt `json:"cached_input_per_million"`
	OutputPerMillion      nullableInt `json:"output_per_million"`
}

type depositInstr struct {
	Enabled      bool   `json:"enabled"`
	TreasuryATA  string `json:"treasury_ata,omitempty"`
	Mint         string `json:"mint,omitempty"`
	TokenProgram string `json:"token_program,omitempty"`
	Decimals     int    `json:"decimals,omitempty"`
}

// --- handlers ---

func (s *Service) handleState(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	// Off mode: serve a zero-cost "off" payload without touching the billing
	// tables. The management route is always mounted (so the wallet page gets
	// a graceful state instead of a 404), but no balances are tracked and no
	// rows are written until billing is enabled with BILLING_MODE.
	if s.mode == config.BillingOff {
		httputil.WriteJSON(w, http.StatusOK, stateResponse{
			Mode:               string(s.mode),
			Balance:            balanceJSON{UpdatedAt: s.now()},
			Caps:               capsJSON{},
			Deposits:           depositInstr{Enabled: false},
			WithdrawalsEnabled: false,
		})
		return
	}
	if err := s.store.EnsureAccountCredit(r.Context(), accountID); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	ac, err := s.store.AccountCredit(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	primaryWallet, hasPrimaryWallet, err := s.store.PrimaryWalletForAccount(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	var primaryLinkedWallet *string
	if hasPrimaryWallet && primaryWallet != "" {
		primaryLinkedWallet = &primaryWallet
	}
	httputil.WriteJSON(w, http.StatusOK, stateResponse{
		Mode: string(s.mode),
		Balance: balanceJSON{
			CreditRaw:    rawInt64(ac.CreditRaw),
			ReservedRaw:  rawInt64(ac.ReservedRaw),
			AvailableRaw: rawInt64(ac.Available()),
			UpdatedAt:    ac.UpdatedAt,
		},
		Caps: capsJSON{
			InputPerMillion:       nullableIntFrom(ac.MaxInputPerMillion),
			CachedInputPerMillion: nullableIntFrom(ac.MaxCachedInputPerMillion),
			OutputPerMillion:      nullableIntFrom(ac.MaxOutputPerMillion),
		},
		Deposits: depositInstr{
			Enabled:      s.mode != config.BillingOff && s.treasuryATA != "",
			TreasuryATA:  s.treasuryATA,
			Mint:         s.mint,
			TokenProgram: s.tokenProgram,
			Decimals:     s.decimals,
		},
		WithdrawalsEnabled:  s.withdrawalsEnabled,
		PrimaryLinkedWallet: primaryLinkedWallet,
	})
}

type preferencesRequest struct {
	MaxInputPerMillion       *int64 `json:"max_input_per_million"`
	MaxCachedInputPerMillion *int64 `json:"max_cached_input_per_million"`
	MaxOutputPerMillion      *int64 `json:"max_output_per_million"`
}

// --- seller pricing surface (GET /manage/billing/asks) ---

// Seller-side constants mirroring the publication contract in pricingapi:
// the server assigns a 5-minute ask TTL and sellers are expected to
// republish roughly every two minutes. The response carries both so the
// console can render ask freshness without hardcoding them.
const (
	sellerAskTTLSeconds    = int((5 * time.Minute) / time.Second)
	sellerRepublishSeconds = int((2 * time.Minute) / time.Second)
	// maxPricedInstances bounds the per-request mesh lookups so a user with
	// a pathological number of registered instances cannot fan out unbounded.
	maxPricedInstances = 50
)

// publicationEndpoint is where asks are published out-of-band: a seller node
// calls it with a pricing-scoped node credential (full replacement, server
// TTL). The reference ask-publisher (cmd/askpublish) implements the loop.
const publicationEndpoint = "POST /internal/pricing"

// unpricedRoute is an advertised (service, model) pair the instance currently
// serves with no live ask. Under BILLING_REQUIRE_PRICED_PEER=true such a
// route is excluded from the priced market; under observe/false it is
// eligible at a zero quote — i.e. serving traffic for free.
type unpricedRoute struct {
	Service string `json:"service"`
	Model   string `json:"model"`
}

// instancePricing is one owned instance's seller pricing state.
type instancePricing struct {
	PeerID      string `json:"peer_id"`
	Label       string `json:"label"`
	OwnerWallet string `json:"owner_wallet,omitempty"`
	// Billable mirrors the gate's routing predicate (credit account + owner
	// wallet): false means the market will not credit this instance.
	Billable bool `json:"billable"`
	// Online/ObservedAt come from the live mesh observation;
	// AdvertisementKnown is false when the lookup failed (unpriced routes
	// cannot be computed without the advertisement).
	Online             bool            `json:"online"`
	ObservedAt         *time.Time      `json:"observed_at,omitempty"`
	AdvertisementKnown bool            `json:"advertisement_known"`
	Asks               []billing.Ask   `json:"asks"`
	UnpricedRoutes     []unpricedRoute `json:"unpriced_routes"`
}

// asksResponse is the GET /manage/billing/asks payload. Asks embed their
// revision and expires_at; freshness = expires_at - now, with republish
// every republish_seconds while the node is live.
type asksResponse struct {
	Now                 time.Time         `json:"now"`
	AskTTLSeconds       int               `json:"ask_ttl_seconds"`
	RepublishSeconds    int               `json:"republish_seconds"`
	PublicationEndpoint string            `json:"publication_endpoint"`
	Instances           []instancePricing `json:"instances"`
}

// handleAsks serves the seller pricing panel: per owned instance, the live
// asks (service, model, three rates, revision, expiry) and the advertised
// routes that currently have no ask (the "serving for free" view).
func (s *Service) handleAsks(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	resp := asksResponse{
		Now:                 s.now().UTC(),
		AskTTLSeconds:       sellerAskTTLSeconds,
		RepublishSeconds:    sellerRepublishSeconds,
		PublicationEndpoint: publicationEndpoint,
		Instances:           []instancePricing{},
	}
	if s.mode == config.BillingOff {
		// No market in off mode: an empty (but well-formed) payload so the
		// console renders a graceful "billing disabled" state.
		httputil.WriteJSON(w, http.StatusOK, resp)
		return
	}
	instances, err := s.store.ListInstancesByUser(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if len(instances) > maxPricedInstances {
		instances = instances[:maxPricedInstances]
	}
	peerIDs := make([]string, 0, len(instances))
	for _, in := range instances {
		peerIDs = append(peerIDs, in.PeerID)
	}
	asksByPeer := make(map[string][]billing.Ask, len(instances))
	if len(peerIDs) > 0 {
		asks, err := s.store.LiveAsksByPeers(r.Context(), peerIDs, s.now())
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, a := range asks {
			asksByPeer[a.PeerID] = append(asksByPeer[a.PeerID], a)
		}
	}
	for _, in := range instances {
		ip := instancePricing{
			PeerID:      in.PeerID,
			Label:       in.Label,
			OwnerWallet: in.OwnerWallet,
			Billable:    billing.BillableProvider(in.AccountID, in.OwnerWallet),
			Asks:        asksByPeer[in.PeerID],
		}
		if ip.Asks == nil {
			ip.Asks = []billing.Ask{}
		}
		ip.UnpricedRoutes = []unpricedRoute{}
		if s.mesh != nil {
			if obs, err := s.mesh.LookupPeer(r.Context(), in.PeerID); err == nil {
				ip.Online = obs.Online
				observedAt := obs.ObservedAt
				ip.ObservedAt = &observedAt
				ip.AdvertisementKnown = true
				ip.UnpricedRoutes = unpricedRoutes(obs.Services, ip.Asks)
			}
		}
		resp.Instances = append(resp.Instances, ip)
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// unpricedRoutes computes the advertised (service, model) pairs that have no
// live ask, sorted for stable display. Mirrors pricingapi's publication
// validation: only specific model= groups count as routable pairs (wildcards
// and catch-alls are not priceable identities).
func unpricedRoutes(services []mesh.ServiceObservation, asks []billing.Ask) []unpricedRoute {
	asked := make(map[string]bool, len(asks))
	for _, a := range asks {
		asked[a.Service+"\x00"+a.Model] = true
	}
	var out []unpricedRoute
	for svc, models := range pricingapi.AdvertisedModelsByService(services) {
		for model := range models {
			if !asked[svc+"\x00"+model] {
				out = append(out, unpricedRoute{Service: svc, Model: model})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func (s *Service) handlePreferences(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	var req preferencesRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := validateCaps(req.MaxInputPerMillion, req.MaxCachedInputPerMillion, req.MaxOutputPerMillion); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.EnsureAccountCredit(r.Context(), accountID); err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	ac, err := s.store.SetAccountCaps(r.Context(), accountID, billing.Caps{
		InputPerMillion:       req.MaxInputPerMillion,
		CachedInputPerMillion: req.MaxCachedInputPerMillion,
		OutputPerMillion:      req.MaxOutputPerMillion,
	})
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, capsJSON{
		InputPerMillion:       nullableIntFrom(ac.MaxInputPerMillion),
		CachedInputPerMillion: nullableIntFrom(ac.MaxCachedInputPerMillion),
		OutputPerMillion:      nullableIntFrom(ac.MaxOutputPerMillion),
	})
}

func (s *Service) handleLedger(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), s.ledgerLimit)
	cursor, err := decodeLedgerCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	entries, next, err := s.store.ListLedger(r.Context(), accountID, cursor, limit)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	out := ledgerPage{Entries: make([]ledgerEntryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, ledgerEntryJSON{
			ID:                    e.ID,
			DeltaRaw:              rawInt64(e.DeltaRaw),
			Source:                e.Source,
			Leg:                   e.Leg,
			Counterparty:          e.Counterparty,
			Ref:                   e.Ref,
			Model:                 e.Model,
			InputPerMillion:       nullableIntFrom(e.InputPerMillion),
			CachedInputPerMillion: nullableIntFrom(e.CachedInputPerMillion),
			OutputPerMillion:      nullableIntFrom(e.OutputPerMillion),
			InputTokens:           nullableIntFromInt(e.InputTokens),
			CachedInputTokens:     nullableIntFromInt(e.CachedInputTokens),
			OutputTokens:          nullableIntFromInt(e.OutputTokens),
			CreatedAt:             e.CreatedAt,
		})
	}
	if next != nil {
		out.Next = encodeLedgerCursor(*next)
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handleDeposits(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), s.depositLimit)
	cursor, err := decodeDepositCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	entries, next, err := s.store.ListDepositEvents(r.Context(), accountID, cursor, limit)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	out := depositPage{Entries: make([]depositEntryJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, depositEntryJSON{
			TransactionSignature: e.TransactionSignature,
			InstructionIndex:     e.InstructionIndex,
			Slot:                 e.Slot,
			FromWallet:           e.FromWallet,
			AmountRaw:            rawInt64(e.AmountRaw),
			AssignmentState:      e.AssignmentState,
			CreditedAt:           e.CreditedAt,
			SeenAt:               e.SeenAt,
		})
	}
	if next != nil {
		out.Next = encodeDepositCursor(*next)
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// --- withdrawals (Step 7) ---

// withdrawalCreateRequest is the POST /manage/billing/withdrawals body. The
// destination is resolved server-side from the seller's primary linked wallet.
// idempotency_key in the body is retained only as a compatibility fallback;
// the design contract is the Idempotency-Key header.
type withdrawalCreateRequest struct {
	DestinationWallet string   `json:"destination_wallet"`
	AmountRaw         rawInt64 `json:"amount_raw"`
	IdempotencyKey    string   `json:"idempotency_key"`
}

// withdrawalJSON is the wire shape for one withdrawal record. The signed
// wire is intentionally omitted: it is an internal implementation detail and
// carries a transaction the operator signed, not the account holder.
type withdrawalJSON struct {
	ID                 int64      `json:"id"`
	DestinationWallet  string     `json:"destination_wallet"`
	AmountRaw          rawInt64   `json:"amount_raw"`
	State              string     `json:"state"`
	Signature          string     `json:"signature,omitempty"`
	Error              string     `json:"error,omitempty"`
	ReservedAt         time.Time  `json:"reserved_at"`
	BlockhashExpiresAt *time.Time `json:"blockhash_expires_at,omitempty"`
	SignedAt           *time.Time `json:"signed_at,omitempty"`
	BroadcastAt        *time.Time `json:"broadcast_at,omitempty"`
	FinalizedAt        *time.Time `json:"finalized_at,omitempty"`
}

type withdrawalPage struct {
	Entries []withdrawalJSON `json:"entries"`
	Next    string           `json:"next,omitempty"`
}

func withdrawalToJSON(w billing.Withdrawal) withdrawalJSON {
	return withdrawalJSON{
		ID:                 w.ID,
		DestinationWallet:  w.DestinationWallet,
		AmountRaw:          rawInt64(w.AmountRaw),
		State:              string(w.State),
		Signature:          w.Signature,
		Error:              w.Error,
		ReservedAt:         w.ReservedAt,
		BlockhashExpiresAt: w.BlockhashExpiresAt,
		SignedAt:           w.SignedAt,
		BroadcastAt:        w.BroadcastAt,
		FinalizedAt:        w.FinalizedAt,
	}
}

// validateDestinationWallet mirrors the deposit/faucet base58 shape: 32 bytes
// when decoded. A bad destination is rejected at the boundary so the worker
// never has to restore credit for a typo.
func validateDestinationWallet(s string) bool {
	b, err := solana.DecodeBase58(s, solana.PublicKeyBytes)
	return err == nil && len(b) == solana.PublicKeyBytes
}

func withdrawalIdempotencyKey(r *http.Request, body string) (string, error) {
	header := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	body = strings.TrimSpace(body)
	switch {
	case header != "" && body != "" && header != body:
		return "", errors.New("Idempotency-Key header does not match idempotency_key body")
	case header != "":
		return header, nil
	default:
		return body, nil
	}
}

// handleWithdrawalsCreate reserves a withdrawal: it debits available credit
// and queues the durable record for the off-request withdrawal worker. The
// response is 202 Accepted (the transfer has not landed yet); idempotent
// re-posts return the original record with the same status.
func (s *Service) handleWithdrawalsCreate(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	var req withdrawalCreateRequest
	if err := httputil.DecodeStrict(w, r, maxBodyBytes, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	idempotencyKey, err := withdrawalIdempotencyKey(r, req.IdempotencyKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if idempotencyKey == "" {
		http.Error(w, "idempotency_key is required", http.StatusBadRequest)
		return
	}
	if !s.withdrawalsEnabled {
		http.Error(w, "withdrawals are disabled", http.StatusServiceUnavailable)
		return
	}
	destinationWallet, ok, err := s.store.PrimaryWalletForAccount(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok || destinationWallet == "" {
		http.Error(w, "primary linked wallet required", http.StatusConflict)
		return
	}
	if !validateDestinationWallet(destinationWallet) {
		http.Error(w, "primary linked wallet is not a valid 32-byte base58 pubkey", http.StatusConflict)
		return
	}
	if req.DestinationWallet != "" && req.DestinationWallet != destinationWallet {
		http.Error(w, "destination_wallet must match the primary linked wallet", http.StatusConflict)
		return
	}
	if req.AmountRaw <= 0 {
		http.Error(w, "amount_raw must be positive", http.StatusBadRequest)
		return
	}
	// Available-credit guard: the store re-checks atomically, but a cheap
	// pre-check here returns a clean 402 without a write transaction.
	ac, err := s.store.AccountCredit(r.Context(), accountID)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if ac.Available() < int64(req.AmountRaw) {
		http.Error(w, "insufficient available credit", http.StatusPaymentRequired)
		return
	}
	wd, err := s.store.ReserveWithdrawal(r.Context(), billing.WithdrawalRequest{
		AccountID:         accountID,
		IdempotencyKey:    idempotencyKey,
		DestinationWallet: destinationWallet,
		AmountRaw:         int64(req.AmountRaw),
	}, s.now())
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrInsufficientCredit):
			http.Error(w, "insufficient available credit", http.StatusPaymentRequired)
		case errors.Is(err, billing.ErrConflict):
			http.Error(w, "idempotency key in use", http.StatusConflict)
		case errors.Is(err, billing.ErrInvalid):
			http.Error(w, "invalid withdrawal request", http.StatusBadRequest)
		default:
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, withdrawalToJSON(wd))
}

// handleWithdrawal returns one withdrawal by id, scoped to the owning
// account: a row that does not exist OR belongs to a different account is a
// 404, so a leaked id cannot probe another user's state.
func (s *Service) handleWithdrawal(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid withdrawal id", http.StatusBadRequest)
		return
	}
	wd, err := s.store.Withdrawal(r.Context(), accountID, id)
	if err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			http.Error(w, "withdrawal not found", http.StatusNotFound)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, withdrawalToJSON(wd))
}

// handleWithdrawalsList returns the account's withdrawals newest-first, with
// the same opaque-base64 cursor pattern as the ledger/deposit endpoints.
func (s *Service) handleWithdrawalsList(w http.ResponseWriter, r *http.Request) {
	accountID, ok := s.requireAccount(w, r)
	if !ok {
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), s.depositLimit)
	cursor, err := decodeWithdrawalCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	entries, next, err := s.store.ListWithdrawals(r.Context(), accountID, cursor, limit)
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	out := withdrawalPage{Entries: make([]withdrawalJSON, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, withdrawalToJSON(e))
	}
	if next != nil {
		out.Next = encodeWithdrawalCursor(*next)
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// --- helpers ---

const maxBodyBytes = 4 << 10

// validateCaps rejects negative values and values that would overflow the
// checked billing math. nil means "clear this dimension to unlimited".
func validateCaps(input, cached, output *int64) error {
	const max = 1 << 62 // comfortably below MaxInt64; rates are per-1M tokens
	for _, p := range []*int64{input, cached, output} {
		if p == nil {
			continue
		}
		if *p < 0 {
			return errors.New("caps must not be negative")
		}
		if *p > max {
			return errors.New("caps must not exceed " + strconv.FormatInt(max, 10))
		}
	}
	return nil
}

func clampLimit(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > 200 {
		return 200
	}
	return n
}

// encodeLedgerCursor base64-encodes the (created_at, id) cursor so it is
// opaque to clients and survives URL round-trips without a JSON parse.
func encodeLedgerCursor(c billing.LedgerCursor) string {
	ts := c.CreatedAt.UTC().UnixNano()
	id := c.ID
	// 8 bytes unix nanos, 8 bytes id, big-endian.
	buf := make([]byte, 16)
	for i := 0; i < 8; i++ {
		buf[i] = byte(ts >> (56 - 8*i))
		buf[8+i] = byte(id >> (56 - 8*i))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func decodeLedgerCursor(raw string) (*billing.LedgerCursor, error) {
	if raw == "" {
		return nil, nil
	}
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(buf) != 16 {
		return nil, errors.New("malformed ledger cursor")
	}
	var ts, id int64
	for i := 0; i < 8; i++ {
		ts = ts<<8 | int64(buf[i])
		id = id<<8 | int64(buf[8+i])
	}
	return &billing.LedgerCursor{CreatedAt: time.Unix(0, ts).UTC(), ID: id}, nil
}

func encodeDepositCursor(c billing.DepositCursor) string {
	ts := c.SeenAt.UTC().UnixNano()
	buf := make([]byte, 8)
	for i := 0; i < 8; i++ {
		buf[i] = byte(ts >> (56 - 8*i))
	}
	// Prefix the timestamp with the signature + index so the cursor is stable
	// across re-pagination: created_at ties, the (signature, index) breaks them.
	prefix := []byte(c.TransactionSignature)
	out := make([]byte, 0, len(prefix)+1+8+4)
	out = append(out, prefix...)
	out = append(out, 0)
	out = append(out, buf...)
	idx := uint32(c.InstructionIndex)
	out = append(out, byte(idx>>24), byte(idx>>16), byte(idx>>8), byte(idx))
	return base64.RawURLEncoding.EncodeToString(out)
}

func decodeDepositCursor(raw string) (*billing.DepositCursor, error) {
	if raw == "" {
		return nil, nil
	}
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(buf) < 13 {
		return nil, errors.New("malformed deposit cursor")
	}
	sep := -1
	for i, b := range buf {
		if b == 0 {
			sep = i
			break
		}
	}
	if sep < 0 || len(buf) < sep+1+12 {
		return nil, errors.New("malformed deposit cursor")
	}
	sig := string(buf[:sep])
	var ts int64
	for i := 0; i < 8; i++ {
		ts = ts<<8 | int64(buf[sep+1+i])
	}
	idx := uint32(buf[sep+9])<<24 | uint32(buf[sep+10])<<16 | uint32(buf[sep+11])<<8 | uint32(buf[sep+12])
	return &billing.DepositCursor{
		SeenAt:               time.Unix(0, ts).UTC(),
		TransactionSignature: sig,
		InstructionIndex:     int(idx),
	}, nil
}

// encodeWithdrawalCursor base64-encodes the (reserved_at, id) cursor so it is
// opaque to clients and survives URL round-trips. The shape is identical to
// the ledger cursor (8 bytes unix nanos + 8 bytes id, big-endian).
func encodeWithdrawalCursor(c billing.WithdrawalCursor) string {
	ts := c.ReservedAt.UTC().UnixNano()
	id := c.ID
	buf := make([]byte, 16)
	for i := 0; i < 8; i++ {
		buf[i] = byte(ts >> (56 - 8*i))
		buf[8+i] = byte(id >> (56 - 8*i))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func decodeWithdrawalCursor(raw string) (*billing.WithdrawalCursor, error) {
	if raw == "" {
		return nil, nil
	}
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(buf) != 16 {
		return nil, errors.New("malformed withdrawal cursor")
	}
	var ts, id int64
	for i := 0; i < 8; i++ {
		ts = ts<<8 | int64(buf[i])
		id = id<<8 | int64(buf[8+i])
	}
	return &billing.WithdrawalCursor{ReservedAt: time.Unix(0, ts).UTC(), ID: id}, nil
}
