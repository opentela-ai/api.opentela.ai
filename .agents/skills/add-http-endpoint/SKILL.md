---
name: add-http-endpoint
description: How to add a new HTTP endpoint to the api.opentela.ai service. Use when adding routes to the /manage/* self-service plane, the /internal/* control plane, the public /v1/services surface, or modifying proxy behavior.
---

# Adding an HTTP endpoint

## 1. Pick the correct plane

The top-level mux lives in `internal/server/server.go`
(`NewWithControlPlanes`). Auth requirements differ per plane — choosing wrong
is a security bug:

| Plane | Path | Auth | Package to extend |
|---|---|---|---|
| Self-service management | `/manage/…` | Neon Auth JWT, scoped to `sub` | `keysapi` / `walletsapi` / `instancesapi` / `regionsapi`, composed by `manageapi.Router` |
| Internal control | `/internal/…` | `INTERNAL_CONTROL_TOKEN` bearer (or node JWT); never browsers | `aclapi`, `nodecred` |
| Permissionless | `GET /v1/services` only | none | `catalog` |
| Proxy | everything else | opentela API key | `proxy`, `auth` |

## 2. Write the handler

Follow the existing style (see `internal/keysapi/handlers.go`):

- Register with method patterns: `mux.HandleFunc("POST /manage/foo", …)`.
- Every plane package exposes `Routes() http.Handler`; shared middleware is
  applied by the *caller* (`manageapi/router.go` or `server.go`), not inside.
- Decode with `httputil.DecodeStrict(w, r, maxBodyBytes, &req)` — define a
  per-handler `maxBodyBytes` const and validate lengths.
- Get the caller from context (`principal` / the package-local `UserID`
  helper); scope every store call to it. Other users' resources → `404`.
- Map sentinel errors with `errors.Is` to precise statuses (400/404/409/422);
  unknown errors → `503` generic. Use `httputil.WriteJSON` for responses.
- Reuse `store` types for persistence; if you need new SQL, see the
  `database-migration` skill.

## 3. Wire it

- `/manage/*`: mount in `internal/manageapi/router.go` behind the existing
  CORS + principal middleware (guard with a nil-check like the other planes do).
- `/internal/*`: mount in `internal/server/server.go`.
- New env config goes through `internal/config` (fail-fast validation, paired
  settings must be set together — mirror the `NODE_CREDENTIAL_*` precedent).

## 4. Test

- Add `*_test.go` next to the handler, using `httptest` against the package's
  `Routes()` with a fake `Service`. Cover: auth missing/wrong, scoping
  (other user's id → 404), validation errors, and the happy path status/body.
- Run `go test ./...` — must stay hermetic (no DB/network in unit tests).

## 5. Document

Update `README.md`'s endpoint list and env-var tables, and
`docs/frontend-integration.md` if browsers call it.
