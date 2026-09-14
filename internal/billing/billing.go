// Package billing defines the OTELA market and settlement contract: the
// three-tier peer ask, buyer caps, conservative reservation sizing, exact
// integer cost math, and the durable reservation/settlement store interface.
//
// The store implementation lives in internal/store; this package holds the
// pure, database-free types so the HTTP gate, the metering hook, and tests can
// reason about a charge without importing a database driver. All spend is
// authorized and finalized inside row-locked transactions — cached reads may
// accelerate asks and catalog views but must never authorize or move a balance.
package billing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// requestIDKey carries the billing request id (the reservation primary key)
// from the gate, which reserves before forwarding, to the response hook, which
// settles exactly once on a 2xx response. It is deliberately a billing-scoped
// value so a Step 4 hook in another package can read it without importing the
// gate.
type requestIDKey struct{}

// WithRequestID returns a context carrying the billing request id. The gate
// calls this after a successful reservation so settlement can find the row.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID returns the billing request id stamped by the gate, if any. The
// response hook uses it to settle or release the reservation.
func RequestID(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(requestIDKey{}).(string)
	return v, ok
}

// Million is the token denominator for every rate (cost is per 1,000,000 tokens).
const Million int64 = 1_000_000

// MaxBaseRate bounds a single per-1M rate. It mirrors peer_asks_rates_bound_chk
// so that a rate multiplied by any realistic token count cannot overflow a
// signed int64 before the cost is divided back down.
const MaxBaseRate int64 = 1_000_000_000_000 // 1e12 base units / 1M tokens (~1e6 OTELA / 1M)

// Ask is one peer's published price for a (service, model) in OTELA base units
// (9 decimals) per 1,000,000 tokens. All three rates are non-negative and
// bounded by MaxBaseRate; a peer that has never published is eligible at a
// zero quote (matching the original design: no price, the request is free).
type Ask struct {
	PeerID                string    `json:"peer_id"`
	Service               string    `json:"service"`
	Model                 string    `json:"model"`
	InputPerMillion       int64     `json:"input_per_million"`
	CachedInputPerMillion int64     `json:"cached_input_per_million"`
	OutputPerMillion      int64     `json:"output_per_million"`
	Revision              int64     `json:"revision"`
	ExpiresAt             time.Time `json:"expires_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// Caps are a buyer's per-1M-token ceilings. A nil dimension means unlimited on
// that axis; a peer is affordable only when every non-nil cap is greater than
// or equal to the peer's corresponding rate.
type Caps struct {
	InputPerMillion       *int64
	CachedInputPerMillion *int64
	OutputPerMillion      *int64
}

// empty reports whether the buyer set no limit at all.
func (c Caps) empty() bool {
	return c.InputPerMillion == nil && c.CachedInputPerMillion == nil && c.OutputPerMillion == nil
}

// BillableProvider reports whether a seller can be credited for routed
// inference: it carries a credit account (so the buyer's charge and the
// seller's earnings both post) and an owner wallet (so earnings are
// withdrawable). Both the pricing publication endpoint and the billing gate
// use this one predicate so a peer can only publish prices for routes the
// market will actually honour — a registered instance without a wallet, or a
// wallet without an account, is neither a billable seller nor a routable one.
func BillableProvider(sellerAccountID, ownerWallet string) bool {
	return sellerAccountID != "" && ownerWallet != ""
}

// Affordable reports whether quote's three rates are within caps on every
// non-nil dimension. Empty caps (all nil) accept any quote, including unpriced.
func Affordable(q EligiblePeerQuote, caps Caps) bool {
	if caps.InputPerMillion != nil && q.InputPerMillion > *caps.InputPerMillion {
		return false
	}
	if caps.CachedInputPerMillion != nil && q.CachedInputPerMillion > *caps.CachedInputPerMillion {
		return false
	}
	if caps.OutputPerMillion != nil && q.OutputPerMillion > *caps.OutputPerMillion {
		return false
	}
	return true
}

// AccountCredit is the transactional projection of one account's balance.
// credit_raw is the total held; reserved_raw is the sum of in-flight request
// reservations and is a part of (not in addition to) credit_raw, so the
// spendable balance is always credit_raw - reserved_raw with the invariant
// 0 <= reserved_raw <= credit_raw.
type AccountCredit struct {
	AccountID                string
	CreditRaw                int64
	ReservedRaw              int64
	MaxInputPerMillion       *int64
	MaxCachedInputPerMillion *int64
	MaxOutputPerMillion      *int64
	UpdatedAt                time.Time
}

// Available returns the spendable balance: credit not reserved.
func (c AccountCredit) Available() int64 { return c.CreditRaw - c.ReservedRaw }

// Allowance is one mirrored SPL delegation (design §11.2): the buyer's
// on-chain approve naming `delegate` (the settlement authority) as spender of
// their OTELA ATA, up to AllowanceRaw. RevokedAt is nil while the delegation
// is live; a drop to zero sets it, and a later grant clears it.
type Allowance struct {
	AccountID    string
	Delegate     string
	AllowanceRaw int64
	ApprovedAt   time.Time
	RevokedAt    *time.Time
}

// Active reports whether the delegation currently backs spend.
func (a Allowance) Active() bool { return a.RevokedAt == nil && a.AllowanceRaw > 0 }

// AllowanceChange is an observed absolute allowance for (account, delegate),
// as read from the chain by the allowance poller. Ref identifies the on-chain
// observation (the approve/revoke transaction signature or slot) and becomes
// the exactly-once ledger leg ref for the credit mirror.
type AllowanceChange struct {
	AccountID    string
	Delegate     string
	AllowanceRaw int64
	ObservedAt   time.Time
	Ref          string
}

// AllowanceChangeResult reports what UpsertAllowance applied.
type AllowanceChangeResult struct {
	// PrevAllowanceRaw is the registry value before the change (0 for a
	// first observation).
	PrevAllowanceRaw int64
	// AppliedRaw is the signed credit delta mirrored into the account
	// (positive grant, negative revocation), 0 when the observation was an
	// idempotent replay.
	AppliedRaw int64
	// ShortfallRaw is the non-negative amount by which a revocation could
	// not be debited because open reservations must keep their backing
	// (credit_raw is clamped at reserved_raw). It is platform exposure
	// until the open reservations settle and surfaces via ReconcileAccount.
	ShortfallRaw int64
}

// DelegationAvailable is the conservative gate bound of §11.3: with no active
// delegation the deposit rail alone backs spend; with one or more active
// allowances the new reservation is bounded by min(credit available,
// Σ allowance), so a stale or inflated credit projection can never overspend
// the on-chain delegation. Both inputs are non-negative, so no overflow
// checks are needed for the min.
func DelegationAvailable(credit AccountCredit, allowances []Allowance) int64 {
	sum := int64(0)
	for _, a := range allowances {
		if a.Active() {
			sum += a.AllowanceRaw
		}
	}
	avail := credit.Available()
	if avail < 0 {
		return 0
	}
	if sum > 0 && sum < avail {
		return sum
	}
	return avail
}

// EligiblePeerQuote is one entry in a request's immutable quote snapshot. The
// seller account and owner wallet are resolved from instances at the gate; the
// ask revision and three rates are read live and frozen here so settlement
// never re-prices at response completion. An unpriced peer is represented with
// all three rates zero.
type EligiblePeerQuote struct {
	PeerID                string `json:"peer_id"`
	SellerAccountID       string `json:"seller_account_id"`
	OwnerWallet           string `json:"owner_wallet"`
	Revision              int64  `json:"revision"`
	InputPerMillion       int64  `json:"input_per_million"`
	CachedInputPerMillion int64  `json:"cached_input_per_million"`
	OutputPerMillion      int64  `json:"output_per_million"`
}

// Usage is the parsed token accounting for one billed response. Regular input
// is OpenAI aggregate input minus cached tokens (cache creation stays at the
// regular rate); for Anthropic, regular input is input_tokens plus
// cache_creation_input_tokens and cached input is cache_read_input_tokens.
type Usage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// Reservation is what the gate hands the store after it has resolved the
// affordable live peers and the request's conservative token ceilings. The
// store computes the reserve amount from quotes, checks available credit under
// a row lock, bumps reserved_raw, and persists the request with its immutable
// quote snapshot.
type Reservation struct {
	RequestID      string
	BuyerAccountID string
	Service        string
	Model          string
	Caps           Caps // effective caps at the gate (account defaults + per-request overrides)
	Quotes         []EligiblePeerQuote
	InputCeil      int       // conservative total input-token ceiling (e.g. request body bytes)
	OutputCeil     int       // configured/injected output-token maximum for generative routes
	ReservedAt     time.Time // optional; now() when zero
}

// RequestState is the durable billing-request state machine.
type RequestState string

const (
	StateReserved RequestState = "reserved"
	StateSettled  RequestState = "settled"
	StateReleased RequestState = "released"
)

// Request is the persisted reservation/settlement record.
type Request struct {
	RequestID                   string
	BuyerAccountID              string
	Service                     string
	Model                       string
	Quotes                      []EligiblePeerQuote
	Caps                        Caps
	ReservedRaw                 int64
	State                       RequestState
	ServedPeerID                string
	ServedSellerAccountID       string
	ServedRevision              int64
	ServedInputPerMillion       int64
	ServedCachedInputPerMillion int64
	ServedOutputPerMillion      int64
	InputTokens                 int
	CachedInputTokens           int
	OutputTokens                int
	CostRaw                     int64
	FeeRaw                      int64
	SellerRaw                   int64
	ReservedAt                  time.Time
	SettledAt                   *time.Time
	ReleasedAt                  *time.Time
	ReleaseReason               string
}

// LedgerEntry is one immutable balance movement. delta_raw is negative for
// consumption/withdrawal and positive for earnings/deposits/fees.
type LedgerEntry struct {
	ID                    int64
	AccountID             string
	DeltaRaw              int64
	Source                string
	Leg                   string
	Counterparty          string
	Ref                   string
	Model                 string
	InputPerMillion       *int64
	CachedInputPerMillion *int64
	OutputPerMillion      *int64
	InputTokens           *int
	CachedInputTokens     *int
	OutputTokens          *int
	CreatedAt             time.Time
}

// LedgerCursor is the stable, opaque pagination cursor over the ledger. It is
// the (created_at, id) of the last entry returned; the next page begins
// strictly before it (newest-first). A nil cursor starts from the newest row.
type LedgerCursor struct {
	CreatedAt time.Time
	ID        int64
}

// DepositEvent is one inbound SPL transfer instruction into the treasury ATA,
// persisted before any credit is applied. assigned_account_id is nil until the
// sending wallet is linked; assignment_state is 'unassigned' until then.
type DepositEvent struct {
	TransactionSignature string
	InstructionIndex     int
	Slot                 int64
	FromWallet           string
	AmountRaw            int64
	AssignedAccountID    *string
	AssignmentState      string // unassigned | assigned | skipped
	CreditedAt           *time.Time
	SeenAt               time.Time
}

// DepositCursor is the stable, opaque pagination cursor over a account's
// credited deposits, newest-first: the (seen_at, transaction_signature,
// instruction_index) of the last entry returned. A nil cursor starts from the
// newest row.
type DepositCursor struct {
	SeenAt               time.Time
	TransactionSignature string
	InstructionIndex     int
}

// WithdrawalState is the durable withdrawal state machine.
type WithdrawalState string

const (
	WithdrawalReserved  WithdrawalState = "reserved"
	WithdrawalSigned    WithdrawalState = "signed"
	WithdrawalBroadcast WithdrawalState = "broadcast"
	WithdrawalFinalized WithdrawalState = "finalized"
	WithdrawalFailed    WithdrawalState = "failed"
	WithdrawalRestored  WithdrawalState = "restored"
)

// WithdrawalRequest initiates a withdrawal. The destination is the seller's
// primary linked wallet; idempotency_key makes retries safe.
type WithdrawalRequest struct {
	AccountID         string
	IdempotencyKey    string
	DestinationWallet string
	AmountRaw         int64
}

// Withdrawal is the persisted withdrawal record.
type Withdrawal struct {
	ID                   int64
	AccountID            string
	IdempotencyKey       string
	DestinationWallet    string
	AmountRaw            int64
	State                WithdrawalState
	SignedWire           string
	Signature            string
	Blockhash            string
	LastValidBlockHeight *uint64
	BlockhashExpiresAt   *time.Time
	Error                string
	ReservedAt           time.Time
	SignedAt             *time.Time
	BroadcastAt          *time.Time
	FinalizedAt          *time.Time
}

// WithdrawalRef is the stable ledger reference for one withdrawal. The
// (WithdrawalRef(id), leg) pair is globally unique under credit_ledger:
// leg="withdraw" is the reserve-time debit (credit_raw -= amount) and
// leg="restore" is the recovery-time credit (credit_raw += amount). A
// finalized withdrawal records only the debit; a restored one records both,
// netting to zero. The store's ReserveWithdrawal/RestoreWithdrawal are the
// only writers.
func WithdrawalRef(id int64) string { return "withdrawal:" + strconv.FormatInt(id, 10) }

// WithdrawalCursor is the stable, opaque pagination cursor over an account's
// withdrawals, newest-first: the (reserved_at, id) of the last entry
// returned. A nil cursor starts from the newest row.
type WithdrawalCursor struct {
	ReservedAt time.Time
	ID         int64
}

// Reconciliation is the result of verifying account_credits against the
// confirmed ledger and open reservations. An account is reconciled when
// ExpectedCredit equals CreditRaw, ExpectedReserved equals ReservedRaw, and no
// invariant is violated.
type Reconciliation struct {
	AccountID        string
	CreditRaw        int64
	ReservedRaw      int64
	ExpectedCredit   int64 // SUM(credit_ledger.delta_raw)
	ExpectedReserved int64 // SUM(billing_requests.reserved_raw WHERE state='reserved')
	LedgerRows       int64
	OpenReservations int64
	DriftCredit      int64
	DriftReserved    int64
	InvariantHeld    bool // reserved_raw <= credit_raw && both >= 0
}

// ErrInsufficientCredit means the account's available balance could not cover
// the conservative reservation.
var ErrInsufficientCredit = errors.New("billing: insufficient credit")

// ErrPriceAboveMax means no live peer is affordable at the buyer's caps.
var ErrPriceAboveMax = errors.New("billing: price above max")

// ErrBillingAccountRequired means the authenticated key has no owning account.
var ErrBillingAccountRequired = errors.New("billing: account required")

// ErrBillingUnsupportedRoute means the route/media type has no valid token bound.
var ErrBillingUnsupportedRoute = errors.New("billing: unsupported route")

// ErrBillingUnavailable means the billing plane is disabled or degraded.
var ErrBillingUnavailable = errors.New("billing: unavailable")

// ErrNotFound means the referenced entity does not exist.
var ErrNotFound = errors.New("billing: not found")

// ErrConflict means a precondition was violated (duplicate id, bad payload,
// unsupported peer for an in-flight request, etc.).
var ErrConflict = errors.New("billing: conflict")

// ErrInvalid means a deposit or withdrawal payload was malformed (missing
// signature, non-positive amount, missing wallet). It is distinct from
// ErrConflict (duplicate) and ErrNotFound (missing row).
var ErrInvalid = errors.New("billing: invalid")

// ErrOverflow means an integer computation would overflow a signed int64.
var ErrOverflow = errors.New("billing: overflow")

// checkedMul returns a*b or ErrOverflow. Non-negative operands only; a
// negative operand is treated as overflow (every quantity here is >= 0).
func checkedMul(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrOverflow
	}
	if a == 0 || b == 0 {
		return 0, nil
	}
	r := a * b
	if r/a != b {
		return 0, ErrOverflow
	}
	return r, nil
}

// checkedAdd returns a+b or ErrOverflow (non-negative operands).
func checkedAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrOverflow
	}
	r := a + b
	if r < a {
		return 0, ErrOverflow
	}
	return r, nil
}

// checkedSub returns a-b or ErrOverflow (a >= b >= 0 expected).
func checkedSub(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrOverflow
	}
	if a < b {
		return 0, ErrOverflow
	}
	return a - b, nil
}

// ceilDiv returns ceil(num/den) for num >= 0, den > 0, checking the rounding
// add against overflow.
func ceilDiv(num, den int64) (int64, error) {
	if den <= 0 {
		return 0, fmt.Errorf("billing: non-positive divisor %d", den)
	}
	if num < 0 {
		return 0, ErrOverflow
	}
	rounded, err := checkedAdd(num, den-1)
	if err != nil {
		return 0, err
	}
	return rounded / den, nil
}

// Cost computes the combined base-unit charge for a usage record at the given
// rates, rounding the combined sum UP to the nearest base unit so a partial
// unit is always paid and never lost. Multiplication and addition are checked
// against int64 overflow. A zero result means the request did no billed work
// or the peer is unpriced; the caller releases the reservation in that case
// rather than recording a charge.
func Cost(regularInput, cachedInput, output int, inRate, cachedRate, outRate int64) (int64, error) {
	if regularInput < 0 || cachedInput < 0 || output < 0 {
		return 0, fmt.Errorf("billing: negative tokens (%d,%d,%d)", regularInput, cachedInput, output)
	}
	if inRate < 0 || cachedRate < 0 || outRate < 0 {
		return 0, fmt.Errorf("billing: negative rate (%d,%d,%d)", inRate, cachedRate, outRate)
	}
	pIn, err := checkedMul(inRate, int64(regularInput))
	if err != nil {
		return 0, err
	}
	pCached, err := checkedMul(cachedRate, int64(cachedInput))
	if err != nil {
		return 0, err
	}
	pOut, err := checkedMul(outRate, int64(output))
	if err != nil {
		return 0, err
	}
	sum, err := checkedAdd(pIn, pCached)
	if err != nil {
		return 0, err
	}
	sum, err = checkedAdd(sum, pOut)
	if err != nil {
		return 0, err
	}
	if sum == 0 {
		return 0, nil
	}
	return ceilDiv(sum, Million)
}

// Fee splits costRaw into the routing fee (floor(cost * feeBps / 10_000)) and
// the seller's share (cost - fee). feeBps == 0 is pure P2P (the devnet
// default). feeBps is clamped to [0, 10000].
func Fee(costRaw int64, feeBps int) (feeRaw, sellerRaw int64, err error) {
	if costRaw < 0 {
		return 0, 0, ErrOverflow
	}
	if feeBps < 0 {
		feeBps = 0
	}
	if feeBps > 10000 {
		feeBps = 10000
	}
	if feeBps == 0 || costRaw == 0 {
		return 0, costRaw, nil
	}
	num, err := checkedMul(costRaw, int64(feeBps))
	if err != nil {
		return 0, 0, err
	}
	feeRaw = num / 10000 // floor; feeBps <= 10000 guarantees feeRaw <= costRaw
	sellerRaw, err = checkedSub(costRaw, feeRaw)
	if err != nil {
		return 0, 0, err
	}
	return feeRaw, sellerRaw, nil
}

// worstCaseCost is the largest possible charge for one quote given conservative
// total-input and output ceilings. Because cache creation is billed at the
// regular rate, the input budget is spent at max(inRate, cachedRate) in the
// worst case (all cached when cached is dearer, all regular otherwise).
func worstCaseCost(q EligiblePeerQuote, inputCeil, outputCeil int) (int64, error) {
	if inputCeil < 0 || outputCeil < 0 {
		return 0, fmt.Errorf("billing: negative ceiling (%d,%d)", inputCeil, outputCeil)
	}
	inRate := q.InputPerMillion
	if q.CachedInputPerMillion > inRate {
		inRate = q.CachedInputPerMillion
	}
	pIn, err := checkedMul(inRate, int64(inputCeil))
	if err != nil {
		return 0, err
	}
	pOut, err := checkedMul(q.OutputPerMillion, int64(outputCeil))
	if err != nil {
		return 0, err
	}
	sum, err := checkedAdd(pIn, pOut)
	if err != nil {
		return 0, err
	}
	if sum == 0 {
		return 0, nil
	}
	return ceilDiv(sum, Million)
}

// ReserveAmount returns the largest possible charge across quotes for a
// request with at most inputCeil total input tokens (regular plus cached) and
// outputCeil output tokens. Reserving this amount guarantees no in-flight
// request can overdraft, regardless of which peer serves it or how the input
// is split between regular and cached. A zero result means every quote is
// unpriced; the request is still recorded with a zero reservation.
func ReserveAmount(quotes []EligiblePeerQuote, inputCeil, outputCeil int) (int64, error) {
	var max int64
	for _, q := range quotes {
		c, err := worstCaseCost(q, inputCeil, outputCeil)
		if err != nil {
			return 0, err
		}
		if c > max {
			max = c
		}
	}
	return max, nil
}

// MergeCaps overlays request caps onto account caps: each supplied (non-nil)
// request dimension tightens the corresponding account default; nil means no
// per-request override, so only a non-nil lower bound can further restrict an
// account cap.
func MergeCaps(account, request Caps) Caps {
	return Caps{
		InputPerMillion:       tighterCap(account.InputPerMillion, request.InputPerMillion),
		CachedInputPerMillion: tighterCap(account.CachedInputPerMillion, request.CachedInputPerMillion),
		OutputPerMillion:      tighterCap(account.OutputPerMillion, request.OutputPerMillion),
	}
}

func tighterCap(account, request *int64) *int64 {
	switch {
	case account == nil:
		return request
	case request == nil:
		return account
	case *request < *account:
		return request
	default:
		return account
	}
}

// BillingStore is the persistence contract for the market and settlement. All
// spend is authorized and finalized inside row-locked transactions; read
// methods (LiveAsks, AccountCredit, ListLedger, ReconcileAccount) may be served
// from a cache but a cached read must never mutate a balance or skip a
// reservation.
type BillingStore interface {
	// Asks: the market. ReplaceAsks performs a full replacement of the peer's
	// asks within a single transaction, assigning one fresh revision across all
	// rows and deleting any (service, model) not in the new set (an empty set
	// clears all of the peer's asks). The caller validates the service/model
	// allowlist; the store enforces structural limits (count <= 256, no
	// duplicate (service, model), non-negative rates bounded by MaxBaseRate).
	// It returns the freshly assigned revision (0 when the asks were cleared).
	ReplaceAsks(ctx context.Context, peerID string, asks []Ask, ttl time.Duration) (int64, error)
	LiveAsks(ctx context.Context, service, model string, now time.Time) ([]Ask, error)

	// Account credit: the projection. EnsureAccountCredit creates the row
	// (zero balance, no caps) if absent. SetAccountCaps updates the three
	// buyer caps (a nil cap clears that dimension to unlimited).
	EnsureAccountCredit(ctx context.Context, accountID string) error
	AccountCredit(ctx context.Context, accountID string) (AccountCredit, error)
	SetAccountCaps(ctx context.Context, accountID string, caps Caps) (AccountCredit, error)

	// Allowances (Phase 2, design §11): the mirrored SPL delegation registry.
	// UpsertAllowance applies one observed absolute allowance for
	// (account, delegate) atomically: the registry row is updated and the
	// grant/revocation delta is mirrored into the credit projection through
	// exactly-once ledger legs (ref = the observation ref). A grant credits
	// the account; a revocation debits it, clamped at reserved_raw (open
	// reservations keep their backing) with the shortfall reported. Re-running
	// the same observation is a no-op. The reserve gate bounds spend by
	// min(credit available, Σ active allowances) whenever any active
	// delegation exists (§11.3).
	UpsertAllowance(ctx context.Context, ch AllowanceChange) (AllowanceChangeResult, error)
	AccountAllowances(ctx context.Context, accountID string) ([]Allowance, error)
	// DelegateAllowances lists the active allowances naming `delegate`, for
	// the settlement worker to plan buyer→seller transfers.
	DelegateAllowances(ctx context.Context, delegate string) ([]Allowance, error)

	// Reservation. ReserveBilling computes the conservative reserve from the
	// snapshot, checks available credit under a row lock, bumps reserved_raw,
	// and persists the request with its immutable quote snapshot. Returns
	// ErrInsufficientCredit when the account cannot cover the reserve and
	// ErrConflict when the request_id already exists.
	ReserveBilling(ctx context.Context, req Reservation) (AccountCredit, error)
	BillingRequest(ctx context.Context, requestID string) (Request, error)

	// Settlement: exact and idempotent. SettleBilling finalizes a reserved
	// request against the served peer resolved from the immutable snapshot,
	// computes the integer charge (checked, ceiling), debits the buyer,
	// credits the seller and any fee, and writes unique ledger legs — all in
	// one transaction. A repeated call for an already-settled request is a
	// no-op. A zero charge (priced peer, no tokens; or unpriced peer) releases
	// the reservation instead of recording an empty charge. The served peer
	// must be present in the snapshot; an unknown peer releases the
	// reservation (ErrConflict) and is never priced at a fresh read.
	SettleBilling(ctx context.Context, requestID, servedPeerID string, usage Usage, feeBps int, now time.Time) (Request, error)
	ReleaseBilling(ctx context.Context, requestID, reason string, now time.Time) (Request, error)

	// SweepStaleReservations releases reserved requests older than `before`
	// using FOR UPDATE SKIP LOCKED so concurrent replicas stay safe. The
	// recovery worker calls it on a cadence older than the max request lifetime.
	SweepStaleReservations(ctx context.Context, before time.Time, limit int) ([]string, error)

	// Reconciliation verifies account_credits against confirmed ledger
	// movements and open reservations, and surfaces invariant violations.
	ReconcileAccount(ctx context.Context, accountID string, now time.Time) (Reconciliation, error)
	ListLedger(ctx context.Context, accountID string, cursor *LedgerCursor, limit int) ([]LedgerEntry, *LedgerCursor, error)

	// Deposits. Every inbound SPL transfer into the treasury ATA is persisted
	// BEFORE any credit is applied (InsertDepositEvent is idempotent via the
	// (signature, instruction_index) primary key; inserted is false when the
	// row already existed from a previous scan). The deposit's from_wallet is
	// recorded but NO account id is ever accepted from the chain or a memo —
	// attribution is resolved only against linked wallets.
	//
	// ApplyDeposit credits one persisted event to accountID, transactionally
	// and idempotently (credit_ledger UNIQUE(ref, leg) makes retries no-ops).
	// It sets assignment_state='assigned' and credited_at. An already-assigned
	// event for the SAME account is a no-op; an already-assigned event for a
	// DIFFERENT account returns ErrConflict (a wallet cannot be linked to two
	// accounts, so this guards only against stale re-link races).
	//
	// DepositCursor/AdvanceDepositCursor persist the watcher's high-water mark
	// per treasury ATA. AdvanceDepositCursor is a compare-and-set: it advances
	// only when the stored signature still equals `fromSignature` (or the row is
	// absent and `fromSignature == ""`). This lets multiple replicas retry
	// safely without skipping a page.
	InsertDepositEvent(ctx context.Context, ev DepositEvent) (inserted bool, err error)
	ApplyDeposit(ctx context.Context, signature string, instructionIndex int, accountID string, now time.Time) (DepositEvent, error)
	DepositCursor(ctx context.Context, treasuryATA string) (signature string, err error)
	AdvanceDepositCursor(ctx context.Context, treasuryATA, fromSignature, toSignature string) (advanced bool, err error)

	// ReconcileDepositsForWallet credits every not-yet-credited deposit from
	// wallet to accountID, returning the count applied. Called after the wallet
	// is linked so transfers that arrived before linkage are not lost. It is
	// idempotent: re-running finds no unassigned rows.
	ReconcileDepositsForWallet(ctx context.Context, wallet, accountID string, now time.Time) (int, error)

	// ListDepositEvents pages over a account's credited deposits, newest-first.
	ListDepositEvents(ctx context.Context, accountID string, cursor *DepositCursor, limit int) ([]DepositEvent, *DepositCursor, error)

	// Withdrawals (Step 7). The durable state machine is
	//   reserved -> signed -> broadcast -> finalized
	// with `restored` (credit returned) and `failed` as terminal recovery
	// states. Unlike an inference reservation (which bumps reserved_raw and
	// only debits credit_raw on settlement), a withdrawal DEBITS credit_raw at
	// reserve time and writes a `withdraw` ledger leg immediately — the funds
	// have left the off-chain projection and cannot be double-spent on
	// inference. A UNIQUE(withdrawal_ref, leg) ledger makes retries no-ops.
	//
	// ReserveWithdrawal is idempotent on idempotency_key: a duplicate returns
	// the existing row unchanged (ErrConflict only if a DIFFERENT account owns
	// the key). It returns ErrInsufficientCredit when available credit
	// (credit_raw - reserved_raw) cannot cover amount, and ErrInvalid when the
	// account, destination, idempotency key, or amount is missing/invalid.
	ReserveWithdrawal(ctx context.Context, req WithdrawalRequest, now time.Time) (Withdrawal, error)

	// Withdrawal returns one withdrawal by id for the owning account.
	// ErrNotFound when the row does not exist OR belongs to a different
	// account, so a leaked id cannot be used to probe another user's state.
	Withdrawal(ctx context.Context, accountID string, id int64) (Withdrawal, error)

	// MarkWithdrawalSigned persists the signed wire transaction and its
	// deterministic signature before broadcast, transitioning reserved ->
	// signed. lastValidBlockHeight is the chain-derived expiry proof the
	// worker uses to decide when a non-finalized tx cannot land. A row no
	// longer in the 'reserved' state is returned unchanged (another replica
	// won the race), so the caller re-reads the persisted wire before
	// broadcasting instead of trusting its own computation.
	MarkWithdrawalSigned(ctx context.Context, id int64, signedWire, signature, blockhash string, lastValidBlockHeight uint64, blockhashExpiresAt, now time.Time) (Withdrawal, error)

	// MarkWithdrawalBroadcast transitions signed -> broadcast, recording the
	// first broadcast time. Idempotent for a row already broadcast or further
	// along; a row not yet 'signed' is returned unchanged.
	MarkWithdrawalBroadcast(ctx context.Context, id int64, now time.Time) (Withdrawal, error)

	// FinalizeWithdrawal transitions a broadcast (or signed) withdrawal to
	// finalized once the on-chain transaction is confirmed. No balance change:
	// the debit was applied at reserve time. A row not in an open state is
	// returned unchanged.
	FinalizeWithdrawal(ctx context.Context, id int64, now time.Time) (Withdrawal, error)

	// RestoreWithdrawal returns the reserved amount to the account
	// (credit_raw += amount, unique `restore` ledger leg) and transitions the
	// row to 'restored'. Called by the worker once blockhash expiry proves the
	// transaction cannot land, or on an unrecoverable construction/broadcast
	// error before finalization. A row not in an open state
	// (reserved/signed/broadcast) is returned unchanged, so a concurrent
	// finalize beats a late restore cleanly.
	RestoreWithdrawal(ctx context.Context, id int64, reason string, now time.Time) (Withdrawal, error)

	// FailWithdrawal records an error and transitions the row to 'failed'
	// WITHOUT restoring credit, parking it for investigation while the
	// blockhash timer handles restoration separately. The funds stay debited
	// until an explicit RestoreWithdrawal (manual or by the expiry sweep). A
	// row not in an open state is returned unchanged.
	FailWithdrawal(ctx context.Context, id int64, reason string, now time.Time) (Withdrawal, error)

	// SweepWithdrawalsDue returns the ids of withdrawals in an open state
	// (reserved/signed/broadcast), oldest first, locked with FOR UPDATE SKIP
	// LOCKED so concurrent replicas claim disjoint sets. The worker reads each
	// row, performs the RPC for its state, and applies the transition with a
	// state-guarded UPDATE; the lock is held only for the fast RPC, and a
	// replica that dies mid-work releases it on transaction abort.
	SweepWithdrawalsDue(ctx context.Context, limit int) ([]Withdrawal, error)

	// ListWithdrawals pages over an account's withdrawals newest-first. A nil
	// cursor starts from the newest row.
	ListWithdrawals(ctx context.Context, accountID string, cursor *WithdrawalCursor, limit int) ([]Withdrawal, *WithdrawalCursor, error)
}

// TreasuryAccountID is the reserved account credited with routing fees
// (feeBps > 0). It carries no private key in the deposit path and is created
// lazily by EnsureAccountCredit / settlement.
const TreasuryAccountID = "treasury"
