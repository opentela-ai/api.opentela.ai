FROM postgres:18-alpine
ENTRYPOINT []
RUN apk add --no-cache postgresql-client >/dev/null 2>&1 || true
COPY pgcli.sh /pgcli.sh
RUN chmod +x /pgcli.sh
CMD ["/pgcli.sh"]
