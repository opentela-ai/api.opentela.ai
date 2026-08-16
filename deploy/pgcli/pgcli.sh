#!/bin/sh
# Mark all neon_auth.user rows as email-verified, transactionally, with before/after proof.
set -e
say() { printf '\n===== %s =====\n' "$*"; }

say "BEFORE: neon_auth.user emailVerified state"
psql -v ON_ERROR_STOP=1 "$NEON_DSN" -c 'SELECT email, "emailVerified" FROM neon_auth.user ORDER BY "createdAt";'

say "UPDATE (in a transaction; COMMIT at the end)"
psql -v ON_ERROR_STOP=1 "$NEON_DSN" <<'SQL'
BEGIN;
-- Count rows we expect to update, for the verification line.
SELECT count(*) AS rows_to_update FROM neon_auth.user WHERE "emailVerified" IS DISTINCT FROM true;

UPDATE neon_auth.user
   SET "emailVerified" = true,
       "updatedAt"     = now()
 WHERE "emailVerified" IS DISTINCT FROM true;

-- Within the same xact: confirm the new state BEFORE committing.
SELECT count(*) AS now_verified_true  FROM neon_auth.user WHERE "emailVerified" = true;
SELECT count(*) AS now_verified_false FROM neon_auth.user WHERE "emailVerified" = false;

COMMIT;
SQL

say "AFTER (fresh read, post-commit): neon_auth.user emailVerified state"
psql -v ON_ERROR_STOP=1 "$NEON_DSN" -c 'SELECT email, "emailVerified", "updatedAt" FROM neon_auth.user ORDER BY "createdAt";'

say "UPDATE-COMPLETE — staying alive 30 min for log re-reads"
exec sleep 1800
