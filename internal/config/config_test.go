package config

import (
	"crypto/ed25519"
	"encoding/base64"
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
	if cfg.JanitorEvery != 1*time.Minute {
		t.Errorf("JanitorEvery = %v, want 1m", cfg.JanitorEvery)
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

func TestLoadRejectsNonPositiveDuration(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("CACHE_TTL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for CACHE_TTL=0s, got nil")
	}
	t.Setenv("CACHE_TTL", "-1h")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for negative CACHE_TTL, got nil")
	}
}

func TestLoadRejectsMalformedDuration(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("CACHE_NEGATIVE_TTL", "notaduration")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for malformed CACHE_NEGATIVE_TTL, got nil")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "")
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when required vars missing, got nil")
	}
}

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

func TestLoadKeyMgmtPartialIssuerOnlyIsError(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NEON_AUTH_ISSUER", "https://auth") // jwks url missing
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when only NEON_AUTH_ISSUER is set")
	}
}

func TestLoadRejectsNonIntegerMaxKeys(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MAX_KEYS_PER_USER", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-integer MAX_KEYS_PER_USER")
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

func TestLoadInternalACLRequiresHighEntropyToken(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("INTERNAL_CONTROL_TOKEN", "too-short")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for short INTERNAL_CONTROL_TOKEN")
	}

	t.Setenv("INTERNAL_CONTROL_TOKEN", " 01234567890123456789012345678901")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for token with surrounding whitespace")
	}

	t.Setenv("INTERNAL_CONTROL_TOKEN", "01234567890123456789012345678901")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() valid internal token: %v", err)
	}
	if !cfg.InternalACLEnabled || cfg.InternalControlToken == "" {
		t.Fatalf("internal ACL config = enabled:%v token:%q", cfg.InternalACLEnabled, cfg.InternalControlToken)
	}
}

func TestLoadNodeCredentialSigningRequiresInternalToken(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NODE_CREDENTIAL_SIGNING_KID", "kid-current")
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	t.Setenv("NODE_CREDENTIAL_SIGNING_KEY", base64.RawURLEncoding.EncodeToString(privateKey))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when node credential signing is enabled without INTERNAL_CONTROL_TOKEN")
	}

	t.Setenv("INTERNAL_CONTROL_TOKEN", "01234567890123456789012345678901")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with internal token: %v", err)
	}
	if !cfg.NodeCredentialEnabled {
		t.Fatal("NodeCredentialEnabled should be true")
	}
}

func TestLoadClickHousePipeline(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("CLICKHOUSE_URL", "https://ch.fly.dev:8443")
	t.Setenv("CLICKHOUSE_USERNAME", "default")
	t.Setenv("CLICKHOUSE_PASSWORD", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ClickHouseURL == nil || cfg.ClickHouseURL.Host != "ch.fly.dev:8443" {
		t.Errorf("ClickHouseURL = %v", cfg.ClickHouseURL)
	}
	if cfg.ClickHouseDatabase != "opentela" {
		t.Errorf("ClickHouseDatabase = %q, want opentela", cfg.ClickHouseDatabase)
	}
	if cfg.PerfFlushInterval != 5*time.Second || cfg.PerfBatchSize != 1024 || cfg.PerfQueueSize != 16384 {
		t.Errorf("perf settings = %v/%d/%d", cfg.PerfFlushInterval, cfg.PerfBatchSize, cfg.PerfQueueSize)
	}
}

func TestLoadClickHouseDisabledByDefault(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ClickHouseURL != nil {
		t.Errorf("ClickHouseURL = %v, want nil (pipeline off)", cfg.ClickHouseURL)
	}
}

func TestLoadRejectsClickHouseAuthWithoutURL(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("CLICKHOUSE_PASSWORD", "secret")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with CLICKHOUSE_PASSWORD but no CLICKHOUSE_URL")
	}
}

func TestLoadRejectsBadClickHouseValues(t *testing.T) {
	base := func() {
		t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
		t.Setenv("DATABASE_URL", "postgres://x")
	}
	base()
	t.Setenv("CLICKHOUSE_URL", "not a url")
	if _, err := Load(); err == nil {
		t.Error("Load() succeeded with relative CLICKHOUSE_URL")
	}
	t.Setenv("CLICKHOUSE_URL", "http://ch:8123")
	t.Setenv("CLICKHOUSE_DATABASE", "bad;db")
	if _, err := Load(); err == nil {
		t.Error("Load() succeeded with non-identifier CLICKHOUSE_DATABASE")
	}
	t.Setenv("CLICKHOUSE_DATABASE", "opentela")
	t.Setenv("PERF_BATCH_SIZE", "0")
	if _, err := Load(); err == nil {
		t.Error("Load() succeeded with PERF_BATCH_SIZE=0")
	}
}
