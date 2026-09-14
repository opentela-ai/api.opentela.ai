# OTELA Billing — Market Mechanism Design

> **Status:** Design, v3. Supersedes v2 (peer-published prices) with three-tier
> metering (regular input, cached input, output), durable idempotent request
> reservations instead of a lossy in-memory accumulator, conservative
> reservation sizing before forwarding, and an immutable quote snapshot so the
> charge never depends on a fresh read at response completion. OTELA is
> devnet-only today; the design ships on devnet and evolves to mainnet
> non-custody without a rewrite.
>
> **Changelog (v2 → v3):**
> - Asks carry three rates (`input_per_million`, `cached_input_per_million`,
>   `output_per_million`), not two. Cache creation bills at the regular rate;
>   cache reads bill at the cached rate.
> - The request model is parsed from the **body** (`splitServiceRoute` returns
>   only service and route); a bounded inspector extracts service, model,
>   streaming mode, and an output-token ceiling while restoring the body
>   byte-for-byte.
> - Buyer caps are three separate dimensions, each overridable per request;
>   a peer is affordable only when **all three** rates are within their caps.
> - The gate **reserves a conservative maximum cost before forwarding** instead
>   of checking only `credit_raw > 0`, and intersects an API-computed
>   `X-Otela-Allowed-Peers` list with the mesh's candidates before load
>   balancing and on every retry (`OpenTela/src/internal/server/proxy_handler.go`).
> - Settlement is a durable, idempotent row-locked transaction keyed on an
>   immutable quote snapshot — not a 1Hz in-memory accumulator.
> - All off-chain credit is one fungible, withdrawable balance. The UI may
>   report lifetime earnings but does not claim a separately knowable
>   "earnings balance."
> - A dedicated treasury is used from day one; the fee is zero and no signup
>   credit is granted. Hard cap enforcement is included in Phase 0;
>   price-aware weighting and non-custodial settlement remain later work.

## 1. Goal & the core decision

**Goal.** Charge OTELA for inference routed through `/v1/*` — **without
OpenTela deciding the price.** Price is discovered by the market: node
operators (sellers) publish ask prices; users (buyers) express how much
they'll pay; the router matches them. OpenTela is the **clearinghouse**, not
the pricer.

This is a two-sided market:
- **Sellers** are node operators. They have GPUs, run inference, set their own
  price per (service, model) in OTELA per 1M tokens, split three ways. They
  earn what buyers pay.
- **Buyers** are API key holders. They deposit OTELA, get off-chain credit,
  and may set a maximum willingness-to-pay per 1M tokens on each of the three
  tiers.
- **OpenTela** routes requests, meters usage, settles (debits buyer, credits
  seller, optional fee), and never sets the price of inference.

**Why off-chain ledger, not per-request on-chain:** an on-chain Solana transfer
per request adds 400ms–2s to the hot path, the API-key flow has no wallet in
the loop to sign, and a peer-set price changes continuously. The market runs
off-chain (peer asks in Postgres, settlement in the ledger) with on-chain
movement only for deposits and withdrawals. §11 adds non-custodial on-chain
settlement using the **same** meter, gate, and ledger.

## 2. What's reused from the existing code

Five pieces are already built — the design slots into them:

| Already exists | Where | Role in the market |
|---|---|---|
| Per-response usage parser | `internal/perf/parser.go` (`inputTokens`, `outputTokens`, `model`) | The meter — extended to three tiers (§5) |
| `X-Computing-Node` + `instances.(peer_id, account_id, owner_wallet)` | `internal/perf/hook.go`, `internal/store/acl.go` | Resolves **who served** → **which seller account to credit** |
| Node credential auth | `internal/nodecred/` (Signer, Verifier, challenges) | Peers authenticate to **publish their ask prices**, under a distinct pricing audience/scope |
| SPL transfer machinery | `internal/faucet/tx.go`, `rpc.go`, `pda.go` | Reused for deposits and withdrawals; §7 adds deposit discovery and transfer decoding |
| Catalog (`/v1/services`) | `internal/catalog/catalog.go` (`Summarise`, per-peer `Service`) | Becomes the **public marketplace view** (§3.4) — additive, never breaking |

**The missing links (Step 2):**
- `auth.Middleware` validates a key but throws the `user_id` away. Widen
  `TokenValidator` to return the owning account ID and set it in request
  context, so the gate and metering hook know **which buyer to charge**.
  Active legacy keys with no `user_id` are rejected with `402
  billing_account_required` while enforcement is on.
- The route only gives the **service**, not the **model**. A bounded
  request-body inspector extracts service, model, streaming mode, and an
  output-token ceiling while restoring the body byte-for-byte (§6.1).

## 3. The market: asks, bids, and matching

### 3.1 Sellers publish asks (peer-set, no central authority)

Each peer publishes an ask price for each (service, model) it serves, in OTELA
base units (9 decimals) per 1,000,000 tokens, split three ways:

```json
{
  "service": "llm",
  "model": "llama3.1-70b",
  "input_per_million": 1200,
  "cached_input_per_million": 300,
  "output_per_million": 3600
}
```

`POST /internal/pricing` is authenticated by a **pricing-scoped** node
credential and performs a **full replacement** of that peer's asks. The server
assigns a **five-minute** expiry; the node republishes every two minutes. An
empty body clears all of the peer's asks. Duplicates, negative or overflowing
rates, unknown services/models, and more than 256 entries are rejected. A
pricing credential uses a distinct audience/scope so expanding seller
eligibility cannot weaken the trusted-region ACL credential. A seller must own
an **active registered instance whose observed wallet still matches** before
its asks are accepted.

Asks are stored in `peer_asks`, keyed by `(peer_id, service, model)` with a
`revision` bumped to a fresh value across each replacement, so a snapshot taken
at the gate can detect that the ask moved by settlement. Peers that serve but
never publish are eligible at a **zero quote** — free to the buyer, nothing to
the seller, the incentive to publish.

### 3.2 Buyers express a max bid (three dimensions, optional)

A buyer can set a maximum willingness-to-pay per 1M tokens on each of the three
tiers:
- **Per-account**, stored as nullable `max_input_per_million`,
  `max_cached_input_per_million`, `max_output_per_million` on `account_credits`
  (NULL = no limit on that dimension).
- **Per-request**, via headers `X-Max-Input-Price-Per-Million`,
  `X-Max-Cached-Input-Price-Per-Million`, `X-Max-Output-Price-Per-Million`.
  Each supplied dimension overrides the account default for that one request.

A peer is affordable only when **all three** rates are within their
corresponding caps. The API overwrites any client-supplied
`X-Otela-Allowed-Peers` and forwards its computed list to the mesh head; the
mesh intersects this list with model, ACL, trust, and health candidates before
load balancing **and on every retry** in `OpenTela/src/internal/server/proxy_handler.go`.
The list is capped at the **128 cheapest affordable peers** to bound header
size. If even the cheapest affordable ask is above the buyer's max on any
dimension → `402 price_above_max` (no GPU work happens).

### 3.3 Matching (routing)

Today the proxy forwards to the mesh head (`cfg.UpstreamURL`), which selects a
peer by load/GPU. The market evolves this in two steps; settlement is correct
from day one because it always charges the **served peer's snapshotted ask**:

1. **Phase 0 — hard caps + allowed-peers filter.** The gate resolves the
   affordable live peers for the requested model, snapshots each one's seller
   account, ask revision, and three rates, reserves a conservative maximum
   cost, persists the request, and forwards the 128 cheapest affordable peers
   to the mesh head. Only peers the buyer can afford reach a worker; the gate
   rejects unsupported inference routes with `400 billing_unsupported_route`.
2. **Mainnet — price-aware routing (deferred).** The mesh head reads
   `peer_asks` directly, prefers the cheapest live peer weighted by load and
   reliability, and respects the buyer's caps. The API stops needing the
   allowed-peers filter.

Settlement depends on the **served peer's published ask, snapshotted at the
gate** — never on a price OpenTela sets, and never on a fresh read at response
completion. Only *which peer gets picked* becomes price-aware later; the ledger
never depends on OpenTela pricing inference.

### 3.4 The public catalog becomes a market view (additive)

`/v1/services.models` keeps its existing string array so current consumers are
unaffected. A sibling `market` array adds per-model provider count and
min/median/max triples across the three tiers:

```json
{ "name": "llm", "models": [{
    "id": "llama3.1-70b",
    "providers": 4,
    "online": 3,
    "input_per_million":    {"min": 800, "median": 1200, "max": 1800},
    "cached_input_per_million": {"min": 200, "median": 300, "max": 450},
    "output_per_million":   {"min": 2400, "median": 3600, "max": 5400}
}]}
```

Min/median/max are aggregated across live peers **without** revealing which
peer asks what (the catalog already anonymizes peer identity via `Summarise`;
that boundary is preserved). Median aggregation is deterministic.

## 4. Data model (new migration `0006_billing.sql`)

```sql
-- Peer-published asks: the market. Three rates + a revision bumped across each
-- replacement + server-assigned expiry. OpenTela never writes here except to
-- clear expired/stale rows and enforce structural limits.
CREATE TABLE IF NOT EXISTS peer_asks (
    peer_id                   TEXT NOT NULL,
    service                   TEXT NOT NULL,
    model                     TEXT NOT NULL,
    input_per_million         BIGINT NOT NULL,
    cached_input_per_million  BIGINT NOT NULL,
    output_per_million        BIGINT NOT NULL,
    revision                  BIGINT NOT NULL DEFAULT 1,
    expires_at                TIMESTAMPTZ NOT NULL,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (peer_id, service, model),
    CONSTRAINT peer_asks_rates_nonneg_chk CHECK (input_per_million >= 0 AND
        cached_input_per_million >= 0 AND output_per_million >= 0),
    CONSTRAINT peer_asks_rates_bound_chk  CHECK (input_per_million < 1000000000000
        AND cached_input_per_million < 1000000000000 AND output_per_million < 1000000000000)
);

-- One fungible, withdrawable API-credit balance per account. reserved_raw is
-- the sum of in-flight request reservations and is a part of (not in addition
-- to) credit_raw, so the spendable balance is always credit_raw - reserved_raw
-- with the invariant 0 <= reserved_raw <= credit_raw.
CREATE TABLE IF NOT EXISTS account_credits (
    account_id                   TEXT PRIMARY KEY,
    credit_raw                   BIGINT NOT NULL DEFAULT 0,
    reserved_raw                 BIGINT NOT NULL DEFAULT 0,
    max_input_per_million        BIGINT,
    max_cached_input_per_million BIGINT,
    max_output_per_million       BIGINT,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_credits_reserved_chk CHECK (reserved_raw <= credit_raw
        AND credit_raw >= 0 AND reserved_raw >= 0),
    CONSTRAINT account_credits_caps_nonneg_chk CHECK (
        (max_input_per_million IS NULL OR max_input_per_million >= 0)
        AND (max_cached_input_per_million IS NULL OR max_cached_input_per_million >= 0)
        AND (max_output_per_million IS NULL OR max_output_per_million >= 0))
);

-- The durable, idempotent reservation and settlement record. eligible_peers is
-- the immutable quote snapshot taken at the gate; settlement resolves
-- X-Computing-Node against it and prices at it, never at a fresh read.
CREATE TABLE IF NOT EXISTS billing_requests (
    request_id                   TEXT PRIMARY KEY,
    buyer_account_id             TEXT NOT NULL,
    service                      TEXT NOT NULL,
    model                        TEXT NOT NULL,
    eligible_peers               JSONB NOT NULL,
    max_input_per_million        BIGINT,  -- effective caps at the gate
    max_cached_input_per_million BIGINT,
    max_output_per_million       BIGINT,
    reserved_raw                 BIGINT NOT NULL,
    state                        TEXT NOT NULL DEFAULT 'reserved',  -- reserved|settled|released
    served_peer_id               TEXT,
    served_seller_account_id     TEXT,
    served_revision              BIGINT,
    served_input_per_million     BIGINT,
    served_cached_input_per_million BIGINT,
    served_output_per_million    BIGINT,
    input_tokens                 INT,
    cached_input_tokens          INT,
    output_tokens                INT,
    cost_raw                     BIGINT,
    fee_raw                      BIGINT,
    seller_raw                   BIGINT,
    reserved_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at                   TIMESTAMPTZ,
    released_at                  TIMESTAMPTZ,
    release_reason               TEXT,
    CONSTRAINT billing_requests_state_chk CHECK (state IN ('reserved','settled','released'))
);

-- Immutable balance movements. UNIQUE(ref, leg) makes the whole system
-- exactly-once: a retried settlement replays the same (request_id, leg) and is
-- rejected rather than double-charging.
CREATE TABLE IF NOT EXISTS credit_ledger (
    id                          BIGSERIAL PRIMARY KEY,
    account_id                  TEXT NOT NULL,
    delta_raw                   BIGINT NOT NULL,
    source                      TEXT NOT NULL,  -- usage|earn|fee|deposit|withdraw|adjust
    leg                         TEXT NOT NULL,   -- buyer|seller|fee|deposit|withdraw|adjustment|...
    counterparty                TEXT,
    ref                         TEXT,            -- request id (usage/earn) or tx signature (deposit/withdraw)
    model                       TEXT,
    input_per_million           BIGINT,
    cached_input_per_million    BIGINT,
    output_per_million          BIGINT,
    input_tokens                INT,
    cached_input_tokens         INT,
    output_tokens               INT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT credit_ledger_source_chk CHECK (source IN
        ('usage','earn','fee','deposit','withdraw','adjust')),
    CONSTRAINT credit_ledger_unique UNIQUE (ref, leg)
);

-- One row per (transaction_signature, instruction_index) of an inbound SPL
-- transfer into the treasury ATA. Persisted BEFORE credit is applied.
CREATE TABLE IF NOT EXISTS deposit_events (
    transaction_signature   TEXT NOT NULL,
    instruction_index       INT NOT NULL,
    slot                    BIGINT NOT NULL,
    from_wallet             TEXT NOT NULL,
    amount_raw              BIGINT NOT NULL,
    assigned_account_id     TEXT,                 -- NULL until the wallet is linked
    assignment_state        TEXT NOT NULL DEFAULT 'unassigned',  -- unassigned|assigned|skipped
    credited_at             TIMESTAMPTZ,
    seen_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transaction_signature, instruction_index),
    CONSTRAINT deposit_events_state_chk CHECK (assignment_state IN ('unassigned','assigned','skipped'))
);

-- Durable per-treasury high-water mark. The watcher advances it only after a
-- contiguous oldest-to-newest prefix has processed successfully.
CREATE TABLE IF NOT EXISTS deposit_cursors (
    treasury_ata   TEXT PRIMARY KEY,
    last_signature TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Withdrawal state machine: reserved -> signed -> broadcast -> finalized.
CREATE TABLE IF NOT EXISTS withdrawals (
    id                   BIGSERIAL PRIMARY KEY,
    account_id           TEXT NOT NULL,
    idempotency_key      TEXT NOT NULL UNIQUE,
    destination_wallet   TEXT NOT NULL,
    amount_raw           BIGINT NOT NULL,
    state                TEXT NOT NULL DEFAULT 'reserved',
    signed_wire          TEXT,
    signature            TEXT,
    blockhash            TEXT,
    last_valid_block_height BIGINT,
    blockhash_expires_at TIMESTAMPTZ,
    error                TEXT,
    reserved_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    signed_at            TIMESTAMPTZ,
    broadcast_at         TIMESTAMPTZ,
    finalized_at         TIMESTAMPTZ,
    CONSTRAINT withdrawals_state_chk CHECK (state IN
        ('reserved','signed','broadcast','finalized','failed','restored'))
);
```

### 4.1 Why one balance, not separate buyer/seller tables

A node operator who earns OTELA and then buys inference with it shouldn't need
an on-chain round-trip (withdraw, then deposit). One balance makes earnings
immediately spendable — an internal zero-sum ledger move, no new credit, no
on-chain tx. The `source` field on each ledger entry distinguishes deposits
from earnings for accounting, without forcing two balances. **There is no
separately knowable "earnings balance"**: `account_credits.credit_raw` is the
single fungible, withdrawable API-credit balance. The UI may report lifetime
earnings (`SUM(delta_raw) WHERE source='earn'`) but never as a spendable
balance distinct from `credit_raw`.

`account_credits` is a **transactional projection**. Reconciliation
(`ReconcileAccount`) verifies it against `SUM(credit_ledger.delta_raw)` and the
open reservation sum (`SUM(billing_requests.reserved_raw) WHERE state='reserved'`),
and surfaces any drift or invariant violation (`0 <= reserved_raw <= credit_raw`).

## 5. Metering semantics

Extend the existing parser in `internal/perf/parser.go` with `cachedInputTokens`
and complete-usage detection.

- **OpenAI:** cached input comes from `cached_tokens`; regular input is
  aggregate input minus cached input, leaving cache-write tokens at the
  regular-input rate. Streaming Chat Completions requests must include
  `stream_options.include_usage=true`. See the [OpenAI prompt-caching
  documentation](https://developers.openai.com/api/docs/guides/prompt-caching).
- **Anthropic:** regular input is `input_tokens + cache_creation_input_tokens`;
  cached input is `cache_read_input_tokens`. See the [Anthropic prompt-caching
  documentation](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching).

Charge only successful 2xx responses with a **complete, authoritative usage
record**. On client abort, malformed usage, or missing final usage, release the
reservation without charging and emit an alerting metric. Phase 0 enforcement
covers token-metered Chat Completions, Completions, Responses, Messages, and
Embeddings shapes; reject unsupported inference routes with `400
billing_unsupported_route` while enforcement is active.

Cost uses **checked integer arithmetic** and rounds the combined charge upward
to the nearest base unit:

```text
cost_raw = ceil(
  regular_input_tokens * input_per_million
  + cached_input_tokens * cached_input_per_million
  + output_tokens       * output_per_million
  / 1,000,000
)
fee_raw   = floor(cost_raw * fee_bps / 10,000)
seller_raw = cost_raw - fee_raw
```

The conservative reserve before forwarding is the **largest possible charge**
across the affordable peers — each quote priced at `max(input_rate,
cached_rate) * input_ceil + output_rate * output_ceil` — so reserving that
amount guarantees no in-flight request can overdraft regardless of which peer
serves or how input splits between regular and cached.

## 6. Hot path: gate → meter → settle

### 6.1 Threading account identity and parsing the request

Widen `auth.TokenValidator` to return the owning account ID and set it in
request context (reuse the `principal` package's key or add a billing-specific
one). In enforcement mode, reject active legacy keys with no `user_id` using
`402 billing_account_required`. Add a bounded request-body inspector that
extracts service, model, streaming mode, and an output-token ceiling while
restoring the body byte-for-byte. Require or inject a configured output-token
maximum for generative routes; use request-body byte length as the conservative
input-token ceiling, and reject route/media types for which that bound is not
valid (`400 billing_unsupported_route`).

### 6.2 The gate (pre-request, fast, never serves a paid request free)

1. Resolve the requested model from the body and the affordable live peers
   (caps from `account_credits` + per-request headers, intersected with model,
   ACL, trust, and health candidates). Cap the list at the 128 cheapest
   affordable peers.
2. Snapshot each eligible peer's seller account, ask revision, and three rates
   into `billing_requests.eligible_peers`.
3. Compute the conservative reserve and, under a row lock on the buyer's
   `account_credits`, check `credit_raw - reserved_raw >= reserve` and bump
   `reserved_raw`. Return `402 insufficient_credit` otherwise.
4. Forward the allowed-peers list to the mesh head (`X-Otela-Allowed-Peers`),
   intersected again on every retry.

Caches may accelerate ask and catalog **reads** but a cached read must never
authorize or move a balance — only the row-locked transaction in
`ReserveBilling` does.

### 6.3 The settlement (post-response, exact, durable)

Inside the response hook, after the body stream ends (the existing hook already
waits for body end to emit a `perf.Sample`):

1. Share the parsed usage (`model`, `inputTokens`, `cachedInputTokens`,
   `outputTokens`) with billing while preserving the existing performance
   sample — no second parse.
2. Resolve `X-Computing-Node` only against the request's **immutable quote
   snapshot**. An unknown peer releases the reservation (`release_reason =
   "unknown_peer"`) and is never priced at a fresh read.
3. Compute `cost_raw` (checked, ceiling). A zero charge (priced peer with no
   tokens, or unpriced peer) releases the reservation rather than recording an
   empty charge.
4. In **one idempotent transaction**: finalize the request (`state='settled'`),
   reduce the buyer's credit and reservation by `cost_raw` and `reserved_raw`
   respectively, credit the snapshotted seller `seller_raw`, credit the
   treasury `fee_raw` when non-zero, and insert the unique `usage`/`earn`/`fee`
   ledger legs. `UNIQUE(ref, leg)` makes a replayed settlement a no-op.
5. Release the reservation for non-2xx, missing usage, unsupported peer,
   zero-price, or unpriced-peer responses.
6. A recovery worker reclaims stale `reserved` requests after confirming they
   can no longer settle. There is no in-memory-only charge queue.

## 7. Deposits: on-chain OTELA → off-chain credit

A background **deposit watcher** (Step 5) polls `getSignaturesForAddress` and
`getTransaction` on the treasury ATA, reusing the faucet's RPC client with new
transfer-decoding primitives. For each inbound SPL transfer instruction:

1. Persist **one row per `(transaction_signature, instruction_index)`** in
   `deposit_events` BEFORE applying any credit (idempotent on the PK).
2. Credit only **finalized, successful** transfers of the **configured mint**
   into the dedicated treasury ATA. Skip wrong mint/destination.
3. Attribute from **linked wallets**: the sending wallet in `user_wallets`
   → credit that `account_id`. Unknown-wallet transfers stay `unassigned` and
   are reconciled automatically after the wallet is linked
   (`ReconcileDepositsForWallet`). **Account IDs are never accepted from
   untrusted memos.**

Each credited instruction moves the ledger exactly once
(`credit_ledger(ref = "tx_sig:idx", leg = "deposit")`).

## 8. Withdrawals: off-chain earnings → on-chain OTELA

Withdrawal is the mirror of the deposit flow and follows a durable state
machine `reserved → signed → broadcast → finalized`, reusing the faucet's SPL
transfer construction:

1. Seller requests `POST /manage/billing/withdrawals` (requires an
   `Idempotency-Key`). The destination is the seller's **primary linked
   wallet**.
2. Gate: `account_credits.credit_raw - reserved_raw >= amount`. Reserve the
   withdrawal (decrement `credit_raw`, insert a `withdraw` ledger row, persist
   the idempotency key).
3. Persist the **signed wire transaction and its deterministic signature before
   broadcast** (`state='signed'`).
4. Broadcast (`state='broadcast'`). On confirmation, `finalized`. On an
   **ambiguous RPC result**, keep funds reserved and query transaction status;
   restore credit only **after the RPC-provided last-valid block height and a
   later `getBlockHeight` prove the transaction cannot land**
   (`state='restored'`).

**Security note.** Unlike deposits (receive-only, no key), withdrawals need a
private key capable of spending the treasury — the same custody concern as the
faucet. For the devnet MVP, a dedicated treasury key with a modest balance is
acceptable. For mainnet, move the treasury key to an HSM or threshold signer,
or — preferably — adopt the non-custodial model (§11) which eliminates the
custodial withdrawal entirely.

## 9. What the user sees

- **Wallet page:** on-chain OTELA balance **and** off-chain API credit (a
  single, fungible, withdrawable balance). For node operators, lifetime
  earnings are shown as a derived report — **not** a separately spendable
  "earnings balance."
- **Catalog** (`/v1/services`): the additive `market` array (§3.4) — per-model
  min/median/max across the three tiers, peer identities hidden.
- **Three buyer-cap controls** on the service catalog: max input, max cached
  input, max output per 1M tokens.
- **Deposit:** build a transfer transaction from the connected linked wallet to
  the treasury ATA and poll billing state until finalized; surface pending
  deposits.
- **Withdraw:** "Withdraw" sends on-chain OTELA to the primary linked wallet;
  surface withdrawal state (`reserved`/`signed`/`broadcast`/`finalized` and,
  when applicable, `failed`/`restored`).
- **Usage panel:** from `credit_ledger`, a paginated log of recent charges
  (model, tokens, cost, which peer served) and earnings, via
  `GET /manage/billing/ledger?cursor=...`.
- **Errors:** `402 insufficient_credit` ("Out of OTELA credit — deposit to
  continue."), `402 price_above_max` ("No provider at or below your max price —
  raise it or wait."), `402 billing_account_required`, and `503
  billing_unavailable` surfaced distinctly.

Management APIs:
- `GET /manage/billing`
- `PATCH /manage/billing/preferences`
- `GET /manage/billing/ledger?cursor=...`
- `GET /manage/billing/deposits?cursor=...`
- `POST /manage/billing/withdrawals` (requires `Idempotency-Key`)
- `GET /manage/billing/withdrawals?cursor=...`
- `GET /manage/billing/withdrawals/{id}`
- `GET /manage/billing/asks` — the seller pricing surface: per owned
  instance, the live asks (three rates, revision, `expires_at`), the
  advertised routes with no live ask (`unpriced_routes`, i.e. currently
  eligible at a zero quote), billable/online/advertisement freshness flags,
  and the out-of-band publication contract (`ask_ttl_seconds`,
  `republish_seconds`, `publication_endpoint`).

All raw monetary values on these JSON APIs are decimal strings, including
balances, ledger deltas, deposits, and withdrawal amounts. This is a wire
contract: JavaScript clients must be able to preserve the full signed 64-bit
range without passing through `number`.

## 10. Fees (OpenTela's revenue, optional)

`BILLING_FEE_BPS` (default 0 for the devnet). When non-zero, each settlement
splits `cost_raw` into `seller_raw = cost_raw - floor(cost_raw * fee_bps /
10_000)` and `fee_raw`, credited to the reserved `account_id = 'treasury'` row
and withdrawable by OpenTela's operator. **Setting the fee is the only pricing
OpenTela does** — a take-rate on a market price, not a price of inference.

## 11. Phase 2 design: non-custodial settlement (decided)

When OTELA has real value, remove the custodial treasury from the critical
path. The market mechanism (asks, matching, the meter, the ledger) is
**unchanged**; only the settlement rails change. This section records the
decided design (tracking issue #6).

### 11.1 Primitive decision: SPL token delegation

Three candidates were considered:

| Candidate | Verdict | Why |
|---|---|---|
| **SPL token delegation (`approve` → settlement authority)** | **Chosen** | A native SPL instruction (no custom program, no upgrade authority to audit); revocable at any time by re-`approve`ing `0`; the allowance cap is enforced by the token program itself, so the buyer's maximum exposure is the allowance, never their balance. Maps 1:1 onto the existing reserve model: remaining allowance ≈ `credit_raw − reserved_raw`. |
| Per-epoch escrow PDA | Rejected | Requires a custom program to release/draw partially (native SPL cannot), i.e. the audit surface we deferred Phase 2 to avoid; locks funds for a full epoch; refunds need another program path. |
| Session keys / limited spenders | Rejected as the rail | Useful later as a UX layer (batched micro-approvals) but they still need a settlement program to enforce per-request limits on-chain; nothing in the SPL token program enforces spend shapes. |

Delegation keeps the trust model strictly better than custody: the platform
holds a **delegated spending right up to a buyer-chosen cap**, not the
funds. Buyers can revoke instantly, and a compromised settlement key
exposes at most the sum of outstanding allowances — bounded, detectable
(allowance drift vs ledger), and smaller than a hot treasury.

### 11.2 On-chain accounts

| Account | Owner / program | Role |
|---|---|---|
| Buyer OTELA ATA | SPL Token | Source of settlement funds; buyer keeps custody. |
| Seller OTELA ATA | SPL Token | Settlement destination (payouts go peer-to-peer). |
| Settlement authority | ed25519 keypair held by the settlement worker (HSM/KMS in production) | The **delegate** named by every buyer's `approve`; signs batched `transfer_checked` instructions from buyer ATAs. Distinct from the treasury keypair (§13) so custodial and non-custodial rails never share a signer. |
| Allowance registry (off-chain) | Postgres | `(buyer_account, delegate, allowance_raw, approved_at, revoked_at)` mirroring the chain; the gate's reserve check consults it instead of `credit_raw`. |

Settlement uses `transfer_checked` (mint-decimals-checked) — never raw
`transfer` — so a mint mismatch fails loudly. Each batched transfer carries
the idempotency ref of the ledger legs it settles (same `UNIQUE(ref, leg)`
exactly-once model as §6), making on-chain settlement a replay of the
already-exact off-chain ledger, not a second accounting system.

### 11.3 Gate semantics under delegation

- The conservative reserve (§5) is unchanged, but bounded by
  `min(credit_raw, remaining_allowance)` instead of `credit_raw`.
- A buyer whose allowance is exhausted mid-epoch fails like an
  insufficient-balance buyer today (`enforce`) / degrades to unpriced-free
  behavior only per `BILLING_REQUIRE_PRICED_PEER` policy.
- Freshness: the settlement worker re-reads on-chain allowances every
  `BILLING_ALLOWANCE_REFRESH` (new flag, default `60s`); a chain-side
  revocation takes effect within that window, the same staleness class as
  the deposit watcher's poll cadence.
- **Consumption-aware mirroring (implementation note):** the worker's own
  `transfer_checked`s also lower the on-chain delegated amount. Those
  observations must NOT debit credit (the charge was already debited at
  settle time); only buyer-initiated revocations clamp the credit
  projection. Until the worker ships, the poller treats every observed
  decrease as a revocation — correct today because no worker consumption
  exists; increment 2 makes the poller subtract worker-attributed
  transfers before interpreting a drop as a revoke.

### 11.4 Migration and coexistence with the custodial ledger

1. **Ledger stays the source of truth for accounting.** `credit_ledger`
   rows, caps, metering, and the manage API are identical in both modes;
   only how a credit balance is *backed on-chain* differs.
2. **Funding source becomes a per-account attribute**: `funding =
   'deposit'` (today) or `'delegation'`. A buyer may hold both; the gate
   bounds spend by the sum of available backing.
3. **Deposits become optional once delegation ships**: a buyer can go
   straight to `approve` + spend; the deposit path remains for buyers who
   prefer prepaying without granting any delegation.
4. **Withdrawals** keep their current reserve/confirm flow for custodial
   balances; delegation-backed spend never touches the treasury, so there
   is nothing to withdraw from that rail.
5. **Cutover is per-account and reversible**: flipping `funding` requires
   only the buyer's on-chain `approve`/`revoke`; no migration touches
   historical ledger rows.

### 11.5 Flow summary

1. **One-time delegation.** Each buyer signs an SPL `approve` naming the
   settlement authority as delegate of their OTELA ATA, up to a cap.
2. **Off-chain metering is identical** (§5). The ledger debits the buyer and
   credits the seller exactly as before.
3. **Periodic on-chain settlement.** The settlement worker sends batched
   `transfer_checked`s from buyers' ATAs to sellers' ATAs within the
   delegated allowances — directly, peer to peer, never through a treasury
   the API controls.
4. **Netting** within a batch reduces on-chain load.
5. **Reconciliation:** a periodic Merkle root of the ledger lets users
   verify their balance independently; allowance registry vs on-chain
   delegate amounts is cross-checked on the same cadence.

   Implemented in `internal/billing/merkle.go` + `internal/delegations`
   `Reconciler`, surfaced at `GET /manage/billing/reconciliation` and
   `GET /manage/billing/merkle-proof?leaf_id=N`:

   - **Merkle commitment.** Every `credit_ledger` row hashes to a leaf
     (`SHA-256` over a length-prefixed, unambiguous encoding of id, delta,
     source, leg, counterparty, ref, timestamp — domains
     `otela-ledger-leaf:v1` / `otela-merkle-node:v1` /
     `otela-merkle-pad:v1`), padded to a power of two with a fixed pad hash.
     The root plus leaf count is the account's balance commitment; a user
     holding their ledger copy (the ledger endpoint serves every row)
     recomputes the root and compares. Row-level inclusion proofs are
     served per leaf (`billing.MerklePath` / `VerifyMerklePath`), so a
     single charge can be proven against the published root without
     trusting the API. Proof construction is O(n) per request (the console
     caches roots); roots are deterministic, so any recomputation must
     match byte-for-byte.
   - **Registry-vs-chain cross-check.** For each registry row, expected
     chain amount = `allowance_raw − in-flight` (in-flight = consumed at
     claim but not yet finalized); the chain's `getAccountInfo` delegation
     read is compared and divergence reported per delegate. The poller
     (§11.3) *corrects* drift continuously; this *measures and publishes*
     it. An RPC failure degrades only the observed/divergence fields to
     null — the report never lies about what it could not see.
   - **Residual exposure.** Settlements that terminally failed on-chain
     (`restored`/`failed`) are summed per account: charges the platform
     credited to sellers but could not collect from the buyer's ATA. This
     is the §16 step-14 residual the worker deliberately does not hide.
   - **Cadence.** The endpoints are pure reads (plus one RPC round trip
     per account); a cron or the console drives them. The ledger integrity
     half (credit vs `SUM(ledger)`, §5's `ReconcileAccount`) is served even
     without a settlement authority configured — delegation-dependent
     sections appear once `BILLING_SETTLEMENT_AUTHORITY` is set.

The custodial MVP (§3–§9) reaches this with **no changes** to the gate,
meter, asks, or ledger — only the deposit/withdrawal rails (§7, §8) gain a
delegation-backed alternative.

## 12. Failure modes

| Failure | Effect | Handling |
|---|---|---|
| Peer served but has no ask | No charge | Release reservation (`release_reason = "unpriced_peer"`) |
| `X-Computing-Node` names a peer not in the snapshot | No charge | Release (`release_reason = "unknown_peer"`) |
| Ask changed between gate and settlement | Charge uses the snapshot, not the new rate | Immutable `eligible_peers` + `served_revision` |
| API restarts mid-request | Reservation still reserved | Row-locked in Postgres; recovery worker reclaims after the request can no longer settle |
| Settlement retried | Exactly-once | `UNIQUE(ref, leg)` rejects the duplicate leg |
| Buyer credit corrupted vs ledger | Drift detected | `ReconcileAccount` surfaces `DriftCredit` |
| `reserved_raw` exceeds `credit_raw` | Invariant violated | `CHECK` constraint + `ReconcileAccount.InvariantHeld` |
| Deposit seen, account not linked | Held as `unassigned` | `ReconcileDepositsForWallet` credits after linking |
| Withdrawal broadcast, ambiguous result | Funds remain reserved | Query tx status; restore only after blockhash expiry |

## 13. Configuration

| Variable | Default | Purpose |
|---|---|---|
| `BILLING_MODE` | `observe` | `off` (no billing), `observe` (gate+meter, no rejects), `enforce` (reject on insufficient/caps) |
| `BILLING_FEE_BPS` | `0` | OpenTela take-rate in basis points |
| `BILLING_ASK_TTL` | `5m` | Ask expiry; nodes republish every `BILLING_ASK_REPUBLISH` (default `2m`) |
| `BILLING_RESERVE_TTL` | `30m` | After this, a `reserved` request is reclaimable |
| `BILLING_TREASURY_WALLET` | — | On-chain treasury ATA the deposit watcher polls |
| `BILLING_TREASURY_KEYPAIR` | — | Signs withdrawals; **verify it owns `BILLING_TREASURY_WALLET` on boot** |
| `BILLING_MINT` | — | The OTELA SPL mint; deposits of any other mint are skipped |
| `BILLING_TOKEN_PROGRAM` | Tokenkeg… | SPL token program id |
| `BILLING_RPC_URL` | — | Solana RPC for deposits/withdrawals (reuses faucet client) |
| `BILLING_OUTPUT_TOKEN_MAX` | per route | Conservative output-token ceiling for generative routes |
| `BILLING_POLL_INTERVAL` | `2s` | Deposit/withdrawal poll cadence |
| `BILLING_REQUIRE_PRICED_PEER` | `false` | When true, a peer with no live ask is excluded from the affordable set for any `(service, model)` that has at least one published ask, instead of being eligible at a zero quote (free). Routes with no live asks keep the original behavior so a cold market still boots. |
| `BILLING_SETTLEMENT_AUTHORITY` | — | The settlement authority's base58 pubkey — the delegate buyers approve (§11.2). Set enables the allowance poller (Phase 2); unset keeps the deposit rail as the only backing. |
| `BILLING_ALLOWANCE_REFRESH` | `60s` | Allowance poller cadence; a chain-side revoke bounds spend within one window (§11.3). |
| `BILLING_SETTLEMENT_KEYPAIR` | — | Signs settlement batches as the on-chain delegate; MUST equal `BILLING_SETTLEMENT_AUTHORITY` (verified on boot). Also pays tx fees — the authority must hold SOL. |
| `BILLING_SETTLEMENT_POLL_INTERVAL` | `10s` | Settlement worker sweep cadence. |

`BILLING_MODE = enforce` is the production posture. `observe` runs the gate and
meter without rejecting requests, so the operator can validate pricing and
usage against real traffic before flipping the switch.

## 14. Phasing

- **Phase 0 — custodial market on devnet (this design):** peer asks, three-tier
  metering, hard-cap gate with conservative reserve and allowed-peers filter,
  durable idempotent settlement, deposits, withdrawals, fee wiring, management
  API. Price-aware peer weighting is **not** included; the API computes the
  allowed list and the mesh load-balances within it.
- **Phase 1 — market signal:** price-aware routing in the mesh head, catalog
  market view, buyer-cap UI.
- **Phase 2 — non-custodial settlement:** delegated SPL spending replaces the
  treasury (§11).

## 15. Open questions (resolved)

- *Separate buyer/seller balances?* **No** — one fungible, withdrawable
  API-credit balance; earnings are a derived report.
- *Cache-write at cached rate?* **No** — cache creation bills at the regular
  input rate; only cache reads bill at the cached rate.
- *Pricing credential same as ACL?* **No** — distinct audience/scope.
- *In-memory charge accumulator?* **No** — durable, idempotent row-locked
  reservations in Postgres.
- *Signup credit?* **No** — deposit-to-use; zero fee on devnet.

## 16. Implementation status

| Step | Status | Notes |
|------|--------|-------|
| 1 — Migration + pure package + store + tests | ✅ Done | `migrations/0006_billing.sql`, `internal/billing`, `internal/store/billing.go`, concurrency/idempotency tests green incl. `-race`. |
| 2 — Account identity + request inspection + caps | ✅ Done | `store.KeyStore.Validate → (accountID, ok, err)`, `auth.Middleware` threads `account.ID`, and `internal/gate.Inspect` bounds bodies, clamps existing output limits, and injects a route-compatible output limit when the operator cap is the only bound. Per-request caps can tighten but never widen account caps. |
| 3 — Seller asks + constrained routing | ✅ Done | Pricing credentials are audience-isolated; asks are validated against the seller's own live advertised service/model groups; the catalog market and billing gate both intersect asks with the current billable/routable peer snapshot. |
| 4 — Exact settlement | ✅ Done | The response parser normalizes regular/cached/output usage by dialect; settlement resolves the served peer against the immutable quote snapshot and finalizes idempotently. |
| 5 — Deposits | ✅ Done | Finalized Solana polling, persist-before-credit events, wallet-link reconciliation, and a durable CAS high-water cursor with retry-safe oldest-first processing. |
| 6 — Management API + UI | ✅ Done | Billing state, caps, decimal-string raw amounts, ledger/deposit pagination, wallet panel, and three-tier catalog market view. |
| 7 — Withdrawals | ✅ Done | Capability-gated, primary-wallet-bound reserve flow; durable signed/broadcast/finalized states; last-valid-block-height expiry proof; bigint-safe UI. |
| 8 — Market operability (issues #1/#2) | ✅ Done | Reference ask-publisher (`cmd/askpublish` + `internal/askpublish`: pricing challenge → sign → issue, 2-minute republish loop), and `BILLING_REQUIRE_PRICED_PEER` so a peer with no live ask is excluded from the affordable set for priced routes instead of serving free. |
| 9 — Price-aware routing (issue #3, Phase 1 market signal) | ✅ Done | The gate's `X-Otela-Allowed-Peers` stamp is cheapest-first; the mesh head (`opentela-ai/OpenTela`) weights selection by `decay^rank` (`routing.price_weight_decay`, default 0 = uniform) and retries deterministically prefer the next-cheapest allowed peer. |
| 10 — Seller pricing surface (cloud issue #1) | ✅ Done | `GET /manage/billing/asks`: live asks + unpriced advertised routes per owned instance for the console pricing panel. |
| 11 — Single settlement rail (issue #4) | ✅ Done | The legacy mesh settlement rail (`rates.yaml` per-1000 pricing, head/worker CRDT dispute reconciliation, head-signed direct SPL transfers) is **retired and removed** from `opentela-ai/OpenTela`; `billing.enabled` now fails fast at startup with a pointer to the market. The settlement tutorial was rewritten to the market model. One rail: this gateway. |
| 12 — Delegation registry + gate bound (Phase 2, §11 increment 1) | ✅ Done | `migrations/0011_delegation.sql` (allowance registry + `delegation` ledger source), `billing.Allowance/AllowanceChange/DelegationAvailable`, `store.UpsertAllowance` (registry upsert + exactly-once credit mirror via `(ref, leg)`; revocations clamp at `reserved_raw` with the shortfall reported), reserve gate bounded by `min(credit available, Σ active allowances)` inside the lock, `GET /manage/billing` reports `allowances`. |
| 13 — Allowance poller (Phase 2, §11 increment 2a) | ✅ Done | `internal/delegations` mirrors on-chain SPL delegations into the registry on `BILLING_ALLOWANCE_REFRESH` (`BILLING_SETTLEMENT_AUTHORITY` gates it): grants credit, revocations (ATA gone / delegate cleared / moved elsewhere) clamp — written only when we hold an active row, so never-delegated accounts poll for free. `solana.RPCClient.TokenDelegation` reads the parsed ATA (`delegate`, `delegatedAmount`, slot). `store.ConsumeAllowance` maintains the registry==chain invariant the worker will rely on so consumption is never misread as a revoke. On-chain batched `transfer_checked` settlement worker = increment 2b. |
| 14 — Settlement worker (Phase 2, §11 increment 2b) | ✅ Done | `internal/delegations.SettlementWorker` + `migrations/0012_delegation_settlements.sql` (durable `pending → signed → broadcast → finalized` batches with the withdrawal worker's expiry-proof restore). Claim pass: earn legs (buyer-side) with an active allowance are netted per (buyer, destination) — seller share buyer→seller, fee buyer→treasury — allowance consumed atomically at claim inside one tx with the batch rows, leg anchors (`UNIQUE ledger_leg_id` = exactly-once), and cursor advance; buyers with no/unlinked/insufficient authority are skipped (deposit rail keeps them). Sign/broadcast/finalize mirrors withdrawals incl. `SKIP LOCKED` replica safety and ambiguous-send retry; restore refunds the registry allowance (the on-chain authority is still there). `transfer_checked` (discriminant 12, decimals-verified) signed by `BILLING_SETTLEMENT_KEYPAIR`, which must equal `BILLING_SETTLEMENT_AUTHORITY` on boot and holds SOL for fees. Residual exposure (buyer ATA drained under the delegation) is §11.5.5's reconciliation domain. |
| 15 — Reconciliation (Phase 2, §11.5.5) | ✅ Done | `internal/billing/merkle.go` (deterministic domain-separated Merkle tree over `credit_ledger`, power-of-two pad, verified for all sizes 1–16 × every index × tamper) + `internal/delegations.Reconciler` + `store/reconciliation.go` (paged leaves, row-index lookup, in-flight and restored-batch sums). Endpoints: `GET /manage/billing/reconciliation` (ledger integrity + Merkle root + per-delegate registry-vs-chain cross-check with `expected = allowance − in-flight` + residual-exposure totals; `chain_audited` false until a settlement authority is configured) and `GET /manage/billing/merkle-proof?leaf_id=N` (leaf, audit path, root, index — hex). Wired in `cmd/server` when the authority is set; on-demand reads, cron/console driven. Also fixed: `RestoreSettlement`/`scanSettlement` crashed scanning NULL wire columns on pending batches. |
