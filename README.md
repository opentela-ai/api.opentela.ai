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
go run ./cmd/keyctl migrate            # apply migrations/0001_init.sql
go run ./cmd/keyctl add [--name NAME]  # create a key, print token once
go run ./cmd/keyctl revoke <token>     # deactivate a key
go run ./cmd/keyctl list               # list keys (hash prefix, name, status)
```

## Test

```bash
go test ./...                          # unit tests (Postgres test skips)
TEST_DATABASE_URL=postgres://... go test ./internal/store/  # + integration
```
