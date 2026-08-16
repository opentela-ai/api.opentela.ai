---
name: database-migration
description: How to change the Postgres schema in the api.opentela.ai repository. Use when adding/altering tables, columns, indexes, or constraints, or when store-layer SQL must change to match.
---

# Database migrations

Schema lives in `migrations/NNNN_description.sql`, applied in lexicographic
order by `keyctl migrate` (which globs `./migrations/*.sql` relative to its
working directory).

## Workflow

1. **Create the next file**, e.g. `migrations/0005_short_snake_name.sql`.
   Never edit or renumber an already-committed migration — append only.

2. **Write idempotent DDL** — deploys and local dev re-run migrations, and
   `release_command` runs `keyctl migrate` before every Fly release:
   - `CREATE TABLE IF NOT EXISTS …`
   - `ALTER TABLE … ADD COLUMN IF NOT EXISTS …`
   - `DROP CONSTRAINT IF EXISTS …` before re-adding a changed `CHECK`
   - `CREATE INDEX IF NOT EXISTS …`
   Follow `migrations/0004_trusted_regions_service_policy.sql` as the template:
   `BIGSERIAL` ids, `TIMESTAMPTZ NOT NULL DEFAULT now()` timestamps, named
   `CHECK` constraints for enum-like text columns, foreign keys with explicit
   `ON DELETE` behavior.

3. **Update the store layer** in `internal/store/` (`store.go`, `acl.go`,
   `policy.go`, `postgres.go`). Keep SQL there in sync; pgx/v5 is used
   directly — no ORM.

4. **Test both ways**:
   ```bash
   # local apply (requires DATABASE_URL)
   go run ./cmd/keyctl migrate

   # integration tests (skip silently without TEST_DATABASE_URL)
   TEST_DATABASE_URL=postgres://... go test ./internal/store/
   ```
   `internal/store/postgres_test.go`'s `newTestStore` drops and recreates the
   schema — add any new tables to its DROP list.

5. **Cross-check consumers**: ACL evaluation (`internal/aclapi`), instance and
   region APIs, and node credentials all read this schema; run
   `go test ./...` after schema changes.

## Deploy notes

- Railway runs `/app/keyctl migrate` as `preDeployCommand` before each release
  (see `railway.json`), so migrations must be backward-compatible with the
  *previous* app version during rolling deploys (additive changes only:
  new nullable columns or columns with defaults, no drops/renames without a
  staged rollout).
- Neon is the production Postgres; connection string comes from `DATABASE_URL`
  (`.env.local` is gitignored for local use).
