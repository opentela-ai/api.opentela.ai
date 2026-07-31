---
name: deploy-flyio
description: How to build, configure, and deploy api.opentela.ai to Fly.io. Use when deploying, rotating/setting secrets, checking production health, or changing fly.toml or the Dockerfile.
---

# Deploying to Fly.io

App `opentela-api`, primary region `fra` (nearest to the Neon DB in
eu-central-1). Config: `fly.toml`; build: multi-stage `Dockerfile` producing
static `server` + `keyctl` binaries on alpine.

## Secrets (never commit)

App config is env-only; production values live in Fly secrets:

```bash
flyctl secrets set \
  DATABASE_URL=postgres://... \
  OPENTELA_UPSTREAM_URL=https://... \
  NEON_AUTH_JWKS_URL=https://... \
  NEON_AUTH_ISSUER=https://... \
  INTERNAL_CONTROL_TOKEN=<min 32 bytes>
# optional, paired: NODE_CREDENTIAL_SIGNING_KID + NODE_CREDENTIAL_SIGNING_KEY
flyctl secrets list
```

Paired settings must be set together or the app refuses to start (see the
README env tables and `internal/config`).

## Deploy

```bash
flyctl deploy                 # build + push; release_command runs migrations first
flyctl status                 # machine state
flyctl logs                   # runtime logs
curl -fsS https://opentela-api.fly.dev/healthz   # expected: ok
```

- `release_command = "/app/keyctl migrate"` applies `migrations/*.sql` before
  the release goes live — see the `database-migration` skill for the
  additive-only compatibility rules.
- Health check: `GET /healthz` every 15s (must stay unauthenticated and cheap).
- `min_machines_running = 0` with auto-stop/start: cold starts are expected on
  the first request after idle.

## Local verification before deploying

```bash
go build ./... && go test ./...
docker build -t opentela-api .   # optional: confirm the image builds
```

## Rollback

```bash
flyctl releases                 # list
flyctl releases rollback <version>
```

Migrations are not rolled back automatically — additive-only schema changes
mean an old release keeps working against the newer schema.
