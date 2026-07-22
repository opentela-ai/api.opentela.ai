# OpenTela API Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go HTTP service that authenticates requests by an API key stored in Postgres (cached in memory for 14 days) and transparently reverse-proxies valid requests to a configurable opentela upstream.

**Architecture:** An auth middleware extracts a Bearer token and validates it through a Validator that fronts a Postgres-backed key store with an in-memory TTL cache. Valid requests flow to a streaming `httputil.ReverseProxy` aimed at the upstream. A `keyctl` CLI seeds and manages keys.

**Tech Stack:** Go 1.26, `net/http` + `net/http/httputil` (stdlib reverse proxy), `github.com/jackc/pgx/v5` (`pgxpool`) for Postgres, `crypto/sha256` for key hashing. Tests use `testing` + `net/http/httptest`.

## Global Constraints

- Module path: `github.com/opentela-ai/api`
- Go version floor: `go 1.26`
- Only new dependency: `github.com/jackc/pgx/v5`. Everything else is stdlib.
- API keys are stored and cached as **SHA-256 hex hashes**, never plaintext.
- Client key header: `Authorization: Bearer <key>`, forwarded to upstream unchanged.
- Config via env: `OPENTELA_UPSTREAM_URL` (required), `DATABASE_URL` (required), `LISTEN_ADDR` (default `:8080`), `CACHE_TTL` (default `336h`), `CACHE_NEGATIVE_TTL` (default `30s`), `CACHE_JANITOR_INTERVAL` (default `10m`).
- Missing/malformed auth or invalid key → `401`. Upstream unreachable → `502`. Store error during validation → `503`.
- TDD: write the failing test first, watch it fail, implement minimally, watch it pass, commit.

## File Structure

```
go.mod / go.sum
migrations/0001_init.sql             — schema DDL
internal/config/config.go            — env loading + validation
internal/cache/cache.go              — TTL cache with janitor
internal/store/store.go              — HashKey + KeyStore interface + KeyInfo
internal/store/postgres.go           — pgxpool implementation + admin ops
internal/auth/validator.go           — cache↔store validation
internal/auth/middleware.go          — Bearer auth middleware
internal/proxy/proxy.go              — streaming reverse proxy
internal/server/server.go            — mux: /healthz + auth→proxy
cmd/server/main.go                   — service entrypoint
cmd/keyctl/main.go                   — CLI: migrate|add|revoke|list
README.md                            — run/operate instructions
```

---

### Task 1: Module init + config package

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Config` struct and `config.Load() (*Config, error)`.
  - `type Config struct { UpstreamURL *url.URL; DatabaseURL, ListenAddr string; CacheTTL, CacheNegTTL, JanitorEvery time.Duration }`

- [ ] **Step 1: Initialize the module**

Run:
```bash
go mod init github.com/opentela-ai/api
```
Expected: creates `go.mod` containing `module github.com/opentela-ai/api` and `go 1.26`.

- [ ] **Step 2: Write the failing test**

Create `internal/config/config_test.go`:
```go
package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	// Ensure optional vars are unset so defaults apply.
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("CACHE_TTL", "")
	t.Setenv("CACHE_NEGATIVE_TTL", "")
	t.Setenv("CACHE_JANITOR_INTERVAL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.UpstreamURL.Host != "api.opentela.ai" {
		t.Errorf("UpstreamURL host = %q, want api.opentela.ai", cfg.UpstreamURL.Host)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.CacheTTL != 336*time.Hour {
		t.Errorf("CacheTTL = %v, want 336h", cfg.CacheTTL)
	}
	if cfg.CacheNegTTL != 30*time.Second {
		t.Errorf("CacheNegTTL = %v, want 30s", cfg.CacheNegTTL)
	}
	if cfg.JanitorEvery != 10*time.Minute {
		t.Errorf("JanitorEvery = %v, want 10m", cfg.JanitorEvery)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "http://up:9000/base")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("CACHE_TTL", "1h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", cfg.ListenAddr)
	}
	if cfg.CacheTTL != time.Hour {
		t.Errorf("CacheTTL = %v, want 1h", cfg.CacheTTL)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when required vars missing, got nil")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Load` / package has no non-test files.

- [ ] **Step 4: Write minimal implementation**

Create `internal/config/config.go`:
```go
// Package config loads and validates service configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"
)

// Config holds all runtime configuration for the proxy service.
type Config struct {
	UpstreamURL  *url.URL
	DatabaseURL  string
	ListenAddr   string
	CacheTTL     time.Duration
	CacheNegTTL  time.Duration
	JanitorEvery time.Duration
}

// Load reads configuration from environment variables, applies defaults, and
// validates required values. It returns an error if a required variable is
// missing or a value is malformed.
func Load() (*Config, error) {
	rawUpstream := os.Getenv("OPENTELA_UPSTREAM_URL")
	if rawUpstream == "" {
		return nil, fmt.Errorf("OPENTELA_UPSTREAM_URL is required")
	}
	upstream, err := url.Parse(rawUpstream)
	if err != nil {
		return nil, fmt.Errorf("OPENTELA_UPSTREAM_URL is invalid: %w", err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("OPENTELA_UPSTREAM_URL must be an absolute URL, got %q", rawUpstream)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	cacheTTL, err := durationEnv("CACHE_TTL", 336*time.Hour)
	if err != nil {
		return nil, err
	}
	negTTL, err := durationEnv("CACHE_NEGATIVE_TTL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	janitor, err := durationEnv("CACHE_JANITOR_INTERVAL", 10*time.Minute)
	if err != nil {
		return nil, err
	}

	return &Config{
		UpstreamURL:  upstream,
		DatabaseURL:  dbURL,
		ListenAddr:   stringEnv("LISTEN_ADDR", ":8080"),
		CacheTTL:     cacheTTL,
		CacheNegTTL:  negTTL,
		JanitorEvery: janitor,
	}, nil
}

func stringEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	return d, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS (ok).

- [ ] **Step 6: Commit**

```bash
git add go.mod internal/config/
git commit -m "feat: config package with env loading and validation"
```

---

### Task 2: TTL cache

**Files:**
- Create: `internal/cache/cache.go`
- Test: `internal/cache/cache_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func New(janitorEvery time.Duration) *Cache`
  - `func (c *Cache) Get(key string) (val bool, ok bool)`
  - `func (c *Cache) Set(key string, val bool, ttl time.Duration)`
  - `func (c *Cache) Len() int`
  - `func (c *Cache) Close()`
  - Unexported clock field `now func() time.Time` and method `deleteExpired()` (white-box tested).

- [ ] **Step 1: Write the failing test**

Create `internal/cache/cache_test.go`:
```go
package cache

import (
	"sync"
	"testing"
	"time"
)

func TestSetGetHit(t *testing.T) {
	c := New(0)
	defer c.Close()
	c.Set("k", true, time.Minute)
	got, ok := c.Get("k")
	if !ok || got != true {
		t.Fatalf("Get = (%v,%v), want (true,true)", got, ok)
	}
}

func TestGetMiss(t *testing.T) {
	c := New(0)
	defer c.Close()
	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get(absent) ok = true, want false")
	}
}

func TestNegativeValueCached(t *testing.T) {
	c := New(0)
	defer c.Close()
	c.Set("bad", false, time.Minute)
	got, ok := c.Get("bad")
	if !ok || got != false {
		t.Fatalf("Get = (%v,%v), want (false,true)", got, ok)
	}
}

func TestExpiryIsMiss(t *testing.T) {
	c := New(0)
	defer c.Close()
	base := time.Unix(1000, 0)
	c.now = func() time.Time { return base }
	c.Set("k", true, time.Minute)
	// Advance the clock past the TTL.
	c.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, ok := c.Get("k"); ok {
		t.Fatal("expired entry returned ok = true, want false")
	}
}

func TestDeleteExpiredEvicts(t *testing.T) {
	c := New(0)
	defer c.Close()
	base := time.Unix(1000, 0)
	c.now = func() time.Time { return base }
	c.Set("k", true, time.Minute)
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	c.now = func() time.Time { return base.Add(2 * time.Minute) }
	c.deleteExpired()
	if c.Len() != 0 {
		t.Fatalf("Len after deleteExpired = %d, want 0", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(0)
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%26))
			c.Set(key, true, time.Minute)
			c.Get(key)
		}(i)
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cache/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cache/cache.go`:
```go
// Package cache provides a concurrency-safe in-memory TTL cache of boolean
// key-validation results, with a background janitor that evicts expired entries.
package cache

import (
	"sync"
	"time"
)

type entry struct {
	val       bool
	expiresAt time.Time
}

// Cache is a concurrency-safe map of string keys to boolean values with per-entry
// expiry. Keys are expected to be SHA-256 hex digests, not plaintext tokens.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time

	stop chan struct{}
	done chan struct{}
}

// New creates a Cache. If janitorEvery > 0, a background goroutine evicts expired
// entries on that interval. Call Close to stop it.
func New(janitorEvery time.Duration) *Cache {
	c := &Cache{
		data: make(map[string]entry),
		now:  time.Now,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if janitorEvery > 0 {
		go c.janitor(janitorEvery)
	} else {
		close(c.done)
	}
	return c
}

// Get returns the cached value and whether a live (non-expired) entry exists.
func (c *Cache) Get(key string) (bool, bool) {
	c.mu.RLock()
	e, ok := c.data[key]
	c.mu.RUnlock()
	if !ok || c.now().After(e.expiresAt) {
		return false, false
	}
	return e.val, true
}

// Set stores val under key with the given time-to-live.
func (c *Cache) Set(key string, val bool, ttl time.Duration) {
	c.mu.Lock()
	c.data[key] = entry{val: val, expiresAt: c.now().Add(ttl)}
	c.mu.Unlock()
}

// Len returns the number of entries currently held (including any not yet swept).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// Close stops the janitor goroutine. Safe to call once.
func (c *Cache) Close() {
	close(c.stop)
	<-c.done
}

func (c *Cache) janitor(every time.Duration) {
	defer close(c.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.deleteExpired()
		}
	}
}

func (c *Cache) deleteExpired() {
	now := c.now()
	c.mu.Lock()
	for k, e := range c.data {
		if now.After(e.expiresAt) {
			delete(c.data, k)
		}
	}
	c.mu.Unlock()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race ./internal/cache/`
Expected: PASS (ok), no race warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/cache/
git commit -m "feat: in-memory TTL cache with janitor"
```

---

### Task 3: Store contracts (HashKey + interface) + migration

**Files:**
- Create: `internal/store/store.go`
- Create: `migrations/0001_init.sql`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func HashKey(token string) string` — SHA-256 hex of the token.
  - `type KeyStore interface { Validate(ctx context.Context, keyHash string) (bool, error) }`
  - `type KeyInfo struct { KeyHash, Name string; Active bool; CreatedAt time.Time; RevokedAt *time.Time }`

- [ ] **Step 1: Write the failing test**

Create `internal/store/store_test.go`:
```go
package store

import "testing"

func TestHashKeyKnownVector(t *testing.T) {
	// SHA-256("test") is a well-known digest.
	const want = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if got := HashKey("test"); got != want {
		t.Fatalf("HashKey(\"test\") = %q, want %q", got, want)
	}
}

func TestHashKeyDeterministicAndDistinct(t *testing.T) {
	if HashKey("a") != HashKey("a") {
		t.Error("HashKey not deterministic")
	}
	if HashKey("a") == HashKey("b") {
		t.Error("HashKey collided on distinct inputs")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL — `undefined: HashKey`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/store/store.go`:
```go
// Package store defines the API-key persistence contract and its Postgres
// implementation. Keys are addressed by their SHA-256 hex digest, never plaintext.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// HashKey returns the lowercase SHA-256 hex digest of a plaintext API token.
func HashKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// KeyStore is the minimal contract the request path depends on: given a key hash,
// report whether a matching active key exists.
type KeyStore interface {
	Validate(ctx context.Context, keyHash string) (bool, error)
}

// KeyInfo is a row of the api_keys table, used by administrative listing.
type KeyInfo struct {
	KeyHash   string
	Name      string
	Active    bool
	CreatedAt time.Time
	RevokedAt *time.Time
}
```

Create `migrations/0001_init.sql`:
```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id         BIGSERIAL PRIMARY KEY,
    key_hash   TEXT NOT NULL UNIQUE,
    name       TEXT,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS (ok).

- [ ] **Step 5: Commit**

```bash
git add internal/store/ migrations/
git commit -m "feat: store contract (HashKey, KeyStore) and schema migration"
```

---

### Task 4: Postgres store implementation

**Files:**
- Create: `internal/store/postgres.go`
- Test: `internal/store/postgres_test.go`

**Interfaces:**
- Consumes: `KeyInfo`, `KeyStore` (Task 3).
- Produces:
  - `func NewPostgres(ctx context.Context, dsn string) (*Postgres, error)`
  - `func (p *Postgres) Validate(ctx context.Context, keyHash string) (bool, error)` — satisfies `KeyStore`.
  - `func (p *Postgres) Migrate(ctx context.Context, ddl string) error`
  - `func (p *Postgres) Insert(ctx context.Context, keyHash, name string) error`
  - `func (p *Postgres) Revoke(ctx context.Context, keyHash string) (bool, error)`
  - `func (p *Postgres) List(ctx context.Context) ([]KeyInfo, error)`
  - `func (p *Postgres) Close()`

- [ ] **Step 1: Add the pgx dependency**

Run:
```bash
go get github.com/jackc/pgx/v5@latest
```
Expected: `go.mod`/`go.sum` updated with `github.com/jackc/pgx/v5`.

- [ ] **Step 2: Write the failing integration test**

Create `internal/store/postgres_test.go`:
```go
package store

import (
	"context"
	"os"
	"testing"
)

// newTestStore connects to TEST_DATABASE_URL and applies the schema. It skips the
// test when the variable is unset so unit runs stay hermetic.
func newTestStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	p, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx, "DROP TABLE IF EXISTS api_keys;"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	ddl, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := p.Migrate(ctx, string(ddl)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return p
}

func TestPostgresValidateLifecycle(t *testing.T) {
	ctx := context.Background()
	p := newTestStore(t)
	hash := HashKey("token-1")

	// Unknown key → not valid, no error.
	if ok, err := p.Validate(ctx, hash); err != nil || ok {
		t.Fatalf("Validate(unknown) = (%v,%v), want (false,nil)", ok, err)
	}

	// Insert → valid.
	if err := p.Insert(ctx, hash, "alice"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if ok, err := p.Validate(ctx, hash); err != nil || !ok {
		t.Fatalf("Validate(active) = (%v,%v), want (true,nil)", ok, err)
	}

	// Revoke → not valid.
	changed, err := p.Revoke(ctx, hash)
	if err != nil || !changed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", changed, err)
	}
	if ok, err := p.Validate(ctx, hash); err != nil || ok {
		t.Fatalf("Validate(revoked) = (%v,%v), want (false,nil)", ok, err)
	}

	// List returns the row.
	rows, err := p.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "alice" || rows[0].Active {
		t.Fatalf("List = %+v, want one inactive row named alice", rows)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL to compile — `undefined: NewPostgres` (the integration test skips at runtime, but the package must compile, so this is a build failure until Step 4).

- [ ] **Step 4: Write minimal implementation**

Create `internal/store/postgres.go`:
```go
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a pgxpool-backed KeyStore plus administrative operations used by keyctl.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a connection pool to dsn and verifies connectivity.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// Migrate executes an arbitrary DDL string (used to apply migration files).
func (p *Postgres) Migrate(ctx context.Context, ddl string) error {
	_, err := p.pool.Exec(ctx, ddl)
	return err
}

// Validate reports whether an active key exists for the given hash.
func (p *Postgres) Validate(ctx context.Context, keyHash string) (bool, error) {
	var active bool
	err := p.pool.QueryRow(ctx,
		`SELECT active FROM api_keys WHERE key_hash = $1`, keyHash).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active, nil
}

// Insert adds a new active key. name may be empty.
func (p *Postgres) Insert(ctx context.Context, keyHash, name string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO api_keys (key_hash, name) VALUES ($1, $2)`, keyHash, name)
	return err
}

// Revoke marks a key inactive. It reports whether a row was changed.
func (p *Postgres) Revoke(ctx context.Context, keyHash string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = now()
		 WHERE key_hash = $1 AND active = TRUE`, keyHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// List returns all keys ordered by creation time.
func (p *Postgres) List(ctx context.Context) ([]KeyInfo, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key_hash, COALESCE(name, ''), active, created_at, revoked_at
		 FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.KeyHash, &k.Name, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run test to verify it passes (or skips)**

Run without a DB: `go test ./internal/store/`
Expected: PASS with the integration test reported as SKIP.

Optional, with a DB: `TEST_DATABASE_URL=postgres://... go test ./internal/store/ -run TestPostgresValidateLifecycle -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/store/postgres.go internal/store/postgres_test.go
git commit -m "feat: postgres key store with validate and admin ops"
```

---

### Task 5: Validator (cache ↔ store)

**Files:**
- Create: `internal/auth/validator.go`
- Test: `internal/auth/validator_test.go`

**Interfaces:**
- Consumes: `store.KeyStore`, `store.HashKey` (Task 3), `*cache.Cache` (Task 2).
- Produces:
  - `func NewValidator(s store.KeyStore, c *cache.Cache, posTTL, negTTL time.Duration) *Validator`
  - `func (v *Validator) Valid(ctx context.Context, token string) (bool, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/auth/validator_test.go`:
```go
package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// fakeStore records how many times Validate is called and returns a scripted result.
type fakeStore struct {
	valid bool
	err   error
	calls int
}

func (f *fakeStore) Validate(_ context.Context, _ string) (bool, error) {
	f.calls++
	return f.valid, f.err
}

func TestValidatorValidCaches(t *testing.T) {
	fs := &fakeStore{valid: true}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Second)

	for i := 0; i < 3; i++ {
		ok, err := v.Valid(context.Background(), "tok")
		if err != nil || !ok {
			t.Fatalf("Valid = (%v,%v), want (true,nil)", ok, err)
		}
	}
	if fs.calls != 1 {
		t.Fatalf("store called %d times, want 1 (cache should serve the rest)", fs.calls)
	}
}

func TestValidatorInvalidCachedNegatively(t *testing.T) {
	fs := &fakeStore{valid: false}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	ok, err := v.Valid(context.Background(), "bad")
	if err != nil || ok {
		t.Fatalf("Valid = (%v,%v), want (false,nil)", ok, err)
	}
	// Cached negatively → no second store call.
	if _, _ = v.Valid(context.Background(), "bad"); fs.calls != 1 {
		t.Fatalf("store called %d times, want 1", fs.calls)
	}
	// The cached key is the hash, and the value is false.
	if val, hit := c.Get(store.HashKey("bad")); !hit || val {
		t.Fatalf("cache Get = (%v,%v), want (false,true)", val, hit)
	}
}

func TestValidatorStoreErrorNotCached(t *testing.T) {
	fs := &fakeStore{err: errors.New("db down")}
	c := cache.New(0)
	defer c.Close()
	v := NewValidator(fs, c, time.Hour, time.Minute)

	if _, err := v.Valid(context.Background(), "tok"); err == nil {
		t.Fatal("Valid expected error, got nil")
	}
	if _, hit := c.Get(store.HashKey("tok")); hit {
		t.Fatal("store error must not be cached")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/`
Expected: FAIL — `undefined: NewValidator`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/auth/validator.go`:
```go
// Package auth validates API keys against a cache-fronted key store and provides
// the Bearer-token HTTP middleware that gates the proxy.
package auth

import (
	"context"
	"time"

	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/store"
)

// Validator answers whether a plaintext token is valid, consulting an in-memory
// cache before the backing store. Positive results live for posTTL, negatives for
// negTTL. Store errors are surfaced and never cached.
type Validator struct {
	store  store.KeyStore
	cache  *cache.Cache
	posTTL time.Duration
	negTTL time.Duration
}

// NewValidator wires a store and cache together with the two TTLs.
func NewValidator(s store.KeyStore, c *cache.Cache, posTTL, negTTL time.Duration) *Validator {
	return &Validator{store: s, cache: c, posTTL: posTTL, negTTL: negTTL}
}

// Valid hashes the token, checks the cache, and on a miss consults the store and
// caches the result with the TTL appropriate to the outcome.
func (v *Validator) Valid(ctx context.Context, token string) (bool, error) {
	h := store.HashKey(token)
	if val, ok := v.cache.Get(h); ok {
		return val, nil
	}
	valid, err := v.store.Validate(ctx, h)
	if err != nil {
		return false, err
	}
	if valid {
		v.cache.Set(h, true, v.posTTL)
	} else {
		v.cache.Set(h, false, v.negTTL)
	}
	return valid, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/`
Expected: PASS (ok).

- [ ] **Step 5: Commit**

```bash
git add internal/auth/validator.go internal/auth/validator_test.go
git commit -m "feat: token validator fronting store with cache"
```

---

### Task 6: Bearer auth middleware

**Files:**
- Create: `internal/auth/middleware.go`
- Test: `internal/auth/middleware_test.go`

**Interfaces:**
- Consumes: nothing beyond stdlib.
- Produces:
  - `type TokenValidator interface { Valid(ctx context.Context, token string) (bool, error) }`
  - `func Middleware(v TokenValidator) func(http.Handler) http.Handler`
  - Behavior: missing/malformed header or empty token → `401`; store error → `503`; invalid key → `401`; valid → next handler.
  - Note: `*Validator` (Task 5) satisfies `TokenValidator`.

- [ ] **Step 1: Write the failing test**

Create `internal/auth/middleware_test.go`:
```go
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubValidator struct {
	valid bool
	err   error
	gotToken string
}

func (s *stubValidator) Valid(_ context.Context, token string) (bool, error) {
	s.gotToken = token
	return s.valid, s.err
}

func nextOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("passed"))
	})
}

func doRequest(t *testing.T, v TokenValidator, header string) *httptest.ResponseRecorder {
	t.Helper()
	h := Middleware(v)(nextOK())
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareMissingHeader(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareMalformedHeader(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: true}, "Basic abc")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareInvalidKey(t *testing.T) {
	rec := doRequest(t, &stubValidator{valid: false}, "Bearer nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestMiddlewareStoreError(t *testing.T) {
	rec := doRequest(t, &stubValidator{err: errors.New("db down")}, "Bearer x")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestMiddlewareValidPassesToken(t *testing.T) {
	sv := &stubValidator{valid: true}
	rec := doRequest(t, sv, "Bearer secret-token")
	if rec.Code != http.StatusOK || rec.Body.String() != "passed" {
		t.Fatalf("code=%d body=%q, want 200/passed", rec.Code, rec.Body.String())
	}
	if sv.gotToken != "secret-token" {
		t.Fatalf("validator got token %q, want secret-token", sv.gotToken)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run TestMiddleware`
Expected: FAIL — `undefined: Middleware`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/auth/middleware.go`:
```go
package auth

import (
	"context"
	"net/http"
	"strings"
)

// TokenValidator reports whether a plaintext bearer token is valid.
type TokenValidator interface {
	Valid(ctx context.Context, token string) (bool, error)
}

const bearerPrefix = "bearer "

// Middleware returns net/http middleware that requires a valid
// "Authorization: Bearer <token>" header. On success it calls next; otherwise it
// writes 401 (missing/malformed/invalid) or 503 (store error).
func Middleware(v TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			valid, err := v.Valid(r.Context(), token)
			if err != nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			if !valid {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization header value, case-insensitive
// on the scheme. It returns ok=false for a missing scheme or empty token.
func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) ||
		!strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/`
Expected: PASS (ok) — validator and middleware tests both green.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/middleware.go internal/auth/middleware_test.go
git commit -m "feat: bearer auth middleware"
```

---

### Task 7: Streaming reverse proxy

**Files:**
- Create: `internal/proxy/proxy.go`
- Test: `internal/proxy/proxy_test.go`

**Interfaces:**
- Consumes: nothing beyond stdlib.
- Produces: `func New(target *url.URL) *httputil.ReverseProxy` — forwards to target, streams responses, returns `502` on upstream errors.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/proxy_test.go`:
```go
package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxyForwardsRequestAndResponse(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "pong")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat?q=1", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/chat" || gotQuery != "q=1" {
		t.Errorf("upstream got path=%q query=%q, want /v1/chat q=1", gotPath, gotQuery)
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("upstream Authorization = %q, want Bearer abc", gotAuth)
	}
	if gotBody != "hello" {
		t.Errorf("upstream body = %q, want hello", gotBody)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("missing upstream response header")
	}
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}
}

func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // block until the test has read the first chunk
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n') // should arrive before "second" is sent
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first chunk = %q, want it to contain 'first'", line)
	}
	close(release) // now allow the upstream to send the rest
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "second") {
		t.Fatalf("rest = %q, want it to contain 'second'", rest)
	}
}

func TestProxyUpstreamDownReturns502(t *testing.T) {
	// Point at a closed server to force a dial failure.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, _ := url.Parse(dead.URL)
	dead.Close()

	front := httptest.NewServer(New(target))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/proxy.go`:
```go
// Package proxy builds a streaming reverse proxy to the opentela upstream.
package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// New returns a reverse proxy that forwards every request to target, preserving
// method, path, query, headers, and body. Responses stream back immediately
// (FlushInterval -1), which matters for opentela's SSE/LLM output. Upstream
// failures produce a 502.
func New(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)     // routes to target scheme/host, joins base path, sets Host to target
			r.SetXForwarded()    // sets X-Forwarded-For/Host/Proto
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proxy/`
Expected: PASS (ok) — all three cases green.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/
git commit -m "feat: streaming reverse proxy to upstream"
```

---

### Task 8: Server wiring (mux + healthz)

**Files:**
- Create: `internal/server/server.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `auth.TokenValidator`, `auth.Middleware` (Task 6).
- Produces: `func New(v auth.TokenValidator, proxy http.Handler) http.Handler` — routes `/healthz` (open, `200 ok`) and everything else through the auth middleware to `proxy`.

- [ ] **Step 1: Write the failing test**

Create `internal/server/server_test.go`:
```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stubValidator struct{ valid bool }

func (s stubValidator) Valid(context.Context, string) (bool, error) { return s.valid, nil }

func proxyStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	})
}

func TestHealthzOpen(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz code = %d, want 200", rec.Code)
	}
}

func TestProxyRequiresAuth(t *testing.T) {
	h := New(stubValidator{valid: false}, proxyStub())
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy code = %d, want 401", rec.Code)
	}
}

func TestProxyPassesWhenValid(t *testing.T) {
	h := New(stubValidator{valid: true}, proxyStub())
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "proxied" {
		t.Fatalf("code=%d body=%q, want 200/proxied", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/server/server.go`:
```go
// Package server assembles the HTTP routes: an open health check and an
// authenticated catch-all that forwards to the proxy.
package server

import (
	"net/http"

	"github.com/opentela-ai/api/internal/auth"
)

// New builds the top-level handler. /healthz is unauthenticated; every other path
// passes through the Bearer auth middleware before reaching proxy.
func New(v auth.TokenValidator, proxy http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", auth.Middleware(v)(proxy))
	return mux
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/`
Expected: PASS (ok).

- [ ] **Step 5: Commit**

```bash
git add internal/server/
git commit -m "feat: server mux with health check and authenticated proxy route"
```

---

### Task 9: Service entrypoint (cmd/server) + README

**Files:**
- Create: `cmd/server/main.go`
- Create: `README.md`

**Interfaces:**
- Consumes: `config.Load`, `store.NewPostgres`, `cache.New`, `auth.NewValidator`, `proxy.New`, `server.New`.
- Produces: a runnable binary. No unit test; verified by build and a health-check smoke run.

- [ ] **Step 1: Write the entrypoint**

Create `cmd/server/main.go`:
```go
// Command server runs the opentela authenticating reverse proxy.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/proxy"
	"github.com/opentela-ai/api/internal/server"
	"github.com/opentela-ai/api/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()

	c := cache.New(cfg.JanitorEvery)
	defer c.Close()

	validator := auth.NewValidator(pg, c, cfg.CacheTTL, cfg.CacheNegTTL)
	handler := server.New(validator, proxy.New(cfg.UpstreamURL))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s, forwarding to %s", cfg.ListenAddr, cfg.UpstreamURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
```

Note: do not set a global `Server.WriteTimeout`; it would cut off long-lived streaming (SSE) responses. `ReadHeaderTimeout` guards against slow-header attacks without harming streaming.

- [ ] **Step 2: Verify it builds and vets**

Run:
```bash
go build ./...
go vet ./...
```
Expected: no output, exit 0.

- [ ] **Step 3: Write the README**

Create `README.md`:
```markdown
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
| `CACHE_JANITOR_INTERVAL` | no       | `10m`   | Expired-entry sweep interval         |

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
```

- [ ] **Step 4: Smoke test the health endpoint (optional, needs Postgres)**

Run (with a reachable Postgres and env set):
```bash
go run ./cmd/server &
sleep 1
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz   # expect 200
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/x       # expect 401
kill %1
```
Expected: `200` then `401`.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/ README.md
git commit -m "feat: service entrypoint with graceful shutdown and README"
```

---

### Task 10: keyctl CLI

**Files:**
- Create: `cmd/keyctl/main.go`

**Interfaces:**
- Consumes: `config`-style env (`DATABASE_URL`), `store.NewPostgres`, `store.HashKey`, `Postgres.{Migrate,Insert,Revoke,List}`.
- Produces: a CLI binary with subcommands `migrate`, `add`, `revoke`, `list`. Verified by build/vet and a manual run against Postgres.

- [ ] **Step 1: Write the CLI**

Create `cmd/keyctl/main.go`:
```go
// Command keyctl seeds and manages API keys in Postgres for the opentela proxy.
//
// Usage:
//
//	keyctl migrate
//	keyctl add [--name NAME]
//	keyctl revoke <token>
//	keyctl list
//
// DATABASE_URL must be set.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/opentela-ai/api/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: keyctl <migrate|add|revoke|list> [flags]")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		return err
	}
	defer pg.Close()

	switch args[0] {
	case "migrate":
		return cmdMigrate(ctx, pg)
	case "add":
		return cmdAdd(ctx, pg, args[1:])
	case "revoke":
		return cmdRevoke(ctx, pg, args[1:])
	case "list":
		return cmdList(ctx, pg)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdMigrate(ctx context.Context, pg *store.Postgres) error {
	ddl, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		return fmt.Errorf("read migration (run from repo root): %w", err)
	}
	if err := pg.Migrate(ctx, string(ddl)); err != nil {
		return err
	}
	fmt.Println("migration applied")
	return nil
}

func cmdAdd(ctx context.Context, pg *store.Postgres, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "human-readable label for the key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := generateToken()
	if err != nil {
		return err
	}
	if err := pg.Insert(ctx, store.HashKey(token), *name); err != nil {
		return err
	}
	fmt.Printf("created key (name=%q)\n", *name)
	fmt.Printf("token (shown once): %s\n", token)
	return nil
}

func cmdRevoke(ctx context.Context, pg *store.Postgres, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: keyctl revoke <token>")
	}
	changed, err := pg.Revoke(ctx, store.HashKey(args[0]))
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("no active key matched that token")
		return nil
	}
	fmt.Println("key revoked")
	return nil
}

func cmdList(ctx context.Context, pg *store.Postgres) error {
	keys, err := pg.List(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("no keys")
		return nil
	}
	for _, k := range keys {
		status := "active"
		if !k.Active {
			status = "revoked"
		}
		prefix := k.KeyHash
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		fmt.Printf("%s… name=%q %s created=%s\n",
			prefix, k.Name, status, k.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// generateToken returns a random opaque token of the form "sk-<48 hex chars>".
func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}
```

Note: the migration is read from `migrations/0001_init.sql` at runtime, so run `keyctl` from the repo root.

- [ ] **Step 2: Verify it builds and vets**

Run:
```bash
go build ./...
go vet ./...
```
Expected: no output, exit 0.

- [ ] **Step 3: Manual end-to-end check (optional, needs Postgres)**

Run from the repo root with `DATABASE_URL` set:
```bash
go run ./cmd/keyctl migrate
go run ./cmd/keyctl add --name alice     # copy the printed token
go run ./cmd/keyctl list                 # shows alice, active
go run ./cmd/keyctl revoke sk-...         # paste the token
go run ./cmd/keyctl list                 # shows alice, revoked
```
Expected: token created, listed active, then revoked.

- [ ] **Step 4: Commit**

```bash
git add cmd/keyctl/
git commit -m "feat: keyctl CLI for migrate/add/revoke/list"
```

---

### Task 11: Full-suite verification

**Files:** none (verification only).

- [ ] **Step 1: Run the whole suite with the race detector**

Run:
```bash
go build ./...
go vet ./...
go test -race ./...
```
Expected: all packages `ok` (the Postgres integration test reports `SKIP` without `TEST_DATABASE_URL`), no race warnings, no vet complaints.

- [ ] **Step 2: Commit any final touch-ups**

```bash
git add -A
git commit -m "chore: full-suite verification" --allow-empty
```

---

## Self-Review Notes

- **Spec coverage:** auth model (Tasks 5,6,8), transparent streaming proxy (Task 7), Postgres store with hashed keys (Tasks 3,4), 14-day/negative cache with janitor (Task 2), TTL/staleness accepted (no revocation propagation — Task 5 caches by outcome only), config env vars (Task 1), CLI seed/manage + migration (Tasks 3,10), TDD tests per unit incl. streaming and gated integration test — all mapped.
- **Error mapping:** 401 missing/malformed/invalid, 503 store error, 502 upstream down — Tasks 6 and 7.
- **Type consistency:** `Validator.Valid`, `TokenValidator.Valid`, `KeyStore.Validate`, `Postgres.{Validate,Insert,Revoke,List,Migrate,Close}`, `cache.New/Get/Set/Len/Close`, `store.HashKey`, `proxy.New`, `server.New`, `config.Load` used consistently across tasks.
