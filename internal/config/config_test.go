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
