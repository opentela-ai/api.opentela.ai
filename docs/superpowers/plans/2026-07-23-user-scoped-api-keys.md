# User-Scoped API Keys via Neon Auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `/manage/keys` HTTP plane that lets a Neon-Auth-authenticated user create, list, and revoke their own opentela API keys.

**Architecture:** A second auth plane, separated from the existing proxy purely by URL path. Requests to `/manage/keys*` carry a Neon Auth Ed25519 JWT, verified with stdlib `crypto/ed25519` against the project's JWKS; the extracted `sub` becomes the key owner. Keys live in the same `api_keys` table (new `user_id` + `key_prefix` columns), stored as SHA-256 hashes with a non-secret display prefix. The proxy plane and its 14-day cache are untouched. The whole plane is mounted only when `NEON_AUTH_JWKS_URL` + `NEON_AUTH_ISSUER` are configured.

**Tech Stack:** Go 1.26, `net/http` (stdlib mux with method+wildcard patterns), `crypto/ed25519`, `crypto/rand`, `crypto/sha256`, `github.com/jackc/pgx/v5` (existing). No new third-party dependencies.

## Global Constraints

- Module path: `github.com/opentela-ai/api`.
- **No new third-party dependencies.** Only `pgx/v5` (already present) + stdlib.
- Keys are stored as SHA-256 hex hashes only; plaintext is shown exactly once, at creation. Never log token/JWT contents, hashes, or key plaintext.
- The proxy plane (`internal/auth`, `internal/cache`, `internal/proxy`) and its behavior must remain unchanged. Existing tests must stay green.
- The key-management plane is **optional**: mounted only when both `NEON_AUTH_JWKS_URL` and `NEON_AUTH_ISSUER` are set; otherwise the binary is exactly today's pure proxy and `/manage/*` is absent (404).
- JWT verification enforces `alg == "EdDSA"` (reject `none` and every other alg), signature via `ed25519.Verify`, `exp` present and unexpired, `iss == NEON_AUTH_ISSUER`, and `aud` only when `NEON_AUTH_AUDIENCE` is set. A bad/expired/forged token is always `401`, never `5xx`.
- `key_prefix` = `"sk-"` + the first 8 hex chars of the token (i.e. `token[:11]`).
- Error wrapping in `store` follows the existing `fmt.Errorf("store: <op>: %w", err)` convention.
- Follow existing code style: package doc comments, focused files, table-driven tests, pristine test output.
- Run `gofmt -l .`, `go vet ./...`, `go build ./...`, and `go test ./...` clean before each commit. Store integration tests are gated on `TEST_DATABASE_URL` and SKIP when unset (existing pattern) — that is expected in hermetic runs.

---

## File Structure

**New files:**
- `migrations/0002_user_keys.sql` — additive schema for owner + prefix.
- `internal/keysvc/keysvc.go` — token generation, prefix, and the Create/List/Revoke service with the per-user cap.
- `internal/keysvc/keysvc_test.go` — unit tests with a fake store (no DB).
- `internal/neonauth/verifier.go` — Ed25519 JWT verifier + JWKS cache.
- `internal/neonauth/verifier_test.go` — unit tests with generated keypairs + httptest JWKS.
- `internal/keysapi/middleware.go` — JWT auth middleware + user-id context + CORS.
- `internal/keysapi/middleware_test.go`
- `internal/keysapi/handlers.go` — the three JSON handlers + `Router`.
- `internal/keysapi/handlers_test.go`

**Modified files:**
- `internal/store/store.go` — extend `KeyInfo`.
- `internal/store/postgres.go` — add `InsertUserKey`, `ListByUser`, `RevokeByIDForUser`, `CountActiveByUser`.
- `internal/store/postgres_test.go` — apply `0002`, integration-test the new methods.
- `internal/config/config.go` — Neon Auth + CORS + cap config, `intEnv` helper.
- `internal/config/config_test.go` — cover new config.
- `internal/server/server.go` — mount `/manage/` when a handler is provided.
- `internal/server/server_test.go` — update `New` callers; test routing.
- `cmd/server/main.go` — wire the plane when configured.
- `cmd/keyctl/main.go` — use `keysvc.GenerateToken`; apply all migrations.
- `README.md` — document the endpoints and config.

Dependency order of tasks: 1 (store/db) → 2 (keysvc) and 3 (neonauth) are independent → 4 (keysapi middleware/cors) needs 3 → 5 (keysapi handlers) needs 2+4 → 6 (config) independent → 7 (wiring + keyctl + README) needs all.

---

### Task 1: Schema + store methods for user-owned keys

**Files:**
- Create: `migrations/0002_user_keys.sql`
- Modify: `internal/store/store.go` (the `KeyInfo` struct)
- Modify: `internal/store/postgres.go` (append four methods)
- Test: `internal/store/postgres_test.go` (extend setup + add a test)

**Interfaces:**
- Consumes: existing `store.HashKey`, `Postgres`, `KeyInfo`.
- Produces:
  - `KeyInfo` gains `ID int64`, `Prefix string`, `UserID *string`.
  - `func (p *Postgres) InsertUserKey(ctx context.Context, userID, keyHash, name, prefix string) (KeyInfo, error)`
  - `func (p *Postgres) ListByUser(ctx context.Context, userID string) ([]KeyInfo, error)`
  - `func (p *Postgres) RevokeByIDForUser(ctx context.Context, userID string, id int64) (bool, error)`
  - `func (p *Postgres) CountActiveByUser(ctx context.Context, userID string) (int, error)`

- [ ] **Step 1: Write the migration**

Create `migrations/0002_user_keys.sql`:

```sql
-- Adds ownership + a non-secret display prefix to api_keys.
-- Additive and idempotent so it can run alongside 0001 on any branch.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS user_id    TEXT;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_prefix TEXT;

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id
    ON api_keys (user_id) WHERE user_id IS NOT NULL;
```

- [ ] **Step 2: Extend `KeyInfo`**

In `internal/store/store.go`, replace the `KeyInfo` struct with:

```go
// KeyInfo is a row of the api_keys table. ID, Prefix, and UserID are populated
// by the user-scoped queries; the admin List (by hash) leaves them zero.
type KeyInfo struct {
	ID        int64
	KeyHash   string
	UserID    *string
	Name      string
	Prefix    string
	Active    bool
	CreatedAt time.Time
	RevokedAt *time.Time
}
```

- [ ] **Step 3: Write the failing integration test**

In `internal/store/postgres_test.go`, extend `newTestStore` to also apply `0002` (insert before the final `return p`, after the `0001` block):

```go
	ddl2, err := os.ReadFile("../../migrations/0002_user_keys.sql")
	if err != nil {
		t.Fatalf("read migration 0002: %v", err)
	}
	if err := p.Migrate(ctx, string(ddl2)); err != nil {
		t.Fatalf("migrate 0002: %v", err)
	}
```

Then add this test:

```go
func TestPostgresUserKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	const alice, bob = "user-alice", "user-bob"

	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(empty) = (%d,%v), want (0,nil)", n, err)
	}

	info, err := p.InsertUserKey(ctx, alice, HashKey("tok-a"), "laptop", "sk-1a2b3c4d")
	if err != nil {
		t.Fatalf("InsertUserKey: %v", err)
	}
	if info.ID == 0 || info.Name != "laptop" || info.Prefix != "sk-1a2b3c4d" || !info.Active {
		t.Fatalf("InsertUserKey returned %+v", info)
	}
	if info.CreatedAt.IsZero() {
		t.Fatal("InsertUserKey CreatedAt is zero")
	}

	// The inserted key validates through the existing proxy path.
	if ok, err := p.Validate(ctx, HashKey("tok-a")); err != nil || !ok {
		t.Fatalf("Validate(new user key) = (%v,%v), want (true,nil)", ok, err)
	}

	// Listing is owner-scoped and never leaks another user's keys.
	if _, err := p.InsertUserKey(ctx, bob, HashKey("tok-b"), "bob-key", "sk-9999abcd"); err != nil {
		t.Fatalf("InsertUserKey(bob): %v", err)
	}
	rows, err := p.ListByUser(ctx, alice)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != info.ID || rows[0].Prefix != "sk-1a2b3c4d" {
		t.Fatalf("ListByUser(alice) = %+v, want only alice's key", rows)
	}

	// Bob cannot revoke Alice's key.
	if changed, err := p.RevokeByIDForUser(ctx, bob, info.ID); err != nil || changed {
		t.Fatalf("RevokeByIDForUser(bob, alice's id) = (%v,%v), want (false,nil)", changed, err)
	}
	// Alice can.
	if changed, err := p.RevokeByIDForUser(ctx, alice, info.ID); err != nil || !changed {
		t.Fatalf("RevokeByIDForUser(alice) = (%v,%v), want (true,nil)", changed, err)
	}
	// Revoked keys drop out of the active count.
	if n, err := p.CountActiveByUser(ctx, alice); err != nil || n != 0 {
		t.Fatalf("CountActiveByUser(after revoke) = (%d,%v), want (0,nil)", n, err)
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `TEST_DATABASE_URL="$DATABASE_URL" go test ./internal/store/ -run TestPostgresUserKeyLifecycle -v`
(Load env first: `set -a; . ./.env.local; set +a`. If no DB is available it SKIPs — in that case confirm it at least fails to compile because the four methods don't exist yet: `go build ./internal/store/` → FAIL "undefined: ... InsertUserKey".)
Expected: FAIL (compile error `p.InsertUserKey undefined`, etc.).

- [ ] **Step 5: Implement the four methods**

Append to `internal/store/postgres.go`:

```go
// InsertUserKey adds a new active key owned by userID and returns the stored row.
func (p *Postgres) InsertUserKey(ctx context.Context, userID, keyHash, name, prefix string) (KeyInfo, error) {
	var info KeyInfo
	err := p.pool.QueryRow(ctx,
		`INSERT INTO api_keys (user_id, key_hash, name, key_prefix, active)
		 VALUES ($1, $2, $3, $4, TRUE)
		 RETURNING id, created_at`,
		userID, keyHash, name, prefix).Scan(&info.ID, &info.CreatedAt)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("store: insert user key: %w", err)
	}
	uid := userID
	info.UserID = &uid
	info.KeyHash = keyHash
	info.Name = name
	info.Prefix = prefix
	info.Active = true
	return info, nil
}

// ListByUser returns userID's keys (active and revoked), newest first. It never
// returns the key hash beyond the display prefix.
func (p *Postgres) ListByUser(ctx context.Context, userID string) ([]KeyInfo, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, COALESCE(name, ''), COALESCE(key_prefix, ''), active, created_at, revoked_at
		 FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list by user query: %w", err)
	}
	defer rows.Close()

	uid := userID
	var out []KeyInfo
	for rows.Next() {
		k := KeyInfo{UserID: &uid}
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: list by user scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list by user rows: %w", err)
	}
	return out, nil
}

// RevokeByIDForUser marks one of userID's keys inactive. It reports whether a row
// changed; a mismatched owner or unknown id changes nothing (reported as false).
func (p *Postgres) RevokeByIDForUser(ctx context.Context, userID string, id int64) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = now()
		 WHERE id = $1 AND user_id = $2 AND active = TRUE`, id, userID)
	if err != nil {
		return false, fmt.Errorf("store: revoke by id: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountActiveByUser returns how many active keys userID currently holds.
func (p *Postgres) CountActiveByUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE user_id = $1 AND active = TRUE`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `set -a; . ./.env.local; set +a; TEST_DATABASE_URL="$DATABASE_URL" go test ./internal/store/ -v`
Expected: PASS (both lifecycle tests). If no DB: `go build ./... && go vet ./internal/store/` clean, tests SKIP.

- [ ] **Step 7: Commit**

```bash
gofmt -w internal/store migrations
git add migrations/0002_user_keys.sql internal/store/
git commit -m "feat(store): user-owned keys (insert/list/revoke/count) + migration 0002"
```

---

### Task 2: `keysvc` — token generation and the key service

**Files:**
- Create: `internal/keysvc/keysvc.go`
- Test: `internal/keysvc/keysvc_test.go`

**Interfaces:**
- Consumes: `store.HashKey`, `store.KeyInfo` (Task 1 fields).
- Produces:
  - `func GenerateToken() (string, error)` → `"sk-"` + 48 hex chars.
  - `func Prefix(token string) string` → `token[:11]` (guards short input).
  - `type Store interface { InsertUserKey(...) (store.KeyInfo, error); CountActiveByUser(...) (int, error); ListByUser(...) ([]store.KeyInfo, error); RevokeByIDForUser(...) (bool, error) }`
  - `func New(s Store, maxPerUser int) *Service`
  - `var ErrTooManyKeys = errors.New("keysvc: key limit reached")`
  - `func (s *Service) Create(ctx, userID, name string) (plaintext string, info store.KeyInfo, err error)`
  - `func (s *Service) List(ctx, userID string) ([]store.KeyInfo, error)`
  - `func (s *Service) Revoke(ctx, userID string, id int64) (bool, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/keysvc/keysvc_test.go`:

```go
package keysvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/store"
)

// fakeStore is an in-memory Store for unit tests.
type fakeStore struct {
	active     int
	insertErr  error
	lastUserID string
	lastHash   string
	lastPrefix string
}

func (f *fakeStore) InsertUserKey(_ context.Context, userID, keyHash, name, prefix string) (store.KeyInfo, error) {
	if f.insertErr != nil {
		return store.KeyInfo{}, f.insertErr
	}
	f.lastUserID, f.lastHash, f.lastPrefix = userID, keyHash, prefix
	f.active++
	uid := userID
	return store.KeyInfo{ID: 1, UserID: &uid, Name: name, Prefix: prefix, Active: true}, nil
}
func (f *fakeStore) CountActiveByUser(context.Context, string) (int, error) { return f.active, nil }
func (f *fakeStore) ListByUser(context.Context, string) ([]store.KeyInfo, error) {
	return []store.KeyInfo{{ID: 1}}, nil
}
func (f *fakeStore) RevokeByIDForUser(context.Context, string, int64) (bool, error) { return true, nil }

func TestGenerateTokenFormat(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(tok, "sk-") || len(tok) != 3+48 {
		t.Fatalf("token %q has wrong shape", tok)
	}
	if Prefix(tok) != tok[:11] {
		t.Fatalf("Prefix(%q) = %q", tok, Prefix(tok))
	}
	if a, _ := GenerateToken(); a == tok {
		t.Fatal("GenerateToken returned identical tokens")
	}
}

func TestCreateStoresHashAndPrefix(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, 10)
	tok, info, err := svc.Create(context.Background(), "alice", "laptop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fs.lastHash != store.HashKey(tok) {
		t.Fatal("Create stored a hash that does not match the returned token")
	}
	if fs.lastPrefix != tok[:11] || info.Prefix != tok[:11] {
		t.Fatalf("prefix mismatch: stored=%q info=%q token=%q", fs.lastPrefix, info.Prefix, tok[:11])
	}
	if fs.lastUserID != "alice" {
		t.Fatalf("owner = %q, want alice", fs.lastUserID)
	}
}

func TestCreateEnforcesCap(t *testing.T) {
	fs := &fakeStore{active: 3}
	svc := New(fs, 3)
	if _, _, err := svc.Create(context.Background(), "alice", ""); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("Create at cap: err = %v, want ErrTooManyKeys", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/keysvc/ -v`
Expected: FAIL (package/symbols undefined).

- [ ] **Step 3: Implement `keysvc`**

Create `internal/keysvc/keysvc.go`:

```go
// Package keysvc mints and manages user-owned API keys. It owns token generation
// (shared with keyctl) and enforces the per-user active-key cap.
package keysvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/opentela-ai/api/internal/store"
)

// ErrTooManyKeys is returned by Create when the user is at their active-key cap.
var ErrTooManyKeys = errors.New("keysvc: key limit reached")

// Store is the persistence surface keysvc needs.
type Store interface {
	InsertUserKey(ctx context.Context, userID, keyHash, name, prefix string) (store.KeyInfo, error)
	CountActiveByUser(ctx context.Context, userID string) (int, error)
	ListByUser(ctx context.Context, userID string) ([]store.KeyInfo, error)
	RevokeByIDForUser(ctx context.Context, userID string, id int64) (bool, error)
}

// Service creates, lists, and revokes user-owned keys.
type Service struct {
	store      Store
	maxPerUser int
}

// New wires a Store with the per-user active-key cap.
func New(s Store, maxPerUser int) *Service {
	return &Service{store: s, maxPerUser: maxPerUser}
}

// GenerateToken returns a random opaque token of the form "sk-<48 hex chars>".
func GenerateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// Prefix returns the non-secret display prefix ("sk-" + first 8 hex chars).
func Prefix(token string) string {
	if len(token) < 11 {
		return token
	}
	return token[:11]
}

// Create mints a new key for userID, returning the plaintext exactly once along
// with the stored row. It fails with ErrTooManyKeys if the user is at the cap.
func (s *Service) Create(ctx context.Context, userID, name string) (string, store.KeyInfo, error) {
	// Soft cap: a benign race could let a user exceed it by one; acceptable.
	n, err := s.store.CountActiveByUser(ctx, userID)
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	if n >= s.maxPerUser {
		return "", store.KeyInfo{}, ErrTooManyKeys
	}
	token, err := GenerateToken()
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	info, err := s.store.InsertUserKey(ctx, userID, store.HashKey(token), name, Prefix(token))
	if err != nil {
		return "", store.KeyInfo{}, err
	}
	return token, info, nil
}

// List returns userID's keys, newest first.
func (s *Service) List(ctx context.Context, userID string) ([]store.KeyInfo, error) {
	return s.store.ListByUser(ctx, userID)
}

// Revoke revokes one of userID's keys by id, reporting whether a row changed.
func (s *Service) Revoke(ctx context.Context, userID string, id int64) (bool, error) {
	return s.store.RevokeByIDForUser(ctx, userID, id)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/keysvc/ -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/keysvc
git add internal/keysvc/
git commit -m "feat(keysvc): user key service with token generation and per-user cap"
```

---

### Task 3: `neonauth` — Ed25519 JWT verifier with JWKS cache

**Files:**
- Create: `internal/neonauth/verifier.go`
- Test: `internal/neonauth/verifier_test.go`

**Interfaces:**
- Consumes: stdlib only.
- Produces:
  - `type Claims struct { Subject string }`
  - `func New(jwksURL, issuer, audience string, cacheTTL time.Duration) *Verifier`
  - `func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error)`
  - Unexported fields settable by white-box tests: `now func() time.Time`, `client *http.Client`.

- [ ] **Step 1: Write the failing test**

Create `internal/neonauth/verifier_test.go`:

```go
package neonauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testKID = "test-kid-1"

// jwksServer serves a JWKS containing pub under testKID.
func jwksServer(t *testing.T, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "kid": testKID,
		"x": base64.RawURLEncoding.EncodeToString(pub),
	}}})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// signJWT builds a signed compact JWT from header + claims maps.
func signJWT(priv ed25519.PrivateKey, header, claims map[string]any) string {
	signingInput := b64(header) + "." + b64(claims)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newVerifier(t *testing.T, srv *httptest.Server, aud string) *Verifier {
	t.Helper()
	v := New(srv.URL, "https://auth.example", aud, time.Hour)
	fixed := time.Unix(1_700_000_000, 0)
	v.now = func() time.Time { return fixed }
	return v
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": "https://auth.example",
		"sub": "user-alice",
		"exp": 1_700_000_000 + 3600,
		"iat": 1_700_000_000 - 10,
	}
}

func TestVerifyValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()
	v := newVerifier(t, srv, "")
	tok := signJWT(priv, map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}, validClaims())

	c, err := v.Verify(context.Background(), tok)
	if err != nil || c.Subject != "user-alice" {
		t.Fatalf("Verify = (%+v, %v), want subject user-alice, nil", c, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()

	hdr := map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}

	cases := map[string]string{
		"alg-none":      signJWT(priv, map[string]any{"alg": "none", "kid": testKID}, validClaims()),
		"wrong-key":     signJWT(otherPriv, hdr, validClaims()),
		"unknown-kid":   signJWT(priv, map[string]any{"alg": "EdDSA", "kid": "nope"}, validClaims()),
		"not-three":     "a.b",
		"expired":       signJWT(priv, hdr, mutate(validClaims(), "exp", 1_700_000_000-1)),
		"future-nbf":    signJWT(priv, hdr, mutate(validClaims(), "nbf", 1_700_000_000+3600)),
		"wrong-iss":     signJWT(priv, hdr, mutate(validClaims(), "iss", "https://evil")),
		"missing-sub":   signJWT(priv, hdr, mutate(validClaims(), "sub", "")),
		"missing-exp":   signJWT(priv, hdr, without(validClaims(), "exp")),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			v := newVerifier(t, srv, "")
			if _, err := v.Verify(context.Background(), tok); err == nil {
				t.Fatalf("Verify(%s) = nil error, want rejection", name)
			}
		})
	}
}

func TestVerifyAudience(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()
	hdr := map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}

	// Audience required but wrong → reject.
	v := newVerifier(t, srv, "my-api")
	if _, err := v.Verify(context.Background(), signJWT(priv, hdr, mutate(validClaims(), "aud", "other"))); err == nil {
		t.Fatal("Verify with wrong aud: want rejection")
	}
	// Audience required and correct → accept.
	if _, err := v.Verify(context.Background(), signJWT(priv, hdr, mutate(validClaims(), "aud", "my-api"))); err != nil {
		t.Fatalf("Verify with correct aud: %v", err)
	}
}

func mutate(m map[string]any, k string, val any) map[string]any {
	out := map[string]any{}
	for kk, vv := range m {
		out[kk] = vv
	}
	if s, ok := val.(string); ok && s == "" {
		delete(out, k)
	} else {
		out[k] = val
	}
	return out
}

func without(m map[string]any, k string) map[string]any { return mutate(m, k, "") }
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/neonauth/ -v`
Expected: FAIL (package/symbols undefined).

- [ ] **Step 3: Implement the verifier**

Create `internal/neonauth/verifier.go`:

```go
// Package neonauth verifies Neon Auth Ed25519 (EdDSA) JWTs against the project's
// JWKS using only the standard library. It never logs token contents.
package neonauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// leeway tolerates small clock skew on exp/nbf.
const leeway = 60 * time.Second

// Claims is the subset of verified JWT claims callers need.
type Claims struct {
	Subject string
}

// Verifier validates compact JWTs signed with EdDSA (Ed25519).
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string
	cacheTTL time.Duration

	now    func() time.Time
	client *http.Client

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey // kid -> public key
	fetchedAt time.Time
}

// New builds a Verifier. audience is enforced only when non-empty.
func New(jwksURL, issuer, audience string, cacheTTL time.Duration) *Verifier {
	return &Verifier{
		jwksURL:  jwksURL,
		issuer:   issuer,
		audience: audience,
		cacheTTL: cacheTTL,
		now:      time.Now,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// audienceClaim accepts either a string or an array of strings.
type audienceClaim []string

func (a *audienceClaim) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = []string{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err == nil {
		*a = ss
		return nil
	}
	return errors.New("aud: not a string or array")
}

type jwtClaims struct {
	Iss string        `json:"iss"`
	Sub string        `json:"sub"`
	Aud audienceClaim `json:"aud"`
	Exp int64         `json:"exp"`
	Nbf int64         `json:"nbf"`
}

// Verify checks the signature and claims of raw, returning its Claims on success.
// Any structural, signature, or claim failure returns a non-nil error.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("neonauth: token is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: header: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return Claims{}, fmt.Errorf("neonauth: header json: %w", err)
	}
	if hdr.Alg != "EdDSA" {
		return Claims{}, fmt.Errorf("neonauth: unexpected alg %q", hdr.Alg)
	}

	pub, err := v.keyFor(ctx, hdr.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: signature: %w", err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, errors.New("neonauth: signature mismatch")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: payload: %w", err)
	}
	var c jwtClaims
	if err := json.Unmarshal(payloadBytes, &c); err != nil {
		return Claims{}, fmt.Errorf("neonauth: payload json: %w", err)
	}
	if err := v.validateClaims(c); err != nil {
		return Claims{}, err
	}
	return Claims{Subject: c.Sub}, nil
}

func (v *Verifier) validateClaims(c jwtClaims) error {
	now := v.now()
	if c.Exp == 0 {
		return errors.New("neonauth: missing exp")
	}
	if now.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return errors.New("neonauth: token expired")
	}
	if c.Nbf != 0 && now.Before(time.Unix(c.Nbf, 0).Add(-leeway)) {
		return errors.New("neonauth: token not yet valid")
	}
	if c.Iss != v.issuer {
		return fmt.Errorf("neonauth: unexpected issuer %q", c.Iss)
	}
	if c.Sub == "" {
		return errors.New("neonauth: missing sub")
	}
	if v.audience != "" && !contains(c.Aud, v.audience) {
		return errors.New("neonauth: audience mismatch")
	}
	return nil
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// keyFor returns the public key for kid, refreshing the JWKS if the cache is
// stale or the kid is unknown (one refresh, to absorb key rotation).
func (v *Verifier) keyFor(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	stale := v.keys == nil || v.now().Sub(v.fetchedAt) > v.cacheTTL
	if _, ok := v.keys[kid]; !ok || stale {
		if err := v.refreshLocked(ctx); err != nil {
			// Serve a cached key if we still have one; otherwise fail.
			if k, ok := v.keys[kid]; ok {
				return k, nil
			}
			return nil, err
		}
	}
	k, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("neonauth: no key for kid %q", kid)
	}
	return k, nil
}

type jwksDoc struct {
	Keys []struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		X   string `json:"x"`
	} `json:"keys"`
}

func (v *Verifier) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("neonauth: jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("neonauth: jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("neonauth: jwks status %d", resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("neonauth: jwks decode: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		return errors.New("neonauth: jwks contained no usable Ed25519 keys")
	}
	v.keys = keys
	v.fetchedAt = v.now()
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/neonauth/ -v`
Expected: PASS (all subtests). Also run `go test -race ./internal/neonauth/` — the mutex-guarded cache must be race-clean.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/neonauth
git add internal/neonauth/
git commit -m "feat(neonauth): stdlib Ed25519 JWT verifier with JWKS cache"
```

---

### Task 4: `keysapi` middleware — JWT auth, user context, CORS

**Files:**
- Create: `internal/keysapi/middleware.go`
- Test: `internal/keysapi/middleware_test.go`

**Interfaces:**
- Consumes: `neonauth.Claims`.
- Produces:
  - `type Verifier interface { Verify(ctx context.Context, raw string) (neonauth.Claims, error) }`
  - `func Middleware(v Verifier) func(http.Handler) http.Handler` — 401 on missing/invalid JWT; on success stores the subject in context.
  - `func UserID(ctx context.Context) (string, bool)`
  - `func CORS(allowedOrigins []string) func(http.Handler) http.Handler` — echoes an allowed Origin, answers preflight `OPTIONS` with 204, and is a pass-through when the list is empty.

- [ ] **Step 1: Write the failing test**

Create `internal/keysapi/middleware_test.go`:

```go
package keysapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentela-ai/api/internal/neonauth"
)

type fakeVerifier struct {
	sub string
	err error
}

func (f fakeVerifier) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: f.sub}, f.err
}

func TestMiddlewarePassesSubject(t *testing.T) {
	var gotUser string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, _ = UserID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(fakeVerifier{sub: "alice"})(next)

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer some.jwt.token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || gotUser != "alice" {
		t.Fatalf("code=%d user=%q, want 200/alice", rec.Code, gotUser)
	}
}

func TestMiddlewareRejects(t *testing.T) {
	cases := []struct {
		name, auth string
		verr       error
	}{
		{"missing", "", nil},
		{"not-bearer", "Basic xyz", nil},
		{"empty-token", "Bearer ", nil},
		{"invalid-jwt", "Bearer bad", errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Middleware(fakeVerifier{sub: "alice", err: tc.verr})(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: code=%d, want 401", tc.name, rec.Code)
			}
		})
	}
}

func TestCORSPreflightAndEcho(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS([]string{"https://app.example"})(next)

	// Preflight is answered without hitting next and without auth.
	pre := httptest.NewRequest(http.MethodOptions, "/manage/keys", nil)
	pre.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight code=%d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("missing ACAO on preflight: %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}

	// Disallowed origin gets no ACAO header.
	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("ACAO set for disallowed origin")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/keysapi/ -v`
Expected: FAIL (package/symbols undefined).

- [ ] **Step 3: Implement the middleware**

Create `internal/keysapi/middleware.go`:

```go
// Package keysapi is the HTTP surface for user-owned key management: a JWT-gated
// set of JSON endpoints under /manage/keys, plus their CORS handling.
package keysapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/opentela-ai/api/internal/neonauth"
)

// Verifier verifies a raw bearer JWT and returns its claims.
type Verifier interface {
	Verify(ctx context.Context, raw string) (neonauth.Claims, error)
}

type ctxKey int

const userIDKey ctxKey = 0

const bearerPrefix = "bearer "

// UserID returns the authenticated subject placed in ctx by Middleware.
func UserID(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(userIDKey).(string)
	return s, ok && s != ""
}

// Middleware requires a valid Neon Auth JWT and stores its subject in the request
// context. Missing/malformed/invalid tokens get 401. (This plane is deliberately
// independent of the proxy plane's Bearer parsing.)
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := v.Verify(r.Context(), token)
			if err != nil || claims.Subject == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, claims.Subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	return token, token != ""
}

// CORS applies a minimal allowlist policy to the management plane. With an empty
// list it is a pass-through (no CORS headers). Preflight OPTIONS is answered 204
// before any downstream auth runs.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/keysapi/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/keysapi
git add internal/keysapi/
git commit -m "feat(keysapi): JWT auth middleware, user context, and CORS"
```

---

### Task 5: `keysapi` handlers + Router

**Files:**
- Create: `internal/keysapi/handlers.go`
- Test: `internal/keysapi/handlers_test.go`

**Interfaces:**
- Consumes: `keysvc.ErrTooManyKeys`, `store.KeyInfo`, the `Verifier`/`Middleware`/`CORS`/`UserID` from Task 4.
- Produces:
  - `type Service interface { Create(ctx, userID, name string) (string, store.KeyInfo, error); List(ctx, userID string) ([]store.KeyInfo, error); Revoke(ctx, userID string, id int64) (bool, error) }`
  - `func Router(svc Service, v Verifier, corsOrigins []string) http.Handler`

- [ ] **Step 1: Write the failing test**

Create `internal/keysapi/handlers_test.go`:

```go
package keysapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/store"
)

type fakeService struct {
	created   string
	listErr   error
	revokeOK  bool
	createErr error
}

func (f *fakeService) Create(_ context.Context, userID, name string) (string, store.KeyInfo, error) {
	if f.createErr != nil {
		return "", store.KeyInfo{}, f.createErr
	}
	uid := userID
	return "sk-secretsecret", store.KeyInfo{ID: 7, UserID: &uid, Name: name, Prefix: "sk-secrets", Active: true}, nil
}
func (f *fakeService) List(context.Context, string) ([]store.KeyInfo, error) {
	return []store.KeyInfo{{ID: 7, Name: "laptop", Prefix: "sk-secrets", Active: true}}, f.listErr
}
func (f *fakeService) Revoke(context.Context, string, int64) (bool, error) { return f.revokeOK, nil }

func router(svc Service) http.Handler {
	return Router(svc, fakeVerifier{sub: "alice"}, nil)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateReturnsKeyOnce(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodPost, "/manage/keys", `{"name":"laptop"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201", rec.Code)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["key"] != "sk-secretsecret" || got["prefix"] != "sk-secrets" || got["id"].(float64) != 7 {
		t.Fatalf("create body = %v", got)
	}
}

func TestListOmitsSecrets(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodGet, "/manage/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "key_hash") || strings.Contains(strings.ToLower(body), `"key"`) {
		t.Fatalf("list leaked secret material: %s", body)
	}
	if !strings.Contains(body, "sk-secrets") {
		t.Fatalf("list missing prefix: %s", body)
	}
}

func TestCreateOverCapIs409(t *testing.T) {
	rec := do(t, router(&fakeService{createErr: keysvc.ErrTooManyKeys}), http.MethodPost, "/manage/keys", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
}

func TestCreateBadJSONIs400(t *testing.T) {
	rec := do(t, router(&fakeService{}), http.MethodPost, "/manage/keys", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestRevokeOwnerScoped(t *testing.T) {
	// Unknown/other-owner id → 404.
	rec := do(t, router(&fakeService{revokeOK: false}), http.MethodDelete, "/manage/keys/99", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
	// Owned id → 204.
	rec = do(t, router(&fakeService{revokeOK: true}), http.MethodDelete, "/manage/keys/7", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d, want 204", rec.Code)
	}
	// Non-numeric id → 400.
	rec = do(t, router(&fakeService{}), http.MethodDelete, "/manage/keys/abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/keysapi/ -run 'Create|List|Revoke' -v`
Expected: FAIL (`Router`, `Service` undefined).

- [ ] **Step 3: Implement handlers + Router**

Create `internal/keysapi/handlers.go`:

```go
package keysapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/store"
)

const maxNameLen = 100
const maxBodyBytes = 4 << 10

// Service is the key-management behavior the handlers depend on. Its methods
// match *keysvc.Service exactly, so the concrete service satisfies it directly.
type Service interface {
	Create(ctx context.Context, userID, name string) (string, store.KeyInfo, error)
	List(ctx context.Context, userID string) ([]store.KeyInfo, error)
	Revoke(ctx context.Context, userID string, id int64) (bool, error)
}

type createRequest struct {
	Name string `json:"name"`
}

type createResponse struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Prefix    string    `json:"prefix"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type keyResponse struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// Router builds the /manage/keys handler tree, wrapped with CORS (outermost) and
// JWT auth. Preflight is handled by CORS before auth.
func Router(svc Service, v Verifier, corsOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		var req createRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if len(req.Name) > maxNameLen {
			http.Error(w, "name too long", http.StatusBadRequest)
			return
		}
		token, info, err := svc.Create(r.Context(), userID, req.Name)
		if errors.Is(err, keysvc.ErrTooManyKeys) {
			http.Error(w, "key limit reached", http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusCreated, createResponse{
			ID: info.ID, Key: token, Prefix: info.Prefix, Name: info.Name, CreatedAt: info.CreatedAt,
		})
	})
	mux.HandleFunc("GET /manage/keys", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		keys, err := svc.List(r.Context(), userID)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		out := make([]keyResponse, 0, len(keys))
		for _, k := range keys {
			out = append(out, keyResponse{
				ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /manage/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		userID, _ := UserID(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid key id", http.StatusBadRequest)
			return
		}
		changed, err := svc.Revoke(r.Context(), userID, id)
		if err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if !changed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return CORS(corsOrigins)(Middleware(v)(mux))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/keysapi/ -v`
Expected: PASS (all handler + middleware tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/keysapi
git add internal/keysapi/
git commit -m "feat(keysapi): create/list/revoke handlers and Router"
```

---

### Task 6: Config for the key-management plane

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces new `Config` fields: `NeonAuthJWKSURL string`, `NeonAuthIssuer string`, `NeonAuthAudience string`, `JWKSCacheTTL time.Duration`, `MaxKeysPerUser int`, `CORSAllowedOrigins []string`, `KeyMgmtEnabled bool`. New helper `intEnv`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoadKeyMgmtDisabledByDefault(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KeyMgmtEnabled {
		t.Fatal("KeyMgmtEnabled should be false when Neon Auth is unset")
	}
	if cfg.MaxKeysPerUser != 10 || cfg.JWKSCacheTTL != time.Hour {
		t.Fatalf("defaults wrong: max=%d ttl=%s", cfg.MaxKeysPerUser, cfg.JWKSCacheTTL)
	}
}

func TestLoadKeyMgmtEnabled(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NEON_AUTH_JWKS_URL", "https://auth/jwks")
	t.Setenv("NEON_AUTH_ISSUER", "https://auth")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example, https://b.example")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.KeyMgmtEnabled {
		t.Fatal("KeyMgmtEnabled should be true")
	}
	if len(cfg.CORSAllowedOrigins) != 2 || cfg.CORSAllowedOrigins[1] != "https://b.example" {
		t.Fatalf("CORS origins = %v", cfg.CORSAllowedOrigins)
	}
}

func TestLoadKeyMgmtPartialIsError(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NEON_AUTH_JWKS_URL", "https://auth/jwks") // issuer missing
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when only one Neon Auth var is set")
	}
}

func TestLoadRejectsNonPositiveMaxKeys(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MAX_KEYS_PER_USER", "0")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for MAX_KEYS_PER_USER=0")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run KeyMgmt -v`
Expected: FAIL (fields undefined / compile error).

- [ ] **Step 3: Implement config additions**

In `internal/config/config.go`, add fields to `Config`:

```go
	// Key-management plane (optional). Enabled only when both NeonAuthJWKSURL and
	// NeonAuthIssuer are set.
	NeonAuthJWKSURL    string
	NeonAuthIssuer     string
	NeonAuthAudience   string
	JWKSCacheTTL       time.Duration
	MaxKeysPerUser     int
	CORSAllowedOrigins []string
	KeyMgmtEnabled     bool
```

In `Load`, before the final `return`, add:

```go
	jwksTTL, err := durationEnv("NEON_AUTH_JWKS_CACHE_TTL", time.Hour)
	if err != nil {
		return nil, err
	}
	maxKeys, err := intEnv("MAX_KEYS_PER_USER", 10)
	if err != nil {
		return nil, err
	}
	jwksURL := os.Getenv("NEON_AUTH_JWKS_URL")
	issuer := os.Getenv("NEON_AUTH_ISSUER")
	if (jwksURL == "") != (issuer == "") {
		return nil, fmt.Errorf("NEON_AUTH_JWKS_URL and NEON_AUTH_ISSUER must be set together")
	}
	var corsOrigins []string
	if raw := os.Getenv("CORS_ALLOWED_ORIGINS"); raw != "" {
		for _, o := range strings.Split(raw, ",") {
			if o = strings.TrimSpace(o); o != "" {
				corsOrigins = append(corsOrigins, o)
			}
		}
	}
```

Then extend the returned struct literal with:

```go
		NeonAuthJWKSURL:    jwksURL,
		NeonAuthIssuer:     issuer,
		NeonAuthAudience:   os.Getenv("NEON_AUTH_AUDIENCE"),
		JWKSCacheTTL:       jwksTTL,
		MaxKeysPerUser:     maxKeys,
		CORSAllowedOrigins: corsOrigins,
		KeyMgmtEnabled:     jwksURL != "" && issuer != "",
```

Add `"strings"` to the imports, and add this helper next to `durationEnv`:

```go
func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, v)
	}
	return n, nil
}
```

Add `"strconv"` to the imports.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (new + existing config tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config
git add internal/config/
git commit -m "feat(config): Neon Auth, CORS, and key-cap configuration"
```

---

### Task 7: Wire the plane into the server, keyctl, and docs

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/server/main.go`
- Modify: `cmd/keyctl/main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `keysapi.Router`, `neonauth.New`, `keysvc.New`, `keysvc.GenerateToken`, config fields from Task 6.
- Produces: `func server.New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler) http.Handler` (keyMgmt may be nil).

- [ ] **Step 1: Write the failing server test**

In `internal/server/server_test.go`, update the three existing `New(...)` calls to pass a third arg `nil` (e.g. `New(stubValidator{valid: false}, proxyStub(), nil)`), and add:

```go
func TestKeyMgmtRoutedWhenPresent(t *testing.T) {
	keyMgmt := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel proving we reached the mgmt handler
	})
	h := New(stubValidator{valid: false}, proxyStub(), keyMgmt)

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("code=%d, want 418 (mgmt handler reached, not proxy auth)", rec.Code)
	}
}

func TestManageNotRoutedWhenNil(t *testing.T) {
	// With no mgmt handler, /manage/* falls through to the auth-gated proxy → 401.
	h := New(stubValidator{valid: false}, proxyStub(), nil)
	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", rec.Code)
	}
}
```

(Ensure `net/http` and `net/http/httptest` are imported in the test file — they already are.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/ -v`
Expected: FAIL (compile: `New` takes 2 args / new test references).

- [ ] **Step 3: Update `server.New`**

Replace `internal/server/server.go`'s `New` with:

```go
// New builds the top-level handler. /healthz is unauthenticated. When keyMgmt is
// non-nil, /manage/ is served by it (its own JWT auth); every other path passes
// through the Bearer API-key middleware before reaching proxy.
func New(v auth.TokenValidator, proxy http.Handler, keyMgmt http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if keyMgmt != nil {
		mux.Handle("/manage/", keyMgmt)
	}
	mux.Handle("/", auth.Middleware(v)(proxy))
	return mux
}
```

- [ ] **Step 4: Run the server test to verify it passes**

Run: `go test ./internal/server/ -v`
Expected: PASS.

- [ ] **Step 5: Wire `cmd/server/main.go`**

Add imports `"net/http"` (already present), `"github.com/opentela-ai/api/internal/keysapi"`, `"github.com/opentela-ai/api/internal/keysvc"`, `"github.com/opentela-ai/api/internal/neonauth"`. Replace the handler-construction line

```go
	handler := server.New(validator, proxy.New(cfg.UpstreamURL))
```

with:

```go
	var keyMgmt http.Handler
	if cfg.KeyMgmtEnabled {
		verifier := neonauth.New(cfg.NeonAuthJWKSURL, cfg.NeonAuthIssuer, cfg.NeonAuthAudience, cfg.JWKSCacheTTL)
		svc := keysvc.New(pg, cfg.MaxKeysPerUser)
		keyMgmt = keysapi.Router(svc, verifier, cfg.CORSAllowedOrigins)
		log.Printf("key management enabled at /manage/keys (issuer %s)", cfg.NeonAuthIssuer)
	}
	handler := server.New(validator, proxy.New(cfg.UpstreamURL), keyMgmt)
```

(`pg` already satisfies `keysvc.Store` via the Task 1 methods.)

- [ ] **Step 6: Refactor `cmd/keyctl/main.go`**

- Remove the local `generateToken` function and its now-unused imports (`crypto/rand`, `encoding/hex`).
- Add import `"github.com/opentela-ai/api/internal/keysvc"`.
- In `cmdAdd`, replace `token, err := generateToken()` with `token, err := keysvc.GenerateToken()`.
- Replace `cmdMigrate` so it applies every `migrations/*.sql` in sorted order (so `0002` is included). Add imports `"path/filepath"` and `"sort"`:

```go
func cmdMigrate(ctx context.Context, pg *store.Postgres) error {
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		return fmt.Errorf("find migrations (run from repo root): %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found (run from repo root)")
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if err := pg.Migrate(ctx, string(ddl)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
		fmt.Printf("applied %s\n", f)
	}
	return nil
}
```

- [ ] **Step 7: Build, vet, and run the full suite**

Run:
```bash
gofmt -l .
go vet ./...
go build ./...
go test ./...
```
Expected: `gofmt` prints nothing; vet/build clean; all tests PASS (store integration SKIPs without `TEST_DATABASE_URL`).

- [ ] **Step 8: Update `README.md`**

Add a "Key management API (optional)" section documenting: the enable rule (`NEON_AUTH_JWKS_URL` + `NEON_AUTH_ISSUER`), the three endpoints with example requests/responses (key shown once), and a config table row set for `NEON_AUTH_JWKS_URL`, `NEON_AUTH_ISSUER`, `NEON_AUTH_AUDIENCE`, `NEON_AUTH_JWKS_CACHE_TTL` (default `1h`), `MAX_KEYS_PER_USER` (default `10`), `CORS_ALLOWED_ORIGINS`. Note that `keyctl` keys remain admin keys with no owner.

- [ ] **Step 9: Commit**

```bash
gofmt -w .
git add internal/server/ cmd/server/ cmd/keyctl/ README.md
git commit -m "feat: mount optional /manage/keys plane; keyctl uses keysvc + all migrations"
```

---

## Post-implementation verification (with a real database)

After all tasks, run the store integration tests and a live smoke against a **non-production** Neon branch (create one with `neon checkout dev-keys` so the schema drop/recreate never touches `production`):

```bash
neon checkout dev-keys           # isolated branch; pulls its DATABASE_URL into .env.local
set -a; . ./.env.local; set +a
TEST_DATABASE_URL="$DATABASE_URL" go test ./... -v   # store integration now runs
go run ./cmd/keyctl migrate                           # applies 0001 + 0002 on the branch
```

Then a whole-branch review (see subagent-driven-development's final review) before merging `feat/user-scoped-keys`.

---

## Self-Review (completed by plan author)

- **Spec coverage:** two auth planes (Tasks 4/5/7) ✓; data model + migration 0002 (Task 1) ✓; `neonauth` verifier with alg/exp/iss/aud enforcement (Task 3) ✓; `keysvc` token gen + cap (Task 2) ✓; `keysapi` endpoints + status codes 201/200/204/400/401/404/409/503 (Task 5) ✓; CORS (Tasks 4/5) ✓; config incl. enable toggle + partial-config error (Task 6) ✓; owner-scoped revoke (Tasks 1/5) ✓; key shown once / list omits secrets (Task 5 tests) ✓; keyctl still works after refactor (Task 7) ✓; disabled-by-default pure proxy (Tasks 6/7) ✓; no new deps (all tasks, stdlib+pgx) ✓; testing per spec (every task) ✓.
- **Placeholder scan:** none — every step has concrete code/commands. The one deliberate "wrong then corrected" `Service` interface in Task 5 is flagged explicitly with the correct version to write.
- **Type consistency:** `store.KeyInfo` fields (ID/Prefix/UserID) are defined in Task 1 and consumed unchanged in Tasks 2/5; `keysvc.Service` methods match the `keysapi.Service` interface (both `context.Context`); `neonauth.Verifier.Verify(ctx, raw) (Claims, error)` matches `keysapi.Verifier`; `server.New` third param `keyMgmt http.Handler` matches the updated callers in `cmd/server` and `server_test`.
