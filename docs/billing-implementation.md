# OTELA Billing — Implementation Status (Steps 1–7)

This document describes what has been built, where it lives, and how the
pieces fit together. It is a companion to `billing-design.md` (the contract)
and `.omx/plans/otela-billing-devnet-marketplace.md` (the plan).

The billing system is a **two-sided market**: sellers publish ask prices,
buyers express maximum bids (caps), and OpenTela routes, meters, and settles
without ever setting the price of inference. All seven steps are now
implemented end-to-end:

1. **Persistence and pure billing logic** — the tables, checked arithmetic,
   and store.
2. **Account identity, request inspection, and caps** — threading the
   API-key owner through the request, parsing the body, clamping output.
3. **Seller asks and constrained routing** — the pricing API, the shared
   peer snapshot, and the gate that reserves conservatively.
4. **Exact settlement** — the post-response hook that charges against
   actual usage (or releases), plus a stale-reservation sweeper.
5. **Deposits** — a Solana watcher that credits inbound SPL transfers.
6. **Management API and UI** — balance, caps, deposit instructions, the
   market price array, and a ledger the seller can read.
7. **Withdrawals** — a durable state machine that signs, broadcasts, and
   finalizes SPL transfers from the treasury to a seller's wallet.

> **Not yet live in production.** `BILLING_MODE` defaults to `off`. In that
> mode the billing gate is a zero-cost pass-through and none of the new
> tables are written. The system is opt-in via `BILLING_MODE=observe`
> (shadow) or `BILLING_MODE=enforce` (production).

---

## Table of contents

1. [Overview and data flow](#1-overview-and-data-flow)
2. [Step 1 — Persistence and pure billing logic](#2-step-1--persistence-and-pure-billing-logic)
3. [Step 2 — Account identity, request inspection, and caps](#3-step-2--account-identity-request-inspection-and-caps)
4. [Step 3 — Seller asks and constrained routing](#4-step-3--seller-asks-and-constrained-routing)
5. [Step 4 — Exact settlement](#5-step-4--exact-settlement)
6. [Step 5 — Deposits](#6-step-5--deposits)
7. [Step 6 — Management API and UI](#7-step-6--management-api-and-ui)
8. [Step 7 — Withdrawals](#8-step-7--withdrawals)
9. [Configuration](#9-configuration)
10. [Server wiring — `cmd/server/main.go`](#10-server-wiring--cmdservermaingo)
11. [Mesh-side changes](#11-mesh-side-changes)
12. [Test coverage](#12-test-coverage)
13. [What is explicitly not implemented](#13-what-is-explicitly-not-implemented)
14. [File manifest](#file-manifest)

---

## 1. Overview and data flow

```
  Buyer (API key)                              Seller (node operator)
        │                                            │
        │  1. POST /v1/service/{svc}/v1/chat/...     │  POST /internal/pricing
        │     Authorization: Bearer <key>            │   (pricing-scoped node cred)
        ▼                                            ▼
┌──────────────────────────────────────────────────────────────┐
│  api.opentela.ai (control plane)                              │
│                                                               │
│  auth.Middleware ──► validates key, threads account.ID(ctx)  │
│        │                                                      │
│        ▼                                                      │
│  billinggate.Service ──► gate.Inspect(req)                   │
│        │                      │                               │
│        │   2. resolve buyer caps (account + per-req headers) │
│        │   3. peers.Snapshot  (shared, policy-filtered)      │
│        │   4. store.LiveAsks  (service, model)               │
│        │   5. intersect + filter affordable (cheapest 128)    │
│        │   6. store.ReserveBilling  (enforce only)            │
│        │   7. X-Otela-Allowed-Peers: peer-A,peer-B,...        │
│        ▼                                                      │
│  reverse proxy ──────────────────────►  mesh head             │
│        │                                                      │
│        │  8. perf hook parses actual usage                    │
│        ▼                                                      │
│  settlement.Settler.Callback ──► SettleBilling or Release    │
└──────────────────────────────────────────────────────────────┘
                                          │
                              ┌───────────┴───────────┐
                              │ globalServiceForward  │
                              │  intersect candidates │
                              │  with X-Otela-        │
                              │  Allowed-Peers        │
                              │  (before LB + retry)  │
                              └───────────┬───────────┘
                                          │
                                   worker (peer)
```

The shared `peers.Service` snapshot is consumed by **both** the public catalog
and the billing gate, so the market view and the routing constraint are
derived from the same policy-filtered source of truth. The seller side is
served by two background loops — a **deposit watcher** that credits inbound
SPL transfers and a **withdrawal worker** that drains earnings to a seller's
wallet — both reading the same immutable `credit_ledger`.

### Two-sided market, end to end

| Stage | Who | How |
|-------|-----|-----|
| **Publish ask** | Seller node | `POST /internal/pricing` with a pricing-scoped node credential. Rates stored in `peer_asks` with a TTL. |
| **Set buyer caps** | Buyer (operator) | `PATCH /manage/billing/preferences`, or per-request `X-Max-*-Per-Million` headers. Stored in `account_credits`. |
| **Reserve** | Control plane (gate) | Before forwarding: resolve affordable live peers, take the largest single-peer worst-case cost, debit `reserved_raw`. |
| **Route** | Mesh head | `intersectAllowedPeers` keeps only the gate's allowed set, before load-balancing and retry. |
| **Meter** | Control plane (perf hook) | Parse actual `input` / `cached_input` / `output` tokens from the streaming response. |
| **Settle** | Control plane (settler) | `SettleBilling` charges the buyer, credits the seller + treasury, writes one ledger leg per role. Exactly-once via `UNIQUE(ref, leg)`. |
| **Deposit** | Anyone → treasury ATA | Solana watcher polls finalized signatures, credits the linked owner. |
| **Withdraw** | Seller → own wallet | `POST /manage/billing/withdrawals` debits credit; worker signs, broadcasts, finalizes (or restores on failure). |

---

## 2. Step 1 — Persistence and pure billing logic

### 2.1 Migrations — `migrations/0006_billing.sql`, `0008_deposit_cursor.sql`, `0009_withdrawal_expiry.sql`

Six new tables, all with `CHECK` constraints and idempotency guarantees:

| Table | Purpose |
|-------|---------|
| `peer_asks` | Seller-published prices per `(peer_id, service, model)`. TTL via `expires_at`; `UNIQUE (peer_id, service, model)`. `CHECK` enforces non-negative rates and a sane ceiling (`< 1e12`). |
| `account_credits` | Single fungible balance per account. `credit_raw` is the total; `reserved_raw` is part of (not in addition to) it — `CHECK (reserved_raw <= credit_raw)`. Holds per-account caps (`max_input_per_million`, `max_cached_input_per_million`, `max_output_per_million`). |
| `billing_requests` | One row per gated inference request. `state` is `reserved`/`settled`/`released`. `eligible_peers` (JSONB) is the **immutable quote snapshot** taken at the gate. `reserved_raw` is the conservative maximum. |
| `credit_ledger` | Append-only ledger. `UNIQUE (ref, leg)` makes retried settlements exactly-once no-ops. `source` is `usage`/`earn`/`fee`/`deposit`/`withdraw`/`adjust`. |
| `deposit_events` | Solana transfer instructions, persisted before credit is applied (Step 5). `assignment_state` is `unassigned`/`assigned`/`skipped`. |
| `withdrawals` | Withdrawal lifecycle: `reserved`→`signed`→`broadcast`→`finalized` (or `failed`/`restored`). `idempotency_key` is unique (Step 7). |

`0008_deposit_cursor.sql` adds the per-treasury durable signature cursor used
by the deposit watcher. `0009_withdrawal_expiry.sql` adds
`last_valid_block_height`, the chain-derived expiry proof used before an
unconfirmed withdrawal may be restored.

### 2.2 Pure package — `internal/billing/billing.go` (541 lines)

No database, no `net/http` — pure types and checked arithmetic. Importing it
imposes no I/O dependencies.

**Types:**
- `Ask` — seller's published three-tier rates (`InputPerMillion`,
  `CachedInputPerMillion`, `OutputPerMillion`) + `Revision`.
- `Caps` — buyer's three maximums (each `*int64`; nil = unlimited).
- `AccountCredit` — `CreditRaw`, `ReservedRaw`, per-account `Caps`. `Available()`
  returns `credit_raw - reserved_raw`.
- `EligiblePeerQuote` — one peer's snapshot at the gate: `PeerID`,
  `SellerAccountID`, `OwnerWallet`, `Revision`, three rates.
- `Usage` — `InputTokens`, `CachedInputTokens`, `OutputTokens`.
- `Reservation` — what the gate hands to the store: `RequestID`,
  `BuyerAccountID`, `Service`, `Model`, `Caps`, `Quotes`, `InputCeil`,
  `OutputCeil`.
- `Request` — full billing request row (state, amounts, usage, timestamps).
- `LedgerEntry`, `LedgerCursor`, `DepositEvent`, `Withdrawal`,
  `Reconciliation`.

**Checked arithmetic** (overflow-safe, used everywhere money moves):
- `checkedMul`, `checkedAdd`, `checkedSub` — return `ErrOverflow` on wrap.
- `ceilDiv` — ceiling division (charges round up, never down).
- `Cost(regularInput, cachedInput, output, inRate, cachedRate, outRate)` —
  the three-tier meter: `regular * inRate + cached * cachedRate + output * outRate`,
  all ceiling-divided by `Million`. Cache **writes** bill at the regular input
  rate; only cache **reads** bill at the cheaper cached rate.
- `Fee(costRaw, feeBps)` — splits into `feeRaw` + `sellerRaw`.
- `worstCaseCost(quote, inputCeil, outputCeil)` — the largest possible charge
  for one peer: `max(inputRate, cachedRate) * inputCeil + outputRate * outputCeil`.
- `ReserveAmount(quotes, inputCeil, outputCeil)` — the **largest**
  worst-case cost among the quotes (the conservative reserve). The mesh
  routes each request to exactly one peer, so the buyer is reserved for at
  most that one peer's worst case regardless of which eligible peer serves.
- `Affordable(quote, caps)` — true only when all three rates are within caps.
- `MergeCaps(account, request)` — per-request headers tighten (never loosen)
  account defaults. A nil dimension in the request falls back to the account
  value; if both are nil the dimension is unlimited.

**Sentinel errors:** `ErrInsufficientCredit`, `ErrPriceAboveMax`,
`ErrBillingAccountRequired`, `ErrBillingUnsupportedRoute`,
`ErrBillingUnavailable`, `ErrNotFound`, `ErrConflict`, `ErrOverflow`.

**`BillingStore` interface** — the contract the store implements:
`ReplaceAsks`, `LiveAsks`, `EnsureAccountCredit`, `AccountCredit`,
`SetAccountCaps`, `ReserveBilling`, `SettleBilling`, `ReleaseBilling`,
`ReconcileAccount`, `ListLedger`, `BillingRequest`, plus deposit/withdrawal
methods (deferred to Steps 5/7).

`TreasuryAccountID = "treasury"` — the fee recipient and the only account that
can receive `fee` ledger legs.

### 2.3 Store implementation — `internal/store/billing.go` (769 lines)

All methods use `pool.BeginTx` + `defer tx.Rollback` + `FOR UPDATE` row locks.
Caches accelerate reads but **never authorize spending or move a balance** —
only the row-locked transaction does.

| Method | Behaviour |
|--------|-----------|
| `ReplaceAsks(peerID, asks, ttl)` | Upserts all asks for a peer in one transaction, deleting any not in the new set. Validates non-negative rates, ≤256 asks, known service/model, no duplicates. Sets `expires_at = now + ttl`. |
| `LiveAsks(service, model, now)` | Returns non-expired asks for the (service, model) pair, sorted cheapest input rate first. |
| `EnsureAccountCredit(accountID)` | Inserts a zero row if one doesn't exist (idempotent). |
| `AccountCredit(accountID)` | Reads the row into `billing.AccountCredit`. |
| `SetAccountCaps(accountID, caps)` | Updates the three per-account caps. |
| `ReserveBilling(reservation)` | Inserts a `billing_requests` row in `reserved` state. Under `FOR UPDATE` on the buyer's `account_credits`, checks `available >= reserve` and bumps `reserved_raw` (CHECK constraint prevents overdraft even under a race). Returns the updated credit. |
| `SettleBilling(requestID, servedPeerID, usage, feeBps, now)` | Resolves the served peer against the **immutable `eligible_peers` snapshot** (never a fresh read). Computes exact `cost_raw`, splits the fee, reduces buyer `credit_raw`/`reserved_raw`, credits the snapshotted seller, credits the treasury, and inserts exactly one ledger leg per role. `UNIQUE (ref, leg)` makes retries no-ops. Unknown peer → release. Zero cost → release. Buyer == seller → balanced (no fee leg needed on devnet). |
| `ReleaseBilling(requestID, reason, now)` | Releases the reservation back to the buyer. Idempotent — releasing an already-settled or already-released request is a no-op. |
| `ReconcileAccount(accountID, now)` | Returns the drift between reserved, settled, and ledger sums for monitoring. |
| `ListLedger(accountID, cursor, limit)` | Cursor-paginated ledger read. |

`scanBillingRequest` scans all nullable columns (`eligible_peers`, `cost_raw`,
`served_peer_id`, usage fields, timestamps) into **pointers** then
dereferences, as required by `pgx`.

### 2.4 Tests — `internal/billing/billing_test.go`, `internal/store/billing_test.go`

**Pure package** (9 tests): `Cost` rounding and overflow, zero cost, negative
rejection, `Fee` split, `Affordable` filtering, `ReserveAmount` worst-case and
unpriced peers, `MergeCaps`.

**Store** (11 tests, requires `TEST_DATABASE_URL`, run with `-race`):
- `ReplaceAsks` lifecycle and validation
- `ReserveBilling` insufficient and conservative
- **Concurrent reservations never overdraft** (500 goroutines vs 50 credit,
  verified under `-race`)
- `SettleBilling` charges snapshot and is idempotent
- `SettleBilling` unknown peer releases; zero cost releases; buyer==seller
  balanced
- `ReleaseBilling` idempotent
- `ReconcileAccount` drift
- `ListLedger` cursor pagination

---

## 3. Step 2 — Account identity, request inspection, and caps

### 3.1 Account context — `internal/account/account.go`

`WithID(ctx, id)` and `ID(ctx) (string, bool)` thread the API-key owner's
account ID through the request. An empty ID is stored as absent so legacy keys
(keys with no `user_id`) return `ok=false`. This is deliberately separate from
the Neon-JWT principal identity: API-key requests have no JWT, and the
account ID (`api_keys.user_id`) is the billing identity for the hot path.

### 3.2 Key validation widened — `internal/store/acl.go`

`KeyStore.Validate` now returns `(accountID, ok, err)` instead of `(ok, err)`.
`Postgres.Validate` queries `SELECT active, user_id FROM api_keys`, returning
the owning account ID (empty for legacy keys with `NULL user_id`). Existing
callers that ignored the first return are unaffected.

### 3.3 Auth validator and middleware — `internal/auth/`

- `TokenValidator.Valid` widened to `(accountID, ok, error)`.
- A new `Verdict{Valid, AccountID}` is cached via the generalized
  `cache.Cache[T]` (was bool-specific; now `Cache[T any]`).
- `NewValidationCache(janitorEvery)` constructor for the cache.
- `auth.Middleware` accepts variadic `Options{EnforceAccount bool}`:
  - Zero value preserves current behavior (backward compatible).
  - When `EnforceAccount` is on (set when `BILLING_MODE=enforce`), a valid
    key with no account ID is rejected with `402 billing_account_required`.
  - In `observe`, legacy keys are allowed through (the gate shadows but
    doesn't reject).
  - The validated account ID is placed in context via `account.WithID`.

### 3.4 Request inspector — `internal/gate/inspect.go` (380 lines)

`Inspect(req *http.Request, opts Options) (Plan, *http.Request)` — no error
return. Malformed or oversized bodies degrade gracefully (`Supported=false`,
fields zero) rather than erroring. The body is restored byte-for-byte via
`io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), req.Body))`;
`Content-Length` is left untouched.

**`Plan` fields:** `Service`, `Route`, `Model`, `Streaming`, `IncludeUsage`,
`InputCeiling`, `OutputCeiling`, `Generative`, `Supported`.

**Service/route parsing:** from the path `/v1/service/{service}/v1/...`
(local implementation, no cross-package dependency on `perf`).

**Body inspection — streaming JSON decoder:**
- A `json.Decoder` with `Token()` reads early fields incrementally from a
  bounded 1 MiB prefix (`InspectCap`). This handles large request bodies
  where `model` is in the first 1 MiB but the prompt content extends beyond —
  unlike `json.Unmarshal`, which requires a complete object.
- `dec.UseNumber()` for exact integer parsing.
- `bodyShape` struct with `Model`, `Streaming`, `IncludeUsage`,
  `MaxTokens *int`, `MaxCompletion *int`, `MaxOutputTokens *int`.
- Helper functions: `readIntPtr`, `readIncludeUsage`, `skipValue`.

**Input ceiling:**
- From `Content-Length` when known.
- When chunked (no `Content-Length`), drains up to `MaxBody` (64 MiB).
- Unbounded chunked (exceeds `MaxBody`) → `Supported=false`.

**Output ceiling:**
- `min(body max_tokens* , operator OutputMax)`.
- When the body omits a maximum and the operator cap is the only bound, the
  inspector injects `max_output_tokens` for the Responses route and
  `max_tokens` for the other supported generative routes.
- Generative route with neither → `Supported=false`.
- `max_tokens` (OpenAI Chat), `max_completion_tokens` (OpenAI Responses),
  `max_output_tokens` (Anthropic) are all recognised.

**Route classification:**
- Generative: `chat/completions`, `completions`, `messages`, `responses`.
- Input-only: `embeddings`.
- Non-inference (e.g. `GET /models`): unsupported → passes through.

**Test hook:** package var `maxBody` (default `MaxBody`) is overridable in
tests so the unbounded-chunked test doesn't allocate 64 MiB.

### 3.5 Config — `internal/config/config.go`

- `BillingMode` type: `BillingOff` (default), `BillingObserve`, `BillingEnforce`.
- `BILLING_MODE` env var (validated; invalid → error).
- `BILLING_OUTPUT_TOKEN_MAX` env var → `BillingOutputMax` (the operator
  output-token cap; zero means no cap).

### 3.6 Server threading — `internal/server/server.go`

- `NewWithControlPlanes(..., billing auth.Options)` passes `auth.Options`
  to `auth.Middleware` on every gated route.
- `New`/`NewWithInternal`/`NewWithInternalOpts` remain backward-compatible
  zero-opts wrappers (existing callers and tests unchanged).

### 3.7 Tests

- `internal/auth/middleware_test.go` (15 tests): key validation, scheme
  precedence, `TestMiddlewareThreadsAccountID`,
  `TestMiddlewareEnforceRejectsLegacyKey` (402),
  `TestMiddlewareObserveAllowsLegacyKey`.
- `internal/gate/inspect_test.go` (16 tests): service/route classification,
  model+stream+max-tokens extraction with byte-for-byte round-trip, operator
  cap clamping, OpenAI `max_completion_tokens`/`max_output_tokens`, generative
  without ceiling → unsupported, Content-Length vs chunked input ceilings,
  large-body graceful degradation, unbounded chunked rejection, unknown-field
  isolation, embeddings input-only.
- `internal/config/config_test.go` (3 new tests):
  `TestLoadBillingModeDefaultAndValid`, `TestLoadBillingModeInvalid`,
  `TestLoadBillingOutputMax`.
- `internal/server/server_test.go` (19 tests): `TestEnforceRejectsLegacyKey-
  ThroughProxy`, `TestDefaultModeAllowsLegacyKeyThroughProxy`.
- `internal/cache/cache_test.go`: updated to `New[bool](0)`.

---

## 4. Step 3 — Seller asks and constrained routing

### 4.1 Pricing-scoped node credential — `internal/nodecred/jwt.go`

- New `PricingAudience = "api.opentela.ai/internal/pricing"`, distinct from
  the existing `Audience = "api.opentela.ai/internal/acl"`.
- `Claims` gains an `Audience` field.
- `NewSignerWithAudience(kid, issuer, audience, privateKey)` and
  `NewVerifierWithAudience(issuer, audience, keys)` constructors.
- The verifier rejects any credential whose audience doesn't match its own,
  so a pricing credential can never be used for ACL authorization and vice
  versa.
- `TestPricingAudienceIsolation` proves the boundary.

### 4.2 Shared peer snapshot — `internal/peers/peers.go` (196 lines)

`Service.Snapshot(ctx) (*Snapshot, error)` is the single policy-filtered view
of the upstream mesh node table (`v1/dnt/table`), consumed by both the
public catalog and the billing gate.

- **`Peer`** — the subset of an upstream node-table entry this package reads
  (`Connected`, `[]PeerService`).
- **`Entry`** — a filtered `Peer` paired with its owning `*store.InstanceInfo`
  (seller account ID, owner wallet).
- **`Snapshot`** — `Entries` (keyed by peer ID) + `Fetched` timestamp.

**Policy filtering (fail-closed):**
- A **nil** `PolicyStore` → `ErrPolicyUnavailable` (without one, the
  permissionless/trusted classification is indeterminate and the raw table
  would leak trusted-region services).
- A policy-store **error** → `ErrPolicyUnavailable`.
- **Service-scope** instances: only services with `ExposurePermissionless`
  survive; trusted-region services are dropped.
- **Trusted-region members** (`Membership != nil`): all services are dropped
  so the peer never appears in the public catalog or the billing-eligible set.
- **Unmanaged peers** (no instance row): all advertised services are kept
  (they were never policy-managed).

A TTL cache (default `catalogCacheTTL`) accelerates reads, but a cached read
never authorizes or moves a balance.

**Tests** (`internal/peers/peers_test.go`, 7 tests, `-race`): nil/error policy
fail-closed, unmanaged peer keeps all services, service-scope filters to
permissionless only, trusted-region drops all services, cache TTL expiry,
concurrent-safe snapshot reads.

### 4.3 Catalog refactor — `internal/catalog/catalog.go`

- `Peer` and `PeerService` are now type aliases for `peers.Peer` and
  `peers.PeerService`, so the catalog and billing gate share the same data
  shapes without importing each other.
- `NewWithPolicies` accepts a `peers.PolicyStore` instead of querying the
  node table directly.
- The local `fetchTable`/`filterTable` and local error types were removed —
  catalog now delegates to `peers.Service`.
- `ServicesForPricing(ctx) ([]Service, error)` remains available as a
  distilled catalog view, but seller ask authorization no longer depends on
  this global list.

### 4.4 Pricing API — `internal/pricingapi/pricing.go` (243 lines)

`POST /internal/pricing` — authenticated by a **pricing-scoped** node
credential (distinct audience from ACL).

**Request body** (`askRequest`): a list of `{service, model, input_per_million,
cached_input_per_million, output_per_million}`.

**Flow:**
1. Authenticate via the pricing-scoped verifier; reject wrong audience,
   unauthenticated, or missing peer ID.
2. Resolve the seller: query the instance by peer ID, verify it is a
   **billable provider** (`billing.BillableProvider`, i.e. both an account
   ID and an owner wallet are present) and that the **observed wallet still
   matches** the credential's wallet (within `ownershipMaxAge`). Trusted-region
   membership is **not** required — the marketplace admits permissionless
   providers.
3. Validate the asks against that seller's **own current mesh observation**:
   ≤256 asks, non-negative rates, no duplicates, and every `(service, model)`
   must be present in the seller's advertised identity groups. A model
   advertised only by another seller cannot authorize this seller's ask.
4. `store.ReplaceAsks(peerID, asks, 5min TTL)` (returns the persisted
   revision, echoed in the response; `0` when asks are cleared).
5. Return the persisted asks.

**Response codes:** `200` (replaced, including an empty ask list that clears
all asks),
`400` (bad payload / unknown service-model / duplicate / negative rate /
stale observation), `401` (unauthenticated / wrong audience), `409` (seller
not billable / observed wallet mismatch / store conflict), `503` (store
unavailable).

The legacy `Allowlist` constructor parameter remains for compatibility, but
seller authorization is derived from the live seller observation rather than
a catalog-wide model list.

**Tests** (`internal/pricingapi/pricing_test.go`): replaces asks
(200, echoes persisted revision), clears on an empty ask list (200, revision 0),
rejects wrong audience / unauthenticated / wallet mismatch / non-billable
seller / unknown service-model / duplicate ask / negative rate / stale
observation / store conflict / too many asks.

### 4.5 Billing gate — `internal/billinggate/gate.go` (375 lines)

`Service.Middleware(inner http.Handler) http.Handler` wraps the inference
proxy. When `mode == BillingOff`, `inner` is returned unchanged (zero-cost
pass-through). In `observe` and `enforce`, the gate runs on every request:

**`serve(w, r, inner)` flow:**
1. `plan, req := gate.Inspect(r, opts)` — extract service/route/model/
   streaming/output-ceiling. Existing `max_tokens`/`max_completion_tokens`/
   `max_output_tokens` fields are clamped **down** to `OutputCeiling` when they
   exceed it. If the operator cap is the only output bound, a route-compatible
   maximum field is injected into the JSON body.
2. Read `buyer, hasAccount := account.ID(r.Context())`.
3. **Non-metered or unsupported route:** if generative but unbounded (no
   output ceiling, no operator cap), reject in enforce with `400
   billing_unsupported_route`; otherwise forward unchanged. Non-inference
   routes (e.g. `GET /models`) always pass through.
4. **Parse per-request price caps up front** (`parseRequestCaps`). A negative
   or non-integer `X-Max-*-Per-Million` value is malformed and rejected with
   `400 invalid_price_cap` in **every** mode (so observe cannot silently
   drop a bogus cap). An empty header is unlimited (nil); **zero is a valid
   cap meaning "free peers only"**.
5. **Resolve buyer caps:** `EnsureAccountCredit` (idempotent), read
   `AccountCredit`, merge with the pre-parsed request caps via `MergeCaps`
   (the **tighter** of account and request wins; nil falls back to the other
   side). In enforce, no account → `402 billing_account_required`; observe
   forwards.
6. **Resolve affordable live peers:** fetch `peers.Snapshot` (intersect with
   requested model via `model=` identity groups), fetch `store.LiveAsks`,
   build a quote per peer. Each quote must clear two filters:
   `billing.BillableProvider` (the seller has an account + wallet —
   permissionless providers qualify, no trusted-region membership) **and**
   `billing.Affordable` (all three rates within caps; unpriced peers are
   eligible at a zero quote — free to the buyer). Sort cheapest input rate
   first, cap at **128**.
7. **Empty quotes:** enforce rejects with `503 no_provider`; observe forwards
   the request **completely unchanged**.
8. **Reserve (enforce only):** `store.ReserveBilling` with a random request
   ID. On `ErrInsufficientCredit` → `402 insufficient_credit`; on
   `ErrConflict` (duplicate request ID) → `409 billing_duplicate_request`;
   other → `503 billing_unavailable`. On success, stamp the reservation id
   into the request context via `billing.WithRequestID` (the Step-4 response
   hook reads it back with `billing.RequestID`) **and** stamp
   `X-Otela-Allowed-Peers` (the cheapest 128 affordable peer IDs,
   comma-separated, **overwriting** any client-supplied value).
9. Forward to `inner`.

**In observe:** only steps 1–4 run fully. Steps 6–7 resolve but do **not**
reject (an unaffordable/empty result is forwarded unchanged). **No
reservation is persisted, `X-Otela-Allowed-Peers` is never stamped, and no
request id is threaded** — observe forwards the request so it cannot
influence routing or balances. The only shared rejection is the up-front
`400 invalid_price_cap`. This lets operators verify cap enforcement without
disrupting traffic.

**In enforce:** the production posture. Requests above any cap are rejected
before any GPU work happens.

**Tests** (`internal/billinggate/gate_test.go`, 17 tests, `-race`): off
mode pass-through, enforce reserves and forwards, enforce rejects unaffordable
peer (402 `price_above_max`), enforce rejects no account (402), enforce
rejects insufficient credit (402), enforce rejects unsupported route (400),
observe forwards without reserving (no allowed-peers, no request id), observe
forwards even when unaffordable (no allowed-peers), per-request caps override
account defaults, allowed-peers overwrites client header, capped at 128
peers, unpriced peer is affordable, non-inference route passes through,
negative/non-integer price cap → 400 in all modes, zero price cap = free
peers only, empty quotes → 503 `no_provider` in enforce, request id stamped
into context after reserve.

---

## 5. Step 4 — Exact settlement

Step 4 closes the metering loop: once the proxied response stream ends, the
control plane settles the reservation against the **exact** usage the peer
reported — or releases it without charging if there was no authoritative
usage. There is no second body parse and no in-memory balance queueing: the
perf hook hands the parsed token counts to a settlement callback, which
finalizes everything inside the store's row-locked transaction exactly once.

### 5.1 Response hook — `internal/perf/hook.go`

`perf.HookWithSettle(rec, gpus, settle)` extends the existing perf
`ModifyResponse` hook. It only samples responses stamped by the mesh
(`X-Computing-Node`) on inference service routes; everything else passes
through untouched. When the response body ends, the hook invokes `settle`
with a `perf.SettleUsage` populated from the **same parser state** that
produced the perf `Sample` — there is no re-read of the stream.

`SettleUsage` carries the **raw** served peer id (from `X-Computing-Node`),
the response `Status`, `ClientAbort`, the three token counts
(`InputTokens`, `CachedInputTokens`, `OutputTokens`), and a `Complete` flag.
`InputTokens` is normalized by the dialect parser to mean billable regular
input: OpenAI prompt tokens exclude cache reads; Anthropic input includes
cache-creation tokens. `CachedInputTokens` contains cache reads only.
`Complete` is true only when the response was 2xx, not aborted, and carried
any positive billable token count — regular input, cached input, or output —
i.e. there is a final, authoritative usage record to charge against. Every
other outcome (non-2xx, client abort, streaming
without `include_usage`, a usage-less route) is `Complete == false`.

The perf package depends on neither `billing` nor `settlement`; the caller
(the settler) converts `SettleUsage` into its own types. `perf.Hook`
(legacy, no settle callback) is preserved for callers that don't bill.

### 5.2 Settler — `internal/settlement/settle.go`

`Settler.Callback(resp *http.Response, u perf.SettleUsage)` is the
`HookWithSettle` sink. It is safe for concurrent use — one callback fires
per proxied response, and each finalize is idempotent at the store level
(`UNIQUE(ref, leg)` on the ledger; `settled`/`released` are terminal states).

1. Read `requestID, ok := billing.RequestID(resp.Request.Context())`. If
   there is no reservation id (off / observe / a non-metered route), there
   is nothing to finalize — return. Only the gate, in enforce, stamps a
   reservation id onto the outbound request via `billing.WithRequestID`, so
   the settler never touches a request that wasn't billed.
2. Open a 10-second background context (the request has already completed).
3. **Not complete** → `ReleaseBilling(requestID, reason, now)`. The reason
   is classified for the `billing_requests.release_reason` column and
   reconciliation: `client_abort`, `no_response` (status 0), `non_2xx`, or
   `no_usage` (2xx but no token count — the gate could not bound the route,
   or the stream lacked `include_usage`). Releasing rather than guessing
   means a reserved request can never be served free of charge *or*
   double-charged: an unsettlable request refunds the buyer and the
   reservation is gone, so a later settle would be a no-op.
4. **Complete** → build `billing.Usage` directly from the already-normalized
   `u.InputTokens`, `u.CachedInputTokens`, and `u.OutputTokens`. Settlement
   does not subtract cached tokens a second time.
   Then `SettleBilling(requestID, u.ServedPeerID, usage, s.feeBps, now)`.

`SettleBilling` (Step 1) resolves `servedPeerID` against the **immutable
`eligible_peers` snapshot** taken at the gate — never a fresh read — so the
peer that actually served is charged at the rate the buyer agreed to, even
if its ask changed between reserve and settle. An unknown peer or a
zero-cost result releases instead of charging. Errors are logged (with
`ErrNotFound` swallowed, since an already-terminal row is expected under
retries).

`SetFeeBps(bps)` updates the routing fee in basis points (0–10000; out of
range clamped). `feeBps == 0` disables the treasury leg — on devnet, where
the buyer may also be the seller, settlement is balanced and no fee leg is
written.

### 5.3 Stale-reservation sweeper — `internal/settlement/settle.go`

A reserved request whose response hook never ran (e.g. a crashed proxy)
would hold credit forever. `Sweeper.Run(ctx)` ticks at `BillingSweepInterval`
(default 2 min) and reclaims `reserved` rows older than `BillingSweepAge`
(default 15 min, set well above the max request lifetime so in-flight
responses are never reclaimed), up to 100 per sweep, via
`store.SweepStaleReservations` — `FOR UPDATE SKIP LOCKED`, so multiple API
replicas can run the loop concurrently without double-releasing or
contention.

### 5.4 Wiring — `cmd/server/main.go`

When `BILLING_MODE != off`, the perf hook is built with
`perf.HookWithSettle(sink, resolve, settler.Callback)` (or a `Null` recorder
variant), and a `settlement.NewSweeper(pg, BillingSweepInterval,
BillingSweepAge, 100).Run(ctx)` goroutine joins the graceful-shutdown path
alongside the deposit watcher and withdrawal worker.

### 5.5 Tests — `internal/perf/perf_test.go`, `internal/settlement/settle_test.go`

- `internal/perf/perf_test.go` (22 tests, `-race`): SSE and JSON usage
  parsing, cached-input token distinction, the settle hook firing at body
  end with `Complete` correctly set for 2xx-with-usage vs. non-2xx / abort /
  usage-less, and the `SettleUsage` peer id / token fields.
- `internal/settlement/settle_test.go` (`-race`): settle on
  complete usage, release on abort / non-2xx / no-usage, idempotent settle
  (retry is a no-op), normalized cached-input accounting, fee split, and the
  sweeper releasing stale rows while ignoring live ones.

---

## 6. Step 5 — Deposits

A background watcher credits the treasury associated token account (ATA) for
inbound SPL transfers, attributing each to a linked wallet. The design is
read-only with respect to the chain: it polls `getSignaturesForAddress` at
**finalized** commitment and credits only finalized, successful (`err == null`)
transfers of the configured mint into the dedicated treasury ATA.

**Solana primitives** (`internal/solana`). The PDA helpers
(`FindProgramAddress`, `AssociatedTokenAddress`, `IsOnCurve`, program
constants) were lifted out of `internal/faucet` into a shared package so the
watcher and the faucet (and, later, withdrawals) derive addresses identically.
`RPCClient` provides `GetSignaturesForAddress` (with an explicit `commitment`
argument), `GetTransaction` (jsonParsed), `GetSignatureStatuses`,
`LatestBlockhash`, and `SendTransaction`. `ConfirmedTransaction.TokenTransfers`
filters SPL Token `transfer` instructions by program and parses amount;
`SenderWallet` attributes the sender via `meta.preTokenBalances`, falling
back to the instruction authority for wrap-then-transfer flows.

**Store** (`internal/store/deposit.go`). `InsertDepositEvent` is idempotent
(primary key on `(transaction_signature, instruction_index)`) and persists
each transfer as `unassigned` *before* any credit is applied — a crash
mid-watch can never lose or double-count a deposit. `ApplyDeposit` is
transactional: it takes a `FOR UPDATE` row lock, calls `applyDelta` to bump
`credit_raw`, writes a `credit_ledger` leg with ref
`sol:{signature}:{index}` (the `UNIQUE(ref, leg)` makes retries no-ops), and
marks the event `assigned`. `ReconcileDepositsForWallet` batch-credits every
`unassigned` event for a wallet — used by the link hook for pre-linkage
deposits. `AccountForWallet` resolves the owning account for a wallet pubkey
(the `user_wallets.wallet` column is UNIQUE).

**Watcher** (`internal/deposits/deposits.go`). `Service.Run(ctx)` polls at
`BillingDepositPollInterval` (default 30s) until canceled. Each pass reloads
the per-treasury high-water signature from `deposit_cursors`, pages backward
to it, and processes the candidate span oldest-to-newest. The cursor advances
with compare-and-set semantics only across the contiguous successful prefix;
RPC, linker, insert, or apply failures are retried and can never be skipped by
a newer signature. Already-persisted events are passed through
`ApplyDeposit` again, whose ledger idempotency makes crash recovery safe. On
the **very first pass** only the newest page is processed, bounding startup
cost for fresh devnet treasuries. Unknown-wallet transfers remain
`unassigned` for later reconciliation, and failed transactions are skipped
without crediting.

**LinkWallet reconciliation** (`internal/walletsapi/api.go`). After a
successful `LinkWallet`, `Service.Reconciler.ReconcileDepositsForWallet(ctx,
wallet, accountID)` is invoked so deposits sent *before* the wallet was
linked are credited immediately. The link itself already succeeded, so a
reconciler error is logged and never surfaced to the user (a later poll or
re-link retries).

**Wiring** (`cmd/server/main.go`, `internal/config`). New config:
`BILLING_TREASURY_WALLET`, `BILLING_SOLANA_RPC_URL` (defaults to the faucet's
`FAUCET_SOLANA_RPC_URL`), `BILLING_DEPOSIT_MINT` / `BILLING_DEPOSIT_TOKEN_PROGRAM`
(default to the faucet's), `BILLING_DEPOSIT_POLL_INTERVAL` (default 30s).
The watcher is constructed and started only when `BILLING_MODE != off` **and**
`BILLING_TREASURY_WALLET` is set. The treasury ATA is derived once at
construction via `solana.AssociatedTokenAddress`. A `depositDone` channel
joins `sweeperDone` in the graceful-shutdown path (2s timeout).

**Why polling, not webhooks.** This implementation polls
`getSignaturesForAddress` at finalized commitment, avoiding a separate
`getSignatureStatuses` confirmation phase: every
returned signature is already safe to credit, while the durable cursor keeps
local crash recovery deterministic.

Tests: `internal/solana/rpc_test.go` (7, stubbed HTTP),
`internal/store/deposit_test.go` (9, DB-backed incl. idempotent apply and
reconciliation), `internal/deposits/deposits_test.go` (10, incl. `-race`),
`internal/walletsapi/api_test.go` (+2 reconciler tests),
`internal/config/config_test.go` (+5 deposit-config tests).

---

## 7. Step 6 — Management API and UI

Step 6 surfaces the billing state to buyers and operators through four
management endpoints and the public catalog market array, backed by the store
methods built in Steps 1 and 5.

**Backend — `internal/billingapi`.** A `Service` holds the resolved treasury
ATA, mint, decimals, and token program (derived once from `BILLING_*` config
in `main.go`) plus a billing-store subset. The owning account is resolved
from `principal.UserID` (Neon JWT, manage page) with an `account.ID`
fallback (API-key path). Four routes:

- `GET /manage/billing` returns `{mode, balance, caps, deposits,
  withdrawals_enabled, primary_linked_wallet}`.
  `deposits.enabled` is true only when `mode != off && treasury_ata != ""`, so
  the wallet page can hide the deposit panel in `off` deployments.
- `PATCH /manage/billing/preferences` sets the three buyer caps. Each is
  nullable: `null` clears that dimension to unlimited, `0` means free peers
  only, and negative is rejected with 400.
- `GET /manage/billing/ledger?cursor=&limit=` is newest-first pagination over
  the immutable `credit_ledger`. The cursor is an opaque base64 of
  `(created_at, id)` so clients cannot forge it; `nil` starts from the newest.
- `GET /manage/billing/deposits?cursor=&limit=` is the same shape over
  assigned `deposit_events` credited to the owning account. Unassigned events
  have no account owner yet and therefore are not exposed by this
  account-scoped endpoint.

Raw monetary fields (`credit_raw`, `reserved_raw`, `available_raw`, ledger
`delta_raw`, and deposit/withdrawal `amount_raw`) are decimal JSON strings.
This preserves the full signed 64-bit range in JavaScript clients. The
withdrawal request decoder temporarily accepts both decimal strings and
legacy integer JSON numbers during rollout.

`internal/manageapi.Router` gains a `BillingRoutes` interface mounted at
`/manage/billing` alongside the existing wallet/instance/region/faucet
routes, all behind the shared `principal.Middleware` + CORS.

**Catalog market array.** `internal/catalog` gains `MarketEntry` (per-model
min/median/max for input, cached-input, and output per-million, plus a
`quoters` count) and a chainable `Handler.WithAsks(AskSource)`. When wired,
`GET /v1/services` returns `{services, market}`; `market` is **omitted** when
no ask source is wired (billing off) so legacy callers see the original
shape. `AskSource` is the same `LiveAsks(ctx, service, model, now)` the gate
quotes against, so buyers and sellers never disagree about who is quoting.
Models with no live ask still appear with `quoters: 0` and zeroed triples so
the UI shows "not yet priced" rather than silently omitting the route. Ask
errors surface as 503 (`ErrAskUnavailable`) rather than the 502 used for
upstream mesh errors.

**Config.** `BILLING_DEPOSIT_DECIMALS` (0–18, defaults to `FAUCET_DECIMALS`)
feeds the human-readable amount rendering on the wallet page.

**Frontend.**

- `services.ts` adds `PriceTriple`, `MarketEntry`, `ServiceCatalogue`, and
  `listMeshCatalogue` (returns `{services, market}`); `listMeshServices`
  stays a backward-compatible wrapper.
- `manage-api.ts` adds `BillingState`, `BillingCaps`, `BillingLedgerEntry`,
  `BillingDepositEntry`, and the four fetch functions using the established
  `requestJson` + typed-error pattern.
- `wallet/billing-panel.tsx` (new) renders available/reserved/total credit,
  deposit instructions with copy buttons, the three-cap buyer editor (empty
  = unlimited, validated non-negative), the recent ledger, and recent
  deposits. It reuses `getAuthJwt` + `tokenManagerConfig.apiBaseUrl` like the
  faucet, and is rendered inside `wallet-view.tsx` whenever a Neon user is
  present.
- `services/services-view.tsx` renders a "Market prices" table (model,
  quoters, and a median-primary / min–max range cell per tier) beneath the
  services table, only when the catalog returns a non-empty market.

---

## 8. Step 7 — Withdrawals

Step 7 closes the seller side of the market: a seller with on-chain OTELA
credit can withdraw earnings to a wallet they control. The API debits
available credit at reserve time (a `withdraw` ledger leg, just like a usage
debit), and an off-request worker signs, broadcasts, and finalizes the SPL
transfer — or restores the credit on definitive failure.

**Credit model (Option B).** `ReserveWithdrawal` decrements `credit_raw`
immediately (not `reserved_raw`, which is the in-flight *request* reserve)
and writes a `credit_ledger` row with `source='withdraw',
leg='withdraw', ref='withdrawal:{id}'`. This keeps `ReconcileAccount`
correct and preserves the `reserved_raw <= credit_raw` invariant. On
definitive failure (`RestoreWithdrawal`), a second leg
(`source='withdraw', leg='restore'`) returns the same amount; note
`'restore'` is a valid *leg* but NOT a valid *source* (the `credit_ledger`
CHECK only allows `usage|earn|fee|deposit|withdraw|adjust`).

**Store — `internal/store/withdrawal.go`.** `ReserveWithdrawal` (idempotent
via `ON CONFLICT (idempotency_key) DO NOTHING`; when the conflict fires,
`pgx.ErrNoRows` is returned — NOT a unique violation — and the code
re-SELECTs, rejecting if the existing row belongs to a different account),
`MarkWithdrawalSigned` (only from `reserved`, persists the base64 wire,
base58 signature, blockhash, and last-valid block height),
`MarkWithdrawalBroadcast` (only from
`signed`), `FinalizeWithdrawal` (no balance change, just state + timestamp),
`RestoreWithdrawal` (returns credit, writes restore leg), `SweepWithdrawalsDue`
(`FOR UPDATE SKIP LOCKED` over active rows due for work; `finalized` rows are
excluded), and `ListWithdrawals` (cursor-paginated, newest-first). Per-row
guarded UPDATEs mean two replicas racing on the same row never double-work.

**Worker — `internal/withdraw/worker.go`.** `Service.Run(ctx)` polls
`SweepWithdrawalsDue` at `BILLING_WITHDRAW_POLL_INTERVAL` (default 5s) and
drives each row through its state machine:

- `reserved → signed`: derive a fresh blockhash and its RPC-provided
  `lastValidBlockHeight`, confirm the destination ATA
  exists (`AccountExists`, a `getAccountInfo` with an empty `dataSlice` so
  base58 never has to encode >128 bytes), and if absent prepend a
  create-ATA instruction. Build the SPL transfer message with the **same**
  account ordering and token2022 rent-sysvar omission as the faucet, sign
  the message with `ed25519.Sign`, persist the compact-u16(1)+signature wire
  via `SerializeTransaction`, and store the **base58** transaction signature
  via `solana.EncodeBase58(sig)`. The row also records
  `blockhash_expires_at` as an operator-facing approximate expiry timestamp,
  but restoration is authorized only by the chain-derived
  `last_valid_block_height`.
- `signed → broadcast`: `SendTransaction` with the **persisted** wire read
  from the DB (not the worker's own computation), so a replica that lost the
  signing race still broadcasts the canonical wire.
- `broadcast → finalized` or `broadcast → restore`: poll
  `GetSignatureStatuses`. `Err == null` && `finalized` → finalize.
  On-chain failure (`Err != null`) → restore. An unknown signature is
  restored only after `getBlockHeight` proves
  `currentBlockHeight > lastValidBlockHeight`.
  `"already processed"` / `"already known"` → leave `broadcast` (the chain
  will confirm). **Ambiguous** errors (timeout / 5xx from `SendTransaction`)
  keep the row `signed` — the transaction may have landed, so credit is
  never speculatively restored.

`RunOnce(ctx)` is unit-testable against a stub `rpcClient` and
`withdrawalStore` interface, covering every transition plus the ambiguous-
error and blockhash-expiry paths under `-race`.

**Config.** `BILLING_TREASURY_KEYPAIR` is loaded via
`nodecred.DecodeSigningKey` (accepts base64 of a 32-byte seed or 64-byte
private key) and the derived public key is verified to equal
`BILLING_TREASURY_WALLET` on boot (mismatch → load error; keypair without a
wallet → load error). `BILLING_WITHDRAW_POLL_INTERVAL` (default 5s) sets the
sweep cadence; `BILLING_WITHDRAW_BLOCKHASH_MAX_AGE` (default 90s) sets the
approximate `blockhash_expires_at` timestamp written alongside the authoritative
last-valid-block-height proof.

**Billing API — `internal/billingapi`.** Three new routes, scoped to the
owning account (resolved from `principal.UserID` or `account.ID`). The API
advertises `withdrawals_enabled` from the same condition that starts the
worker and exposes the account's `primary_linked_wallet` in billing state:

- `POST /manage/billing/withdrawals` is rejected with `503` before any debit
  when the worker is disabled. Otherwise it resolves the destination from the
  account's primary linked wallet, validates that server-owned value, and
  rejects any conflicting client destination. `Idempotency-Key` is the
  authoritative idempotency input; a matching body field is accepted as a
  compatibility fallback. `amount_raw` is a positive decimal string (legacy
  integer JSON numbers are also accepted temporarily). The endpoint pre-checks
  available credit and returns `202` with the reserved row, `402`
  insufficient, `409` conflict, or `400` invalid. The response omits
  `signed_wire`.
- `GET /manage/billing/withdrawals/{id}` returns one row; `404` if not owned.
- `GET /manage/billing/withdrawals?cursor=&limit=` newest-first, opaque
  base64 cursor over `(reserved_at, id)`.

**`main.go` wiring.** The worker is constructed only when
`cfg.BillingTreasuryKeypair != nil` (which itself requires `BILLING_MODE !=
off && BILLING_TREASURY_WALLET != ""`), started in a goroutine with a
`withdrawDone` channel and the same 2-second graceful shutdown as the
deposit watcher and settlement sweeper.

**Frontend.** `manage-api.ts` adds `BillingWithdrawal`,
`BillingWithdrawalPage`, and `BillingWithdrawalCreateInput` types plus
`createWithdrawal` (POST, surfaces 402/409/400 specifically), `listWithdrawals`,
and `getWithdrawal` (404-aware). All raw monetary values use `bigint` in the
client and decimal strings on the wire. `billing-panel.tsx` shows a withdrawal
form only when the backend capability is enabled, targets the displayed
primary linked wallet (there is no editable destination), converts the OTELA
amount via `uiAmountToRaw`, and sends a client-generated
`Idempotency-Key`. It also renders a live withdrawals list with state badges
(queued / signing /
broadcasting / finalized / failed / restored); submitting debits the balance
immediately and re-polls so the badge advances to `finalized`.

---

## 9. Configuration

| Env var | Type | Default | Purpose |
|---------|------|---------|---------|
| `BILLING_MODE` | `off` \| `observe` \| `enforce` | `off` | Master switch. `off` = zero-cost pass-through. `observe` = shadow (no rejects, no balance moves). `enforce` = production. |
| `BILLING_OUTPUT_TOKEN_MAX` | int | `0` (none) | Operator output-token ceiling. Generative routes with no body-level max and no operator max are rejected in enforce. |

The pricing API reuses the existing node-credential signing/verification
configuration (`NODE_CREDENTIAL_SIGNING_KID`, `NODE_CREDENTIAL_ISSUER`,
`NODE_CREDENTIAL_SIGNING_KEY`, `NODE_CREDENTIAL_VERIFY_KEYS`,
`OWNERSHIP_MAX_AGE`, `INTERNAL_CONTROL_TOKEN`).

The shared peer snapshot reuses `UPSTREAM_URL` and `catalogCacheTTL`.

---

## 10. Server wiring — `cmd/server/main.go`

When `BILLING_MODE != off`:

1. A shared `peers.Service` is constructed (reused by both the catalog
   handler and the billing gate).
2. A `billinggate.Service` wraps `proxy.NewWithPerfHook(...)` and becomes the
   inference proxy handler.
3. A pricing-scoped `nodecred.Verifier` is constructed with
   `PricingAudience`.
4. A `pricingapi.Service` is constructed with access to the seller's live mesh
   observation and exposed at `POST /internal/pricing`.
5. `server.NewWithBilling(...)` assembles routes: the billing gate replaces
   the raw proxy on the catch-all `/` route; the pricing handler is mounted
   at `/internal/pricing`.

When `BILLING_MODE == off`, none of this is constructed — the proxy is the
raw `proxy.NewWithPerfHook(...)`, and behavior is identical to before billing
existed.

---

## 11. Mesh-side changes — `OpenTela/src/internal/server/proxy_handler.go`

`intersectAllowedPeers(candidates, allowedHeader)` is applied in
`globalServiceForwardWithScope` immediately after `selectCandidates`,
**before** the control-plane ACL check and the retry loop:

- Keeps only candidates whose peer ID appears in the comma-separated
  `X-Otela-Allowed-Peers` header.
- **Order is preserved** — candidates are already priority-sorted, and the
  billing gate sorts cheapest-first.
- An absent header means no billing constraint (backward compatible with
  pre-billing traffic).
- No match → `503 no billing-affordable provider for the requested service.`
- On every retry, `excludePeers` runs first, then the remaining candidates
  are still within the allowed set (the header is fixed for the request's
  lifetime).

Tests: `OpenTela/src/internal/server/allowed_peers_test.go` (5 tests) — order
preservation, empty header no-op, no match, whitespace trimming, empty
candidates.

---

## 12. Test coverage

All tests pass, including `-race` on the critical packages (`store`,
`auth`, `gate`, `server`, `config`, `billinggate`, `peers`, `pricingapi`,
`perf`, `settlement`, `deposits`, `withdraw`).

| Package | Test file | Tests | Notes |
|---------|-----------|-------|-------|
| `internal/billing` | `billing_test.go` | 9 | Pure arithmetic, overflow, caps |
| `internal/store` | `billing_test.go` | 13 | DB-backed, `-race`, concurrent overdraft, stale sweep |
| `internal/store` | `deposit_test.go` | 10 | DB-backed: insert/apply idempotency, reconciliation, AccountForWallet |
| `internal/gate` | `inspect_test.go` | 16 | Body inspection, byte-for-byte restore, max-token clamp |
| `internal/auth` | `middleware_test.go` | 15 | Account threading, enforce/observe |
| `internal/config` | `config_test.go` | 13 | Billing mode, output max, fee bps, sweep, deposits, treasury keypair verify |
| `internal/server` | `server_test.go` | 19 | Proxy integration, enforce/observe |
| `internal/cache` | `cache_test.go` | — | Updated to `Cache[T]` |
| `internal/nodecred` | `jwt_test.go` | 5 | Pricing audience isolation |
| `internal/nodecred` | `pricing_api_test.go` | 6 | Pricing challenge/issue round-trip, cross-audience isolation |
| `internal/peers` | `peers_test.go` | 7 | Policy filtering, cache, `-race` |
| `internal/pricingapi` | `pricing_test.go` | 14 | Full pricing API |
| `internal/billinggate` | `gate_test.go` | 17 | Full gate, `-race` |
| `internal/perf` | `perf_test.go` | 22 | SSE/JSON parser, cached tokens, settle hook, `-race` |
| `internal/settlement` | `settle_test.go` | 7 | Settle/release/abort/idempotent, sweeper |
| `internal/solana` | `rpc_test.go` | 7 | RPC stub: signatures, getTransaction, transfers, sender attribution |
| `internal/deposits` | `deposits_test.go` | 10 | Watcher: credit linked, skip failed/non-treasury, idempotent, bounded first pass, `-race` |
| `internal/walletsapi` | `api_test.go` | 6 | Wallet link; reconciler fires after link (error does not fail link) |
| `internal/billingapi` | `api_test.go` | 22 | State/preferences/ledger/deposits, withdrawal create/get/list, cursor round-trip, JSON key guard |
| `internal/billingapi` | `api_db_test.go` | 3 | DB-backed state, ledger cursor paging, deposits (TEST_DATABASE_URL) |
| `internal/withdraw` | `worker_test.go` | 11 | reserved→signed→broadcast→finalized/restore, base58 sig, ambiguous-error handling, `-race` |
| `internal/store` | `withdrawal_test.go` | 10 | DB-backed: reserve debits + ledger, idempotent, insufficient, mark signed/broadcast/finalize/restore, sweep due, paging |
| `internal/catalog` | `catalog_test.go` | 4 | Market array: min/median/max, expired drops, error propagation |
| `OpenTela/.../account` | `services.test.ts` | 11 | Catalogue + market normalisation, off-mode empty market |
| `OpenTela/.../account` | `manage-api.test.ts` | 12 | Billing state, preferences PATCH, ledger/deposit paging, withdrawal create/get/list, 400/401/402/404/409 |
| `OpenTela/.../server` | `allowed_peers_test.go` | 5 | Peer intersection |

The billing tables are in the `newTestStore` DROP list in `postgres_test.go`
for test isolation (stale rows across runs otherwise).

---

## 13. What is explicitly not implemented

The following are part of the overall plan but are **not** in Steps 1–7:

- **Price-aware routing** in the mesh head (weighting peers by price) is
  explicitly deferred to Phase 1; the gate computes the allowed list and the
  mesh load-balances within it.

Steps 1–7 are now wired end-to-end: the gate conservatively reserves before
forwarding and settles exactly once on response; a Solana deposit watcher
credits inbound SPL transfers and reconciles them on wallet link; sellers
publish asks and buyers see market prices in the catalog; and the management
API plus wallet UI let a seller set buyer caps, deposit OTELA, and withdraw
earnings to a wallet they control — all metered by the same immutable
`credit_ledger`.

---

## File manifest

### New files
```
api.opentela.ai/migrations/0006_billing.sql
api.opentela.ai/migrations/0007_pricing_challenges.sql
api.opentela.ai/migrations/0008_deposit_cursor.sql
api.opentela.ai/migrations/0009_withdrawal_expiry.sql
api.opentela.ai/internal/billing/billing.go
api.opentela.ai/internal/billing/billing_test.go
api.opentela.ai/internal/store/billing.go
api.opentela.ai/internal/store/billing_test.go
api.opentela.ai/internal/account/account.go
api.opentela.ai/internal/gate/inspect.go
api.opentela.ai/internal/gate/inspect_test.go
api.opentela.ai/internal/peers/peers.go
api.opentela.ai/internal/peers/peers_test.go
api.opentela.ai/internal/pricingapi/pricing.go
api.opentela.ai/internal/pricingapi/pricing_test.go
api.opentela.ai/internal/billinggate/gate.go
api.opentela.ai/internal/billinggate/gate_test.go
api.opentela.ai/internal/settlement/settle.go
api.opentela.ai/internal/settlement/settle_test.go
api.opentela.ai/internal/solana/pda.go
api.opentela.ai/internal/solana/solana.go
api.opentela.ai/internal/solana/base58.go
api.opentela.ai/internal/solana/rpc.go
api.opentela.ai/internal/solana/rpc_test.go
api.opentela.ai/internal/solana/tx.go
api.opentela.ai/internal/store/deposit.go
api.opentela.ai/internal/store/deposit_test.go
api.opentela.ai/internal/deposits/deposits.go
api.opentela.ai/internal/deposits/deposits_test.go
api.opentela.ai/internal/billingapi/api.go
api.opentela.ai/internal/billingapi/api_test.go
api.opentela.ai/internal/billingapi/api_db_test.go
api.opentela.ai/internal/store/withdrawal.go
api.opentela.ai/internal/store/withdrawal_test.go
api.opentela.ai/internal/withdraw/worker.go
api.opentela.ai/internal/withdraw/worker_test.go
api.opentela.ai/docs/billing-design.md
api.opentela.ai/docs/billing-implementation.md   ← this document
OpenTela/src/internal/server/allowed_peers_test.go
```

### Modified files
```
api.opentela.ai/internal/cache/cache.go          (Cache[T any])
api.opentela.ai/internal/cache/cache_test.go
api.opentela.ai/internal/store/store.go          (KeyStore.Validate widened)
api.opentela.ai/internal/store/postgres.go       (Validate returns accountID)
api.opentela.ai/internal/store/policy.go         (Challenge audience column)
api.opentela.ai/internal/store/acl.go            (Audience field, audFor())
api.opentela.ai/internal/store/postgres_test.go  (DROP list + validate tests)
api.opentela.ai/internal/auth/validator.go       (Valid → (accountID, ok, err))
api.opentela.ai/internal/auth/validator_test.go
api.opentela.ai/internal/auth/middleware.go      (Options{EnforceAccount})
api.opentela.ai/internal/auth/middleware_test.go
api.opentela.ai/internal/config/config.go        (BillingMode, OutputMax, FeeBps, Sweep, Deposits, Treasury keypair, Withdraw tuning)
api.opentela.ai/internal/config/config_test.go
api.opentela.ai/internal/nodecred/jwt.go         (Audience, pricing constructors)
api.opentela.ai/internal/nodecred/jwt_test.go
api.opentela.ai/internal/nodecred/api.go         (pricingSigner, Pricing handlers)
api.opentela.ai/internal/nodecred/pricing_api_test.go
api.opentela.ai/internal/catalog/catalog.go      (delegates to peers.Service + market array + WithAsks)
api.opentela.ai/internal/catalog/catalog_test.go (market tests)
api.opentela.ai/internal/server/server.go        (NewWithBilling)
api.opentela.ai/internal/perf/parser.go          (cached input tokens, dialect)
api.opentela.ai/internal/perf/sample.go          (CachedInputTokens field)
api.opentela.ai/internal/perf/hook.go            (HookWithSettle, SettleUsage)
api.opentela.ai/internal/perf/perf_test.go       (cached-token + settle tests)
api.opentela.ai/internal/manageapi/router.go     (BillingRoutes interface)
api.opentela.ai/internal/manageapi/router_test.go
api.opentela.ai/internal/walletsapi/api.go        (Reconciler hook after LinkWallet)
api.opentela.ai/internal/walletsapi/api_test.go
api.opentela.ai/internal/faucet/pda.go            (delegates to internal/solana)
api.opentela.ai/internal/faucet/tx.go             (delegates to internal/solana)
api.opentela.ai/cmd/server/main.go               (billing gate + pricing + settlement + deposit watcher + withdrawal worker + billing routes)
OpenTela/src/internal/server/proxy_handler.go    (intersectAllowedPeers)
OpenTela/docs/app/account/services.ts             (market types + listMeshCatalogue)
OpenTela/docs/app/account/services.test.ts
OpenTela/docs/app/account/manage-api.ts           (billing + withdrawal types and fetch functions)
OpenTela/docs/app/account/manage-api.test.ts
OpenTela/docs/app/account/wallet/wallet-view.tsx  (renders BillingPanel)
OpenTela/docs/app/account/wallet/billing-panel.tsx (billing UI + withdraw form + withdrawals list)
OpenTela/docs/app/account/services/services-view.tsx (market prices table)
OpenTela/docs/app/global.css                       (billing + market + withdrawal styles)
.omx/plans/otela-billing-devnet-marketplace.md
```
