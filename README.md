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
| `NODE_CREDENTIAL_ISSUER` | no       | `api.opentela.ai` | Issuer for trusted-node JWTs |
| `NODE_CREDENTIAL_SIGNING_KID` | no* | —       | Active Ed25519 signing-key id |
| `NODE_CREDENTIAL_SIGNING_KEY` | no* | —       | Base64 Ed25519 seed or private key; enables trusted-node credentials |
| `NODE_CREDENTIAL_VERIFY_KEYS` | no | —       | Comma-separated `kid:base64-public-key` verification keys for rotation overlap |

`INTERNAL_CONTROL_TOKEN` is required when `NODE_CREDENTIAL_SIGNING_KEY` is set.

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
  `/v1/messages` root is not routed by the mesh. `GET .../v1/models` is not
  routed on that prefix either; with the three default-model variables set,
  Claude Code never needs it.
- `<model>` must be a served-model alias — Claude Code cannot use model names
  containing `/`.
- If prefix caching suffers from Claude Code's per-request attribution hash,
  set `"CLAUDE_CODE_ATTRIBUTION_HEADER": "0"` in `~/.claude/settings.json`
  (handled server-side by vLLM > 0.17.1).

Any other Anthropic SDK client works the same way: use the service-scoped
gateway URL as the SDK base URL and the opentela key as the API key.

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

- `GET /manage/wallets` lists the caller's verified linked wallets.
- `POST /manage/wallets/challenges` accepts `{"wallet":"<base58>"}` and returns
  `{id,message,expires_at}` for a single-use five-minute challenge.
- `POST /manage/wallets` accepts `{"challenge_id":"...","signature":"<base58>"}`.
  The signature must verify over the exact returned challenge message. Success
  returns `201`. Replay/expired/already-linked wallets return `409`. Invalid
  signatures return `422`.
- `DELETE /manage/wallets/{id}` unlinks one of the caller's wallets. It returns
  `409` while that wallet is still the ownership proof for one of the caller's
  claimed instances.

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

\* `NEON_AUTH_JWKS_URL` and `NEON_AUTH_ISSUER` must be set together — setting
only one is a config error. Setting neither leaves key management disabled.
`NODE_CREDENTIAL_SIGNING_KID` and `INTERNAL_CONTROL_TOKEN` are required when
`NODE_CREDENTIAL_SIGNING_KEY` is configured.

## Test

```bash
go test ./...                          # unit tests (Postgres test skips)
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # + integration
```
