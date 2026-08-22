# opentela api proxy

An authenticating reverse proxy in front of [opentela](https://opentela.ai/docs).
Clients authenticate with `Authorization: Bearer <key>` — or, for the Anthropic
Messages API, with `x-api-key: <key>`; valid keys (stored in
Postgres, cached in memory for 14 days) are forwarded transparently to the
configured upstream.

## Configuration (environment)

| Variable                 | Required | Default | Purpose                              |
|--------------------------|----------|---------|--------------------------------------|
| `OPENTELA_UPSTREAM_URL`  | yes      | —       | Base URL requests are forwarded to   |
| `DATABASE_URL`           | yes      | —       | Postgres DSN                         |
| `LISTEN_ADDR`            | no       | `:8080` | Listen address                       |
| `CACHE_TTL`              | no       | `336h`  | TTL for validated keys (14 days)     |
| `CACHE_NEGATIVE_TTL`     | no       | `30s`   | TTL for invalid results              |
| `CACHE_JANITOR_INTERVAL` | no       | `1m`    | Expired-entry sweep interval         |
| `INTERNAL_CONTROL_TOKEN` | no*      | —       | Bearer secret for internal ACL and node-credential endpoints (minimum 32 bytes) |
| `IDENTITY_MAX_AGE`       | no       | `720h`  | Max age for email-domain ACL matches |
| `OWNERSHIP_MAX_AGE`      | no       | `30s`   | Target freshness window for peer ownership checks |
| `DECISION_CACHE_TTL`     | no       | `30s`   | Suggested TTL returned by the ACL evaluator |
| `EVALUATOR_RATE_LIMIT_RPS` | no     | `50`    | Per-caller rate for live v2 ACL evaluations (`0` disables the limiter) |
| `EVALUATOR_RATE_LIMIT_BURST` | no   | `100`   | Per-caller burst allowance for the evaluator rate limiter |
| `NODE_CREDENTIAL_ISSUER` | no       | `api.opentela.ai` | Issuer for trusted-node JWTs |
| `NODE_CREDENTIAL_SIGNING_KID` | no* | —       | Active Ed25519 signing-key id |
| `NODE_CREDENTIAL_SIGNING_KEY` | no* | —       | Base64 Ed25519 seed or private key; enables trusted-node credentials |
| `NODE_CREDENTIAL_VERIFY_KEYS` | no | —       | Comma-separated `kid:base64-public-key` verification keys for rotation overlap |

### GPU performance pipeline (optional)

| Variable                 | Required | Default | Purpose                              |
|--------------------------|----------|---------|--------------------------------------|
| `TINYBIRD_APPEND_TOKEN`  | no       | —       | Tinybird `perf_append` token (APPEND scope); enables the managed Tinybird backend |
| `TINYBIRD_LEADERBOARD_TOKEN` | no   | —       | Tinybird `leaderboard_read` token (READ scope on the `gpu_leaderboard` pipe); required with the append token |
| `TINYBIRD_HOST`          | no       | `https://api.tinybird.co` | Tinybird API host (switch regions/workspace here) |
| `CLICKHOUSE_URL`         | no       | —       | ClickHouse HTTP endpoint; enables per-request performance sampling and `GET /v1/leaderboard` (mutually exclusive with the Tinybird tokens) |
| `CLICKHOUSE_DATABASE`    | no       | `opentela` | Database holding the perf tables |
| `CLICKHOUSE_USERNAME`    | no       | —       | ClickHouse user (sent as `X-ClickHouse-User`) |
| `CLICKHOUSE_PASSWORD`    | no       | —       | ClickHouse password (sent as `X-ClickHouse-Key`) |
| `PERF_FLUSH_INTERVAL`    | no       | `5s`    | Batch flush interval for perf inserts |
| `PERF_BATCH_SIZE`        | no       | `1024`  | Max rows per insert |
| `PERF_QUEUE_SIZE`        | no       | `16384` | Buffered-sample queue; excess samples are dropped and logged |
| `PERF_PEER_CACHE_TTL`    | no       | `5m`    | TTL for the peer→GPU attribution cache |
| `LEADERBOARD_CACHE_TTL`  | no       | `1m`    | TTL for cached leaderboard responses |

`INTERNAL_CONTROL_TOKEN` is required when `NODE_CREDENTIAL_SIGNING_KEY` is set.
`CLICKHOUSE_USERNAME`/`CLICKHOUSE_PASSWORD` require `CLICKHOUSE_URL`.
`TINYBIRD_APPEND_TOKEN` and `TINYBIRD_LEADERBOARD_TOKEN` must be set together.

## Run

```bash
export OPENTELA_UPSTREAM_URL=https://api.opentela.ai
export DATABASE_URL=postgres://user:pass@localhost:5432/opentela

# one-time: create the schema and a key
go run ./cmd/keyctl migrate
go run ./cmd/keyctl add --name alice   # prints the plaintext token once

go run ./cmd/server
```

## Manage keys

```bash
go run ./cmd/keyctl migrate            # apply migrations/*.sql, in order
go run ./cmd/keyctl add [--name NAME]  # create a key, print token once
go run ./cmd/keyctl revoke <token>     # deactivate a key
go run ./cmd/keyctl list               # list keys (hash prefix, name, status)
```

`keyctl`-created keys are plain admin keys with no owning user — they are
separate from, and unaffected by, the per-user key management API below.

## Using with Claude Code (Anthropic Messages API)

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) speaks the
Anthropic Messages API. Mesh services that implement it — SGLang and vLLM both
ship an Anthropic-compatible server (start the model with tool calling enabled,
i.e. `--enable-auto-tool-choice` and the right `--tool-call-parser`; see the
[vLLM Claude Code guide](https://docs.vllm.ai/en/stable/serving/integrations/claude_code/))
— are reachable through the service-scoped route. This gateway forwards
`POST /v1/service/<service>/v1/messages` (plus `/v1/messages/count_tokens` and
streaming SSE) with all `anthropic-*` headers intact, and accepts your opentela
API key in either credential header Claude Code sends: `Authorization: Bearer`
(from `ANTHROPIC_AUTH_TOKEN`) or `x-api-key` (from `ANTHROPIC_API_KEY`).

Launch Claude Code against the `llm` service (model names from
`GET /v1/services`):

```bash
ANTHROPIC_BASE_URL=https://api.opentela.ai/v1/service/llm \
ANTHROPIC_API_KEY=<your opentela key> \
ANTHROPIC_AUTH_TOKEN=<your opentela key> \
ANTHROPIC_DEFAULT_OPUS_MODEL=<model> \
ANTHROPIC_DEFAULT_SONNET_MODEL=<model> \
ANTHROPIC_DEFAULT_HAIKU_MODEL=<model> \
claude
```

Verified live against `moonshotai/Kimi-K3` (SGLang): plain and streaming
responses (full `message_start`…`message_stop` SSE sequence), `thinking`
blocks, and forced `tool_use` all come back as spec-correct Anthropic
envelopes.

- `ANTHROPIC_AUTH_TOKEN` is required by Claude Code; set both auth variables to
  the same opentela key.
- The base URL must include the service prefix (`/v1/service/llm`) — the bare
  `/v1/messages` root is not routed by the mesh. `GET .../v1/models` is served
  locally by this gateway as an OpenAI-shaped list of that service's models
  (API-key gated), so generic OpenAI-compatible SDKs can discover models
  instead of the upstream's "no provider found"; Claude Code itself does not
  need it as long as the three default-model variables are set.
- `<model>` must be a served-model alias — Claude Code cannot use model names
  containing `/`.
- If prefix caching suffers from Claude Code's per-request attribution hash,
  set `"CLAUDE_CODE_ATTRIBUTION_HEADER": "0"` in `~/.claude/settings.json`
  (handled server-side by vLLM > 0.17.1).

Any other Anthropic SDK client works the same way: use the service-scoped
gateway URL as the SDK base URL and the opentela key as the API key.

### Reasoning-marker normalization (`internal/thinkfix`)

Some backends serve the Anthropic API without a reasoning parser configured
for the model's actual reasoning marker format. vLLM drives its reasoning
state machine on token IDs, so markers can never appear in its output, but a
text-matching parser that misses the model's real section tokens surfaces
them in-band — the whole reply arrives as one text block containing
`<|open|>think<|sep|<|sep|>…reasoning…<|close|>think<|sep|…answer…`
(observed with Kimi-K3-style markers on SGLang; the separator renders in two
forms, `<|sep|` and `<|sep|>`, and often appears doubled). Instead of
guessing which backend is behind a request, the gateway scans Anthropic
Messages responses for the leaked-marker *effect* and translates it: the
response is restructured into proper `thinking` and `text` content blocks,
with dense downstream block indices and markers stripped, in both streaming
(SSE) and non-streaming form. Separator tokens (`<|sep|`/`<|sep|>`) are
stripped at the start of every section — after a translated marker, at an
upstream block boundary, and even in marker-free responses, which cleans up
the lone `<|sep|>` remnant left by backends that half-apply the thinking/text
split themselves.

- **Inert when nothing leaks.** Marker- and separator-free responses pass
  through byte-for-byte in streaming mode and unmodified in non-streaming
  mode, so well-configured backends (vLLM, fixed SGLang) see zero behavior
  change; the only cost is the scan.
- **Scope.** Only successful `POST …/v1/messages` responses without
  `Content-Encoding` are scanned (200 + `text/event-stream` SSE or
  `application/json`). Tool-call argument deltas (`input_json_delta`) are
  never scanned — a marker-shaped string inside tool JSON is data, not a
  section header — and corrupted/garbage frames pass through verbatim rather
  than being dropped.
- **Truncation tails.** A marker fragment at the end of a truncated stream
  (e.g. `<|close|>` cut at `max_tokens`) is dropped as truncation garbage.
- **Text-level trade-off.** Translation keys on rendered text, not token IDs,
  so a model that *deliberately* prints the literal marker strings would be
  misclassified. Those strings are special-token renderings that do not occur
  in legitimate output, so this is accepted; the fix still belongs on the
  inference node (correct `--reasoning-parser`), this is a safety net.

## OpenAI-compatible model list

Generic OpenAI-compatible SDKs hard-code `GET {base}/models` to discover what
they can call. The mesh only routes `/v1/service/<service>/` for inference, so
that request would otherwise reach the upstream and come back `503 no provider
found`. This gateway serves it locally instead:

```bash
curl -H "Authorization: Bearer <key>" \
  https://api.opentela.ai/v1/service/llm/v1/models
```

```json
{
  "object": "list",
  "data": [
    {"id": "moonshotai/Kimi-K3", "object": "model", "created": 1754012400, "owned_by": "opentela"}
  ]
}
```

The list is scoped to the single service named in the path, built from the
same distilled table as `GET /v1/services` and cached the same way. It is
API-key gated (like the rest of the proxy plane), so `Authorization: Bearer`
or `x-api-key` is required. An unknown service returns `200` with an empty
`data` array. Claude Code does not need this endpoint; set the three
`ANTHROPIC_DEFAULT_*_MODEL` variables instead.

## GPU performance sampling and leaderboard (optional, Tinybird or ClickHouse)

Every inference response the mesh routes is stamped with an `X-Computing-Node`
header naming the serving peer. When one of the two analytics backends is
configured, the streaming proxy measures each stamped response — time-to-first-byte, time-to-first-
content-token, generation duration, and token counts (parsed incrementally
from the OpenAI and Anthropic SSE frames, or from the JSON body for
non-streaming replies) — and resolves the peer to its GPU model via the node
table it already fetches for the catalog.

Samples are batched into `perf_samples` (30-day TTL) and aggregated into the
public leaderboard:

```bash
curl "https://api.opentela.ai/v1/leaderboard?hours=168&service=llm"
```

```json
{
  "generated_at": "2026-08-04T20:55:31Z",
  "window_hours": 168,
  "entries": [
    {
      "gpu_model": "NVIDIA GeForce RTX 4090",
      "model": "gpt-4o",
      "requests": 101,
      "providers": 8,
      "success_rate": 0.99,
      "avg_output_tokens_per_sec": 55.94,
      "p50_output_tokens_per_sec": 56,
      "ttft_p50_ms": 224,
      "ttft_p90_ms": 244,
      "ttft_p99_ms": 248
    }
  ]
}
```

`hours` defaults to 168 (max 720); `service` and `model` filter the rows. The
endpoint needs no API key, like `/v1/services`.

**Privacy**: ingestion persists only counters, timings, GPU model, and the
served model name — never API keys, prompts, response payloads, or user
identity. The serving peer is stored only as a truncated SHA-256 fingerprint
(`peer_fp`) used to count distinct providers.

**Setup — Tinybird Forward (managed, recommended)**: the data project lives in
[tinybird/](tinybird/) — the raw datasource (`perf_samples`) plus the published
aggregate endpoint (`gpu_leaderboard.pipe`) with its scoped tokens
(`perf_append`, `leaderboard_read`). Deploy and wire the tokens:

```bash
tb deploy --allow-destructive-operations   # first deploy may drop quickstart leftovers
export TINYBIRD_HOST=https://api.tinybird.co
tb token ls   # copy the perf_append and leaderboard_read tokens into:
export TINYBIRD_APPEND_TOKEN=…
export TINYBIRD_LEADERBOARD_TOKEN=…
```

The pipe answers the leaderboard straight off raw samples — no rollup table,
materialized view, or cron — and 30-day TTL lives in the datasource engine.

**Setup — self-managed ClickHouse (alternative)**: point the gateway at a
ClickHouse HTTP endpoint and apply the DDL (idempotent — safe to re-run):

```bash
clickhouse-client --database opentela --multiquery < clickhouse/schema.sql
```

Rollups are maintained by the `perf_hourly_mv` materialized view; no cron or
scheduler is needed.

Backends are mutually exclusive (`CLICKHOUSE_URL` and `TINYBIRD_APPEND_TOKEN`
together are rejected at startup). With neither set the pipeline is fully
inert: no hooks are installed and `/v1/leaderboard` is not mounted.

## Management and ACL API (optional)

In addition to `keyctl`-managed admin keys, end users can mint their own API
keys through a self-service HTTP plane, gated by a
[Neon Auth](https://neon.tech/docs/guides/auth) JWT rather than an opentela
API key.

This plane is enabled only when **both** `NEON_AUTH_JWKS_URL` and
`NEON_AUTH_ISSUER` are set. When disabled (the default), `/manage/*` is not
routed specially and falls through to the normal auth-gated proxy — the
service behaves exactly like a pure proxy.

When enabled, requests to `/manage/*` must carry
`Authorization: Bearer <Neon Auth JWT>` (EdDSA-signed, verified against the
project's JWKS; `iss`/`exp`/`nbf` and, if configured, `aud` are enforced).
The JWT's `sub` claim scopes every operation to that user's own keys. Each
authenticated management request refreshes a server-side identity snapshot
(`email`, verified flag, normalized domain, and verification time). Email-domain
ACL rules fail closed when that snapshot becomes older than `IDENTITY_MAX_AGE`.

The Neon Auth project itself is configured to **require email verification**:
sign-up sends a verification email and unverified users cannot sign in
(`neon neon-auth config email-password update --require-email-verification
--send-verification-email-on-sign-up`). The verification method is OTP
(`otp`), which works with Neon's shared email provider. Verified accounts can
claim one OTELA grant from the [faucet](#faucet-endpoints).

### Endpoints

**`POST /manage/keys`** — create a key for the authenticated user.

```
POST /manage/keys
Authorization: Bearer <neon-auth-jwt>
Content-Type: application/json

{"name": "laptop"}
```

```
201 Created
{
  "id": 7,
  "key": "sk-3f9c2a1e...",     // shown once — store it now, it is not retrievable again
  "prefix": "sk-3f9c2a1",
  "name": "laptop",
  "created_at": "2026-07-23T09:00:00Z"
}
```

Returns `409 Conflict` if the user is already at `MAX_KEYS_PER_USER` active keys.

**`GET /manage/keys`** — list the authenticated user's keys (secrets omitted).

```
GET /manage/keys
Authorization: Bearer <neon-auth-jwt>
```

```
200 OK
[
  {
    "id": 7,
    "name": "laptop",
    "prefix": "sk-3f9c2a1",
    "created_at": "2026-07-23T09:00:00Z",
    "revoked_at": null
  }
]
```

**`DELETE /manage/keys/{id}`** — revoke one of the authenticated user's keys.
Scoped to the caller: revoking another user's key id returns `404 Not Found`.

```
DELETE /manage/keys/7
Authorization: Bearer <neon-auth-jwt>
```

```
204 No Content
```

### Wallet endpoints

Each OpenTela Cloud account operates a **single linked wallet**, and every
peer it claims is owned by that wallet — one account, one wallet, many peers.
A second, different wallet is rejected with `409`; unlinking the wallet is
blocked with `409` while it still proves one of the account's claimed
instances (release those instances first).

- `GET /manage/wallets` lists the caller's verified linked wallet (at most one).
- `POST /manage/wallets/challenges` accepts `{"wallet":"<base58>"}` and returns
  `{id,message,expires_at}` for a single-use five-minute challenge.
- `POST /manage/wallets` accepts `{"challenge_id":"...","signature":"<base58>"}`.
  The signature must verify over the exact returned challenge message. Success
  returns `201`. Replay/expired/already-linked wallets return `409`; an account
  that already has a linked wallet also returns `409`. Invalid signatures return
  `422`.
- `DELETE /manage/wallets/{id}` unlinks the caller's wallet. It returns
  `409` while that wallet is still the ownership proof for one of the caller's
  claimed instances.

### Faucet endpoints

The OTELA faucet is enabled only when `FAUCET_WALLET_KEYPAIR`, `FAUCET_MINT`,
and `FAUCET_SOLANA_RPC_URL` are all set. It pays a **one-time** OTELA grant
to accounts whose Neon Auth JWT carries a verified email claim, sending the
tokens on-chain to the caller's primary linked wallet's associated token
account, creating that account first when it does not yet exist. The faucet
wallet itself must hold OTELA in its own associated token account and enough
SOL for transaction fees.

The transfer is built and signed entirely with the standard library (no
`@solana/web3.js` / `@solana/spl-token` dependency): the recipient's
associated token account address is derived with the same
`findProgramAddress` semantics as the SPL library (off-curve SHA-256 over
`[owner, tokenProgram, mint]` + bump, hashed with `ProgramDerivedAddress`
after the program id), and the transaction is the SPL `transfer`
instruction, optionally preceded by `createAssociatedTokenAccount`.

- `GET /manage/faucet` reports the caller's faucet state:
  ```
  {"enabled": true, "email_verified": true, "mint": "Esmc…",
   "amount_raw": 1000000000, "amount_ui": "1", "decimals": 9,
   "claimed": false, "claimed_at": null, "wallet": null, "tx_signature": null}
  ```
  `enabled` is `false` when the faucet is not configured on this deployment.
  `claimed` (and `claimed_at`/`wallet`/`tx_signature`) is populated only for
  a **completed** claim — a claim whose transaction has been recorded. An
  in-flight claim therefore reads `claimed:false`, which is why the claim
  endpoint still returns `409` for a recent in-flight attempt.

- `POST /manage/faucet/claim` performs the payout.
  - `201 Created` with `{"status":"claimed","wallet","amount_raw",
    "amount_ui","tx_signature"}` on success.
  - `403 Forbidden` — the email is not verified.
  - `409 Conflict` — no wallet is linked, or the account has already claimed
    (a completed claim, or a pending reservation that is still in flight).
  - `503 Service Unavailable` — the on-chain transfer failed (the pending
    reservation is rolled back so the account may retry), or the transaction
    landed but could not be recorded (returned for manual reconciliation).
  - `404 Not Found` — the faucet is disabled.

#### Claim lifecycle and race safety

A claim is a three-phase operation, with the `faucet_claims` table's
`PRIMARY KEY (account_id)` providing the race-safety invariant:

1. **Reserve.** Insert a row with `tx_signature = ''` ("pending"). The unique
   constraint means only one concurrent request per account reaches the
   on-chain send — a second attempt is rejected with `409`.
2. **Send.** Broadcast the SPL `transfer`. On failure the pending row is
   deleted (`ClearFaucetClaim`) so the account can retry immediately.
3. **Complete.** `UPDATE … SET tx_signature = $sig WHERE tx_signature = ''`
   records the on-chain signature. A completed claim is never overwritten,
   so a late-arriving retry from a crashed request cannot clobber it.

If the server crashes between phases 2 and 3, a pending row lingers and
blocks retries. `ClaimFaucet` will take it over again only once it is older
than `FaucetPendingTTL` (2 minutes) — past the Solana mainnet blockhash
validity window (~60s) and the API's 60s send timeout, so the original
transaction, if any, can no longer confirm. The edge case this leaves: a
process that crashed *after* a successful send but *before* completing the
row, retried after 2 minutes, can double-pay by at most one extra grant.

### Instance and ACL endpoints

- `POST /manage/instances` accepts `{"peer_id":"...","label":"..."}`. The API
  verifies that the peer is currently visible upstream, its identity attestation
  is valid, and the attested wallet is already linked to the caller. Success
  returns `201` and starts with `restricted` owner-only access.
- `GET /manage/instances` lists only the caller's claimed peers, including
  `peer_id`, `label`, `owner_wallet`, `mode`, `rules`, `ownership_status`,
  `ownership_observed_at`, `policy_revision`, and `online`.
- `PATCH /manage/instances/{id}` updates only `label` and `mode`.
- `PUT /manage/instances/{id}/acl` atomically replaces the mode and entire rule
  set with `{"mode":"public|restricted","rules":[...]}`.
- `GET /manage/instances/{id}/services` returns observed exact service names,
  current `peer|service` policy scope, and the `service-policy-v2` capability.
- `PUT /manage/instances/{id}/services` atomically replaces mixed-service
  exposure. Each exact service is `permissionless`, `trusted_region`, or
  `disabled`, with its own `inherit|public|restricted` ACL mode. Moving from
  service scope back to peer scope requires `acknowledge_scope_reset: true` and
  is rejected while trusted bindings remain.
- `PUT /manage/instances/{id}/services/{service_id}/acl` replaces one service ACL.
- `DELETE /manage/instances/{id}` deletes one of the caller's claims.

Supported ACL rule kinds:

- `email_domain`: lower-case ASCII DNS domain, exact match only after the final `@`.
- `wallet`: canonical base58 Ed25519/Solana public key.

Rules use OR semantics. Duplicate normalized rules are collapsed. An empty
restricted ACL is owner-only.

### Trusted-region endpoints

- `POST|GET /manage/regions` creates or lists regions owned by the caller. List
  responses include current members and pending invitations.
- `GET|PATCH|DELETE /manage/regions/{region_id}` reads, enables/disables, or
  deletes an owned region.
- `POST|GET /manage/regions/{region_id}/members` creates an expiring invitation
  (or explicitly auto-accepts a caller-owned instance) and lists lifecycle state.
- `PATCH /manage/regions/{region_id}/members/{instance_id}` accepts an invitation
  or changes an owned region member's role/status. Reactivation requires a fresh
  matching peer ownership observation.
- `DELETE /manage/regions/{region_id}/members/{instance_id}` cancels a pending
  invitation or releases membership. Release fails while trusted bindings exist.

Region membership is API-authoritative. Advertising a region name, copying a
service name, or joining the physical libp2p mesh does not grant trusted access.

### Internal evaluator

`POST /internal/acl/evaluate` is mounted ahead of the proxy catch-all and is
never for browsers or public callers. It requires:

```http
Authorization: Bearer <INTERNAL_CONTROL_TOKEN>
Content-Type: application/json
```

Request body:

```json
{"key_hash":"<lowercase sha256 hex>","peer_ids":["peer-a","peer-b"]}
```

Response body:

```json
{
  "key_id": 17,
  "allowed_peer_ids": ["peer-a"],
  "denied": [{"peer_id":"peer-b","reason":"no_match"}],
  "primary_wallet": "<base58-solana-pubkey>",
  "cache_ttl_seconds": 30
}
```

The raw API key is never sent to this endpoint. Invalid/revoked keys return
`401`; malformed payloads return `400`; evaluation failures return `503`.

Service-aware runtimes use `POST /internal/acl/evaluate-v2`. Permissionless
calls authenticate with `INTERNAL_CONTROL_TOKEN`; trusted calls authenticate
with a short-lived peer-key-bound node JWT. The request names the exact
`partition`, `region`, `route_kind`, `service`, candidate peer ids, and (for a
worker check) its authenticated libp2p upstream peer id. Trusted decisions are
not cached by OpenTela and unmanaged peers are denied.

Trusted nodes acquire that JWT through `POST /internal/node-credentials/challenges`
and `POST /internal/node-credentials`. Both endpoints require
`INTERNAL_CONTROL_TOKEN`; issuance additionally verifies a single-use challenge
signed by the node's libp2p private key, active region membership, role,
membership revision, and fresh ownership.

### Configuration (key management)

| Variable                    | Required | Default | Purpose                                          |
|------------------------------|----------|---------|---------------------------------------------------|
| `NEON_AUTH_JWKS_URL`         | no*      | —       | JWKS endpoint used to verify Neon Auth JWTs       |
| `NEON_AUTH_ISSUER`           | no*      | —       | Expected JWT `iss` claim                          |
| `NEON_AUTH_AUDIENCE`         | no       | —       | Expected JWT `aud` claim (enforced only if set)   |
| `NEON_AUTH_JWKS_CACHE_TTL`   | no       | `1h`    | How long fetched JWKS keys are cached             |
| `MAX_KEYS_PER_USER`          | no       | `10`    | Max active self-service keys per user             |
| `CORS_ALLOWED_ORIGINS`       | no       | —       | Comma-separated origins allowed to call the API from a browser — both `/manage/keys*` and the `/v1/*` proxy |
| `INTERNAL_CONTROL_TOKEN`     | no*      | —       | Shared bearer secret for internal ACL and credential endpoints (minimum 32 bytes) |
| `IDENTITY_MAX_AGE`           | no       | `720h`  | Freshness window for email-domain ACL matches     |
| `OWNERSHIP_MAX_AGE`          | no       | `30s`   | Target max peer-ownership staleness window        |
| `DECISION_CACHE_TTL`         | no       | `30s`   | Suggested downstream cache TTL for ACL decisions  |
| `NODE_CREDENTIAL_ISSUER`     | no       | `api.opentela.ai` | Trusted-node JWT issuer                 |
| `NODE_CREDENTIAL_SIGNING_KID` | no*     | —       | Active Ed25519 signing key id                     |
| `NODE_CREDENTIAL_SIGNING_KEY` | no*     | —       | Base64 Ed25519 seed/private key                    |
| `NODE_CREDENTIAL_VERIFY_KEYS` | no      | —       | Comma-separated verification keys for overlap     |
| `FAUCET_SOLANA_RPC_URL`       | no*     | —       | Solana JSON-RPC endpoint used for faucet payouts  |
| `FAUCET_MINT`                 | no*     | —       | OTELA SPL token mint address                      |
| `FAUCET_TOKEN_PROGRAM`        | no      | `TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA` | SPL Token or Token-2022 program id |
| `FAUCET_WALLET_KEYPAIR`       | no*     | —       | Base64 Ed25519 seed/private key of the funded faucet wallet |
| `FAUCET_AMOUNT`               | no      | `1000000000` | Per-claim payout in token base units (1 OTELA @ 9 decimals) |
| `FAUCET_DECIMALS`             | no      | `9`      | Token decimals, used for display and JSON output  |

\* `NEON_AUTH_JWKS_URL` and `NEON_AUTH_ISSUER` must be set together — setting
only one is a config error. Setting neither leaves key management disabled.
`NODE_CREDENTIAL_SIGNING_KID` and `INTERNAL_CONTROL_TOKEN` are required when
`NODE_CREDENTIAL_SIGNING_KEY` is configured. The faucet requires
`FAUCET_WALLET_KEYPAIR`, `FAUCET_MINT`, and `FAUCET_SOLANA_RPC_URL` together —
setting any one without the others is a config error. The faucet wallet must
hold OTELA in its associated token account (plus SOL for fees) to pay out.

## Test

```bash
go test ./...                          # unit tests (Postgres test skips)
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # + integration
```
