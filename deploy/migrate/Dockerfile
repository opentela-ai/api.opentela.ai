# Throwaway one-shot data migration runner: Neon app DB -> Railway Postgres.
# Built on postgres:18-alpine so pg_dump/psql match the Railway target (PG18)
# and can read the Neon source. NOT used at runtime by the api service.
# Safe to delete (railway service delete -s migrate --yes) after migration.
FROM postgres:18-alpine

# We run a script, not a database server — drop the postgres entrypoint.
ENTRYPOINT []

COPY migrate.sh /migrate.sh
RUN chmod +x /migrate.sh

CMD ["/migrate.sh"]
