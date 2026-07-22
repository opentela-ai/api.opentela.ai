# OpenTela API Proxy — Design

**Date:** 2026-07-22
**Status:** Approved

## Summary

An HTTP service in Go that acts as an authenticating reverse proxy in front of
[opentela](https://opentela.ai/docs). Clients present their own API key; the
service validates it against a Postgres database and, if valid, transparently
forwards the request to a configurable opentela upstream. Validated keys are
cached in memory for 14 days to avoid a database round-trip on every request.

## Goals

- Authenticate incoming requests using an API key stored in Postgres.
- Transparently reverse-proxy all valid requests to opentela, including
  streaming (SSE) responses used for LLM output.
- Cache validation results in memory (14-day TTL) to minimize DB load.
- Provide a small CLI + SQL migration to seed and manage keys for dev/testing.

## Non-Goals

- No HTTP admin API for key management (CLI only).
- No active revocation propagation — a revoked key may keep working until its
  cache entry expires or the process restarts (accepted staleness).
- No per-client → upstream-key swapping. The client's `Authorization` header is
  forwarded to opentela unchanged.
- No rate limiting, quotas, or billing (out of scope for this iteration).

## Auth Model

- Clients send their own key as `Authorization: Bearer <key>`.
- The service validates the key against Postgres (exists AND `active`).
- Valid → forward the request to opentela. Invalid or missing → `401`.
- The `Authorization` header is passed through to opentela unchanged. This
  service is the access-control layer; opentela handles its own upstream
  concerns.

## Forwarding Behavior

- Transparent reverse proxy: all paths and HTTP methods are forwarded.
- Preserve request path, query string, headers, and body.
- Stream responses back to the client (SSE-friendly): the proxy flushes as data
  arrives rather than buffering full responses.
- Hop-by-hop headers are handled by the standard library proxy; standard
  `X-Forwarded-*` headers are set.

## Architecture

```
cmd/server/main.go     — entrypoint: load config, wire store→validator→proxy, serve
cmd/keyctl/main.go     — CLI: migrate | add | revoke | list  (seeds/manages keys)
internal/config        — env var loading + validation
internal/store         — KeyStore interface + Postgres impl (validate by key hash)
internal/cache         — TTL cache (positive 14d, negative short, janitor)
internal/auth          — Validator (cache↔store) + Bearer-auth middleware
internal/proxy         — httputil.ReverseProxy to upstream, streaming-tuned
internal/server        — mux: /healthz + auth-middleware → proxy
migrations/0001_init.sql
```

### Component responsibilities

- **config** — Reads and validates environment variables at startup. Fails fast
  with a clear error if a required variable is missing or malformed.
- **store** — `KeyStore` interface with a single `Validate(ctx, keyHash) (bool,
  error)` method (returns whether an active key exists for the hash). Postgres
  implementation uses `pgxpool`. The interface lets validation logic be tested
  with a fake.
- **cache** — Concurrency-safe TTL cache keyed by key-hash. Stores a boolean
  result with an expiry. Positive results use the 14-day TTL; negative results
  use a short TTL. A background janitor goroutine periodically evicts expired
  entries to bound memory (important because arbitrary bad keys can populate
  negative entries).
- **auth.Validator** — Combines cache and store. On a request: check cache; on
  miss, query the store, populate the cache with the appropriate TTL, and return
  the result.
- **auth middleware** — Extracts the Bearer token, rejects missing/malformed
  headers with `401`, hashes the token, asks the Validator, and either calls the
  next handler (the proxy) or returns `401`.
- **proxy** — A `net/http/httputil.ReverseProxy` targeting the upstream base
  URL, tuned for streaming (`FlushInterval`), with an `ErrorHandler` that returns
  `502` when the upstream is unreachable.
- **server** — Builds the `http.ServeMux`: an unauthenticated `/healthz` and the
  authenticated catch-all that runs the proxy behind the auth middleware.

### Request flow

```
request
  → auth middleware: extract "Authorization: Bearer <key>"
  → Validator.Valid(ctx, sha256(key)):
      → cache hit  → return cached bool
      → cache miss → store.Validate(...) → populate cache → return bool
  → valid   → ReverseProxy streams upstream response back to client
  → invalid → 401
```

## Security

- API keys are stored as **SHA-256 hex hashes**, never plaintext.
- The in-memory cache is keyed by the same hash, so plaintext keys are not
  retained in cache maps.
- The seeding CLI generates/accepts a token, stores only its hash, and prints
  the plaintext once for the operator to record.

## Data Model

```sql
CREATE TABLE api_keys (
  id         BIGSERIAL PRIMARY KEY,
  key_hash   TEXT NOT NULL UNIQUE,           -- sha256 hex of the token
  name       TEXT,                            -- label/owner, for humans
  active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);
```

Validation query: `SELECT active FROM api_keys WHERE key_hash = $1`. A key is
valid when a row exists and `active` is true. Revoking sets `active = false` and
`revoked_at = now()`.

## Configuration

All configuration is via environment variables:

| Variable                 | Required | Default  | Purpose                                  |
|--------------------------|----------|----------|------------------------------------------|
| `OPENTELA_UPSTREAM_URL`  | yes      | —        | Base URL requests are forwarded to       |
| `DATABASE_URL`           | yes      | —        | Postgres connection DSN                  |
| `LISTEN_ADDR`            | no       | `:8080`  | Address the HTTP server listens on       |
| `CACHE_TTL`              | no       | `336h`   | TTL for validated (positive) keys (14d)  |
| `CACHE_NEGATIVE_TTL`     | no       | `30s`    | TTL for invalid (negative) results       |
| `CACHE_JANITOR_INTERVAL` | no       | `10m`    | How often expired entries are swept      |

## CLI (`keyctl`)

- `keyctl migrate` — apply `migrations/0001_init.sql` to `DATABASE_URL`.
- `keyctl add [--name <label>]` — generate a random token, store its hash, print
  the plaintext token once.
- `keyctl revoke <token|hash>` — set `active = false`, `revoked_at = now()`.
- `keyctl list` — list keys (hash prefix, name, active, timestamps).

## Testing (TDD)

Written test-first, per unit:

- **cache** — positive/negative TTL expiry, concurrent access, janitor eviction.
- **auth.Validator** — cache hit skips store; cache miss hits store then caches;
  negative results cached with the short TTL.
- **auth middleware** — missing header, malformed header, invalid key → `401`;
  valid key → next handler invoked.
- **proxy** — against an `httptest` upstream: path/query/header/body
  preservation, and a **streaming** case verifying incremental flushing.
- **store (Postgres)** — one integration test gated behind `TEST_DATABASE_URL`;
  skipped when unset.

## Dependencies

- `github.com/jackc/pgx/v5` (with `pgxpool`) — Postgres driver.
- Standard library for everything else (`net/http`, `net/http/httputil`,
  `net/http/httptest`, `testing`, `crypto/sha256`).

## Module

`github.com/opentela-ai/api` (adjustable).
