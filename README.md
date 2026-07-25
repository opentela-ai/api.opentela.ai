# opentela api proxy

An authenticating reverse proxy in front of [opentela](https://opentela.ai/docs).
Clients authenticate with `Authorization: Bearer <key>`; valid keys (stored in
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

## Key management API (optional)

In addition to `keyctl`-managed admin keys, end users can mint their own API
keys through a self-service HTTP plane, gated by a
[Neon Auth](https://neon.tech/docs/guides/auth) JWT rather than an opentela
API key.

This plane is enabled only when **both** `NEON_AUTH_JWKS_URL` and
`NEON_AUTH_ISSUER` are set. When disabled (the default), `/manage/*` is not
routed specially and falls through to the normal auth-gated proxy — the
service behaves exactly like a pure proxy.

When enabled, requests to `/manage/keys*` must carry
`Authorization: Bearer <Neon Auth JWT>` (EdDSA-signed, verified against the
project's JWKS; `iss`/`exp`/`nbf` and, if configured, `aud` are enforced).
The JWT's `sub` claim scopes every operation to that user's own keys.

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

### Configuration (key management)

| Variable                    | Required | Default | Purpose                                          |
|------------------------------|----------|---------|---------------------------------------------------|
| `NEON_AUTH_JWKS_URL`         | no*      | —       | JWKS endpoint used to verify Neon Auth JWTs       |
| `NEON_AUTH_ISSUER`           | no*      | —       | Expected JWT `iss` claim                          |
| `NEON_AUTH_AUDIENCE`         | no       | —       | Expected JWT `aud` claim (enforced only if set)   |
| `NEON_AUTH_JWKS_CACHE_TTL`   | no       | `1h`    | How long fetched JWKS keys are cached             |
| `MAX_KEYS_PER_USER`          | no       | `10`    | Max active self-service keys per user             |
| `CORS_ALLOWED_ORIGINS`       | no       | —       | Comma-separated origins allowed to call the API from a browser — both `/manage/keys*` and the `/v1/*` proxy |

\* `NEON_AUTH_JWKS_URL` and `NEON_AUTH_ISSUER` must be set together — setting
only one is a config error. Setting neither leaves key management disabled.

## Test

```bash
go test ./...                          # unit tests (Postgres test skips)
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # + integration
```
