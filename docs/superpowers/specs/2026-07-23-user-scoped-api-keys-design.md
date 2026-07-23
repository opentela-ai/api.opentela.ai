# User-Scoped API Keys via Neon Auth — Design

**Date:** 2026-07-23
**Status:** Approved
**Builds on:** [2026-07-22-opentela-proxy-design.md](./2026-07-22-opentela-proxy-design.md)

## Summary

Add a second, independent auth plane to the existing opentela proxy so that a
logged-in user can mint, list, and revoke their own opentela API keys over
HTTP. A separate frontend authenticates users with **Neon Auth** and calls the
backend with the resulting **Ed25519 (EdDSA) JWT**. The backend verifies the
JWT against Neon Auth's JWKS, then manages `sk-…` keys owned by that user in the
same `api_keys` table the proxy already validates against.

The existing proxy plane (API-key `Bearer` → Postgres hash lookup → forward to
opentela) is **unchanged**. The new key-management plane lives under a distinct
`/manage/` path prefix and is validated by a completely separate mechanism.

## Goals

- Let a Neon-Auth-authenticated user create a new opentela API key on demand.
- Let that user list their keys (metadata only) and revoke individual keys.
- Verify Neon Auth JWTs with zero new third-party dependencies (stdlib
  `crypto/ed25519`), consistent with the project's stdlib-first + `pgx`-only
  philosophy.
- Keep the proxy plane and its 14-day validation cache untouched.
- Make the key-management plane **optional**: if Neon Auth is not configured,
  the service behaves exactly as it does today (pure proxy).

## Non-Goals

- No user accounts, passwords, or sessions in this backend — login happens in
  the frontend via Neon Auth; the backend only verifies JWTs.
- No returning an existing key's plaintext. Keys are stored as SHA-256 hashes
  only; a key's plaintext is shown **exactly once**, at creation.
- No per-user quotas/billing/rate-limiting beyond a simple cap on the number of
  active keys per user.
- No change to how the proxy forwards requests or caches validations.

> This supersedes the prior spec's non-goal "No HTTP admin API for key
> management (CLI only)" — but only for **user-owned** keys under `/manage/`.
> The `keyctl` CLI remains for admin/seed keys (which have `user_id = NULL`).

## Architecture: two auth planes

Both planes use the `Authorization: Bearer <token>` header. They are
disambiguated **entirely by request path** — a client hits the right path with
the right token type. There is no content-sniffing of the token.

| Path pattern | Auth | Validated by | Handler |
|---|---|---|---|
| `GET /healthz` | none | — | health (existing) |
| `POST /manage/keys` | Neon Auth JWT | `internal/neonauth` (Ed25519 + JWKS) | create key |
| `GET /manage/keys` | Neon Auth JWT | same | list keys |
| `DELETE /manage/keys/{id}` | Neon Auth JWT | same | revoke key |
| everything else (`/`) | API key `sk-…` | `internal/auth` + `store` + cache (existing) | reverse proxy → opentela |

Rationale for the `/manage/` prefix: the proxy plane forwards **all**
non-carved paths to the opentela upstream. A management path must never be
forwardable and must not collide with opentela's own namespace (e.g. OpenAI-style
`/v1/...`). `/manage/` is reserved locally and stripped from the proxy plane by
mux routing.

## Data model

Migration `migrations/0002_user_keys.sql`:

```sql
ALTER TABLE api_keys ADD COLUMN user_id    TEXT;   -- Neon Auth 'sub'; NULL = admin/keyctl key
ALTER TABLE api_keys ADD COLUMN key_prefix TEXT;   -- non-secret display hint, e.g. 'sk-1a2b3c4'
CREATE INDEX IF NOT EXISTS idx_api_keys_user_id
    ON api_keys (user_id) WHERE user_id IS NOT NULL;
```

- Existing keyctl-seeded keys keep `user_id = NULL` and continue to validate.
- `key_prefix` = `sk-` + the first 8 hex chars of the token (e.g. `sk-1a2b3c4d`).
  This is ~32 bits of a ~256-bit key — enough to identify a key in a list, far
  too little to be a secret. It is the only non-hash material stored.
- The proxy's `Validate(keyHash)` query is unchanged; ownership columns do not
  affect it.

## Components

Each new package has one responsibility, a small interface, and is testable in
isolation.

### `internal/neonauth` — JWT verifier

- `Verifier` with `Verify(ctx, rawToken) (Claims, error)`.
- Fetches the JWKS from `NEON_AUTH_JWKS_URL`, parses the single Ed25519 JWK
  (`kty:OKP`, `crv:Ed25519`) by base64url-decoding `x` into an
  `ed25519.PublicKey`, indexed by `kid`. Caches with a TTL
  (`NEON_AUTH_JWKS_CACHE_TTL`, default `1h`) and refreshes once on an unknown
  `kid` (key rotation).
- Verification enforces, in order: header `alg == "EdDSA"` (explicitly reject
  `none` and any other alg — algorithm-confusion guard); signature via
  `ed25519.Verify` over `base64url(header) + "." + base64url(payload)`;
  `exp` present and in the future; `nbf`/`iat` sane (small leeway); `iss ==
  NEON_AUTH_ISSUER`; `aud == NEON_AUTH_AUDIENCE` **iff** that env is set.
- Returns `Claims{Subject string; ...}`. Never logs token contents.
- Uses `net/http` with a bounded timeout for JWKS fetch; a fetch failure on an
  unknown `kid` yields an auth failure, not a 500.

### `internal/keysvc` — key lifecycle

- Owns token generation: `sk-<hex>` from `crypto/rand` (moved here from
  `cmd/keyctl`; `keyctl` is refactored to call this so there is one generator).
- `Create(ctx, userID, name) (plaintext string, info KeyInfo, err error)`:
  generate token → `store.HashKey` → compute prefix → enforce per-user cap →
  `store.InsertUserKey`. Returns the plaintext once.
- Depends only on `store` (via an interface) and `crypto/rand`.

### `internal/keysapi` — HTTP handlers + JWT middleware

- `Middleware(v Verifier)` extracts the Bearer JWT, calls `Verify`, and puts the
  `sub` (user id) into the request context; missing/invalid → `401`.
- Three handlers: create, list, revoke — thin, reading `userID` from context and
  delegating to `keysvc` / `store`.
- JSON request/response encoding; no HTML.

### `store` additions (`internal/store/postgres.go`)

- `InsertUserKey(ctx, userID, keyHash, name, prefix) error`
- `ListByUser(ctx, userID) ([]KeyInfo, error)` — active + revoked, newest first,
  **no hash or plaintext** in the returned rows beyond `key_prefix`.
- `RevokeByIDForUser(ctx, userID, id) (bool, error)` — `UPDATE ... WHERE id=$1
  AND user_id=$2 AND active=TRUE`. Owner-scoped: another user's id (or a
  nonexistent id) affects 0 rows → reported as not-found by the handler.
- `CountActiveByUser(ctx, userID) (int, error)` — for the cap check.
- `KeyInfo` gains `ID int64`, `UserID *string`, `Prefix string`.

## HTTP API

```
POST /manage/keys
  Auth: Bearer <neon-auth-JWT>
  Body: {"name": "laptop"}            (name optional, <= 100 chars)
  201:  {"id": 42, "key": "sk-1a2b3c4d...", "prefix": "sk-1a2b3c4d",
         "name": "laptop", "created_at": "2026-07-23T..."}   # key shown ONCE

GET /manage/keys
  Auth: Bearer <neon-auth-JWT>
  200:  [{"id": 42, "name": "laptop", "prefix": "sk-1a2b3c4d",
          "created_at": "...", "revoked_at": null}, ...]      # never any plaintext

DELETE /manage/keys/{id}
  Auth: Bearer <neon-auth-JWT>
  204:  (revoked)
  404:  (no such key owned by this user)
```

### Status codes

| Condition | Status |
|---|---|
| Missing/malformed/expired/invalid JWT | 401 |
| Valid JWT, key cap exceeded on create | 409 |
| Valid JWT, revoke id not owned by user | 404 |
| Store/DB error | 503 |
| Key-management plane not configured | 404 (route absent) |
| Bad JSON / name too long | 400 |

## CORS

The frontend calls `/manage/keys` from the browser, so the management plane
needs CORS. A minimal, configurable layer applied **only** to `/manage/`:

- `CORS_ALLOWED_ORIGINS` — comma-separated allowlist (exact-match origins). Unset
  → CORS disabled (no headers emitted).
- Handle preflight `OPTIONS` (respond `204` with
  `Access-Control-Allow-Methods: GET, POST, DELETE, OPTIONS`,
  `Access-Control-Allow-Headers: Authorization, Content-Type`), and echo an
  allowed `Origin` on actual responses. No wildcard with credentials.
- The proxy plane is unaffected (opentela clients are not browsers).

## Configuration additions (`internal/config`)

| Env var | Required | Default | Purpose |
|---|---|---|---|
| `NEON_AUTH_JWKS_URL` | to enable mgmt plane | — | JWKS endpoint (public keys) |
| `NEON_AUTH_ISSUER` | to enable mgmt plane | — | expected `iss` claim |
| `NEON_AUTH_AUDIENCE` | no | — | if set, `aud` is verified |
| `NEON_AUTH_JWKS_CACHE_TTL` | no | `1h` | JWKS cache TTL (must be > 0) |
| `MAX_KEYS_PER_USER` | no | `10` | cap on active keys per user (> 0) |
| `CORS_ALLOWED_ORIGINS` | no | — | browser origins for `/manage/` |

**Enable rule:** the key-management plane is mounted only when **both**
`NEON_AUTH_JWKS_URL` and `NEON_AUTH_ISSUER` are set. If either is missing, the
routes are not registered and the binary runs as today's pure proxy. This lets
us deploy to Fly before the frontend/Neon-Auth wiring exists, and keeps all
existing tests and behavior valid.

## Error handling

- JWT verification never returns 5xx for a bad/expired/forged token — always
  401. 5xx is reserved for genuine server faults (DB down → 503).
- Token contents, key plaintext, and hashes are never logged. JWKS fetch errors
  are logged without token material.
- Store errors are wrapped with a per-operation label (matching the existing
  `store: <op>: %w` convention) and surfaced as 503.

## Testing

- `internal/neonauth`: table-driven tests using **test-generated** Ed25519
  keypairs and a hand-built JWKS. Cases: valid; expired; `nbf` in future;
  bad signature; `alg:none`; `alg:HS256` (confusion); unknown `kid` (triggers one
  refresh); missing `sub`; wrong `iss`; wrong `aud` when audience configured.
- `internal/keysvc`: token format (`sk-` + hex), hash matches `store.HashKey`,
  prefix derivation, cap enforcement (via a fake store).
- `internal/keysapi`: `httptest` with a fake verifier + fake store — create
  returns key exactly once and never on list; list omits all secret material;
  delete is owner-scoped (another user's id → 404); missing JWT → 401; cap →
  409; bad JSON → 400. CORS preflight and origin echo.
- `store` user methods: integration test gated on `TEST_DATABASE_URL` (Neon
  branch or containerized Postgres), exercising insert → list → owner-scoped
  revoke → cap count. Runs migrations `0001` + `0002`.
- Regression: existing proxy/auth/cache tests unchanged and still green;
  `keyctl` still works after the token-generator refactor.

## Deployment note (tracked separately)

The Fly.io deployment (Dockerfile, `fly.toml`, secrets via `flyctl secrets`) is
independent of this feature and handled as a separate task. Because the
management plane is opt-in, the current proxy can be deployed first; the Neon
Auth env vars are added later to light up `/manage/keys`.
