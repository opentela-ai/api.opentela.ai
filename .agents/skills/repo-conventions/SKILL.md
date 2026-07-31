---
name: repo-conventions
description: Architecture, package map, coding conventions, and build/test commands for the api.opentela.ai Go repository (opentela authenticating reverse proxy). Load this whenever making any code change in this repository.
---

# Repo conventions — api.opentela.ai

Go 1.26, module `github.com/opentela-ai/api`. An authenticating reverse proxy
in front of the opentela mesh. Clients send `Authorization: Bearer <key>`;
valid keys (Postgres-backed, cached in memory) are forwarded to the upstream.

## Commands

```bash
go build ./...                       # build
go test ./...                        # unit tests (hermetic; Postgres tests self-skip)
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # + integration tests
go run ./cmd/server                  # run (needs OPENTELA_UPSTREAM_URL + DATABASE_URL)
go run ./cmd/keyctl migrate          # apply migrations/*.sql in order
```

## Layout

- `cmd/server` — main; wires config → store → planes.
- `cmd/keyctl` — admin CLI: `migrate`, `add`, `revoke`, `list`.
- `internal/server` — top-level mux: `/healthz`, `/manage/`, `/internal/…`,
  `GET /v1/services`, then the API-key-gated proxy catch-all.
- `internal/proxy` — reverse proxy to `OPENTELA_UPSTREAM_URL`.
- `internal/auth` — API-key middleware + token validator (cache → Postgres).
- `internal/keysvc`, `internal/keysapi` — admin/self-service key issuing.
- `internal/manageapi` — router composing the `/manage/*` self-service plane
  (keys, wallets, instances, regions) behind the Neon Auth JWT middleware.
- `internal/principal`, `internal/neonauth` — JWT principal + Neon Auth verifier.
- `internal/walletsapi`, `internal/solana` — wallet linking (base58 Ed25519
  challenge/response).
- `internal/instancesapi`, `internal/regionsapi` — instance claims and
  trusted-region membership behind `/manage/*`.
- `internal/aclapi` — internal ACL evaluator endpoints (`/internal/acl/evaluate*`),
  bearer-token gated; never for browsers.
- `internal/nodecred` — trusted-node JWT challenge/issuance.
- `internal/mesh`, `internal/catalog` — upstream mesh client and the
  permissionless `GET /v1/services` catalogue.
- `internal/store` — pgx/v5 Postgres layer; `migrations/*.sql` hold the DDL.
- `internal/cache`, `internal/corsmw`, `internal/httputil`, `internal/config`,
  `internal/identity` — small shared utilities.

## Conventions

- Stdlib `net/http` only — no web framework. Routes use Go 1.22+ method
  patterns (`mux.HandleFunc("POST /manage/keys", …)`) or subtree mounts.
- JSON in/out via `internal/httputil`: `DecodeStrict` (size-capped,
  `DisallowUnknownFields`, rejects trailing values) and `WriteJSON`.
- Errors are mapped explicitly in handlers: domain sentinel errors → specific
  status codes (e.g. `ErrTooManyKeys` → 409); unexpected errors → 503 with a
  generic message. Never leak internals or secrets in responses.
- Handlers depend on small local interfaces; add a compile-time assertion
  (`var _ Iface = (*Concrete)(nil)`) where the concrete type must satisfy one.
- Config comes exclusively from env vars, parsed once in `internal/config`
  with documented defaults and fail-fast validation of paired settings.
- Everything user-facing is scoped to the authenticated principal; handlers
  must scope queries by the caller's subject (cross-user access → 404).
- Package-level doc comments explain intent; keep `README.md` in sync when
  endpoints, env vars, or ACL rule kinds change.

## Security invariants (do not break)

- Fail closed: unknown/expired identity, unverifiable attestation, or stale
  ownership ⇒ deny.
- The raw API key is never sent to internal ACL endpoints — only its sha256 hash.
- Region membership is API-authoritative; mesh/libp2p presence never grants
  trusted access. Unmanaged peers are denied in evaluate-v2.
- `INTERNAL_CONTROL_TOKEN` gates `/internal/*`; Neon Auth JWT gates `/manage/*`;
  opentela API keys gate everything else.
- Secrets (tokens, keys) are printed/returned exactly once at creation and
  stored hashed.
