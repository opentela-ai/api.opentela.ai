---
name: deploy-railway
description: How to build, configure, and deploy api.opentela.ai to Railway. Use when deploying, rotating/setting variables, checking production health, or changing railway.json or the Dockerfile.
---

# Deploying to Railway

Three Railway services, each built from its own Dockerfile via the Railway
`DOCKERFILE` builder:

- **API** (repo root, `railway.json`): the authenticating reverse proxy.
  `preDeployCommand = /app/keyctl migrate` applies `migrations/*.sql` in order
  before each release goes live; `healthcheckPath = /healthz` must stay
  unauthenticated and cheap.
- **migrate** (`deploy/migrate/`, one-shot): `startCommand = /migrate.sh`
  loads Neon app-DB data into the Railway Postgres (`pg_dump --data-only`,
  `session_replication_role = replica`, then re-advances sequences).
- **pgcli** (`deploy/pgcli/`): `startCommand = /pgcli.sh` keeps a `psql` shell
  alive for ad-hoc inspection of the production Postgres.

## Variables (never commit)

App config is env-only; production values live in Railway service variables.
CLI (v4); setting a variable triggers a redeploy:

```bash
railway login
railway link                 # associate this dir with the Railway project/service

railway variables --set \
  DATABASE_URL=postgres://... \
  OPENTELA_UPSTREAM_URL=https://... \
  NEON_AUTH_JWKS_URL=https://... \
  NEON_AUTH_ISSUER=https://... \
  INTERNAL_CONTROL_TOKEN=<min 32 bytes>
# optional, paired: NODE_CREDENTIAL_SIGNING_KID + NODE_CREDENTIAL_SIGNING_KEY
railway variables            # list (add -k for KEY=value, --json for machine)
railway variables -s <service> --set CORS_ALLOWED_ORIGINS="https://app.opentela.ai"
```

Paired settings must be set together or the app refuses to start (see the
README env tables and `internal/config`). Manage the same values from the
Railway dashboard (`railway open`) when batch-editing.

## Deploy

```bash
railway up                   # build + deploy from the current directory
railway up -d                # detach (don't attach to the log stream)
railway status               # linked project/service/environment
railway logs                 # runtime logs of the active deployment
curl -fsS https://<your-domain>/healthz   # expected: ok
```

- `preDeployCommand = /app/keyctl migrate` (see `railway.json`) applies
  `migrations/*.sql` before the release goes live — additive-only rules apply
  (see the `database-migration` skill).
- `healthcheckPath = /healthz`: Railway watches this path and restarts the
  service on failure; it must remain unauthenticated and cheap.
- `deploy/migrate` and `deploy/pgcli` are deployed independently from their
  own directories (`cd deploy/migrate && railway up -s migrate`), not from root.

## Local verification before deploying

```bash
go build ./... && go test ./...
docker build -t opentela-api .   # optional: confirm the image builds
```

## Rollback

```bash
railway redeploy -s <service>     # redeploy the latest deployment
railway down                      # remove the most recent deployment
```

For redeploying an *earlier* deployment (true rollback), use the Railway
dashboard (`railway open`) — pick the prior deployment and redeploy it.
Migrations are not rolled back automatically — additive-only schema changes
mean an old release keeps working against the newer schema.
