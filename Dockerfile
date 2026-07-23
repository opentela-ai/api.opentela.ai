# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binaries; strip debug info. Build both the server and the keyctl CLI
# (keyctl runs migrations as the Fly release_command before each deploy).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/keyctl ./cmd/keyctl

# --- runtime stage ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/keyctl /app/keyctl
# keyctl migrate globs ./migrations/*.sql relative to WORKDIR.
COPY migrations /app/migrations
USER app
EXPOSE 8080
ENV LISTEN_ADDR=:8080
# CMD (not ENTRYPOINT): Fly's release_command replaces the command entirely, so
# `/app/keyctl migrate` runs on its own instead of being appended to an entrypoint.
CMD ["/app/server"]
