#!/bin/sh
# One-shot data migration: Neon app DB (NEON_SOURCE_DSN) -> Railway Postgres (TARGET_DSN).
#
# Strategy
#  * pg_dump --data-only of the public-schema tables that exist on BOTH sides.
#  * Load into the target with session_replication_role = replica so the
#    internal foreign keys don't force a particular insert order (the dump
#    already holds a consistent point-in-time snapshot).
#  * Re-advance every BIGSERIAL sequence to max(id) so new rows don't collide.
#
# Idempotent: it TRUNCATEs the target tables (RESTART IDENTITY) before each
# load, so a re-run (e.g. after a crash/restart) is safe and produces the same
# end state. Prints source vs target row counts for verification, then stays
# alive so the deployment isn't marked crashed before the logs are read.
set -eu

SOURCE="${NEON_SOURCE_DSN:?NEON_SOURCE_DSN not set}"
TARGET="${TARGET_DSN:?TARGET_DSN not set}"

say() { printf '\n========== %s ==========\n' "$*"; }

say "versions"
echo "pg_dump: $(pg_dump --version)"
echo "psql:    $(psql --version)"

say "connectivity"
psql -v ON_ERROR_STOP=1 -At "$SOURCE" -c "select 'source: db='||current_database()||' server='||version();" \
  || { echo "SOURCE CONNECT FAILED"; exit 2; }
psql -v ON_ERROR_STOP=1 -At "$TARGET" -c "select 'target: db='||current_database()||' server='||version();" \
  || { echo "TARGET CONNECT FAILED"; exit 3; }

say "source public-schema tables"
TABLES=$(psql -v ON_ERROR_STOP=1 -At "$SOURCE" \
  -c "select tablename from pg_tables where schemaname='public' order by 1;")
echo "$TABLES" | sed 's/^/  /'

say "tables to migrate (exist on BOTH source and target)"
KEEP=""
for t in $TABLES; do
  if psql -v ON_ERROR_STOP=1 -At "$TARGET" \
       -c "select 1 from pg_tables where schemaname='public' and tablename='$t'" \
       | grep -qx 1; then
    KEEP="$KEEP $t"
    echo "  + $t"
  else
    echo "  - $t  (not present on target — skipped)"
  fi
done
[ -n "$KEEP" ] || { echo "NO TABLES TO MIGRATE"; exit 4; }

say "column-list parity check (guard against schema drift)"
MISMATCH=0
for t in $KEEP; do
  s=$(psql -v ON_ERROR_STOP=1 -At "$SOURCE" \
      -c "select string_agg(attname,',' order by attnum) from pg_attribute where attrelid='public.$t'::regclass and attnum>0 and not attisdropped;")
  g=$(psql -v ON_ERROR_STOP=1 -At "$TARGET" \
      -c "select string_agg(attname,',' order by attnum) from pg_attribute where attrelid='public.$t'::regclass and attnum>0 and not attisdropped;")
  if [ "$s" != "$g" ]; then
    echo "  MISMATCH $t"
    echo "    source: $s"
    echo "    target: $g"
    MISMATCH=1
  else
    echo "  ok       $t"
  fi
done
[ "$MISMATCH" = "0" ] || { echo "COLUMN MISMATCH — aborting before load"; exit 6; }

say "source row counts"
for t in $KEEP; do
  printf '  %-34s ' "$t"
  psql -v ON_ERROR_STOP=1 -At "$SOURCE" -c "select count(*) from public.$t;" || echo "ERR"
done

say "truncate target (RESTART IDENTITY, all FKs are internal so no CASCADE)"
TRUNC=""
for t in $KEEP; do
  if [ -z "$TRUNC" ]; then TRUNC="$t"; else TRUNC="$TRUNC,$t"; fi
done
psql -v ON_ERROR_STOP=1 "$TARGET" -c "TRUNCATE $TRUNC RESTART IDENTITY;"

say "dump (data-only) + load (session_replication_role=replica)"
DUMPARGS=""
for t in $KEEP; do
  DUMPARGS="$DUMPARGS -t public.$t"
done
{
  echo 'SET session_replication_role = replica;'
  pg_dump --data-only --no-owner --no-privileges $DUMPARGS "$SOURCE"
  echo 'SET session_replication_role = DEFAULT;'
} | psql -v ON_ERROR_STOP=1 "$TARGET"

say "advance BIGSERIAL sequences to max(id)"
psql -v ON_ERROR_STOP=1 "$TARGET" <<'SQL'
DO $$
DECLARE r record; m bigint; seq text;
BEGIN
  FOR r IN
    SELECT table_schema, table_name, column_name
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND column_default LIKE 'nextval%'
    ORDER BY table_name, column_name
  LOOP
    EXECUTE format('SELECT COALESCE(max(%I), 0) FROM %I.%I',
                   r.column_name, r.table_schema, r.table_name) INTO m;
    seq := pg_get_serial_sequence(r.table_schema || '.' || r.table_name, r.column_name);
    IF seq IS NOT NULL AND m > 0 THEN
      PERFORM setval(seq, m);
      RAISE NOTICE 'setval % -> %', seq, m;
    END IF;
  END LOOP;
END
$$;
SQL

say "target row counts (verify against source above)"
for t in $KEEP; do
  printf '  %-34s ' "$t"
  psql -v ON_ERROR_STOP=1 -At "$TARGET" -c "select count(*) from public.$t;" || echo "ERR"
done

say "target sequence values"
psql -v ON_ERROR_STOP=1 -At "$TARGET" \
  -c "select schemaname||'.'||sequencename, coalesce(last_value,0) as last_value from pg_sequences order by 1;" \
  | sed 's/^/  /'

say "MIGRATION COMPLETE — staying alive so logs can be read (delete this service when done)"
exec sleep 86400
