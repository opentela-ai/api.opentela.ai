package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/solana"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	// Ensure optional vars are unset so defaults apply.
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("PORT", "")
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
	if cfg.BetterStackSourceToken != "" {
		t.Errorf("BetterStackSourceToken = %q, want empty (Better Stack off by default)", cfg.BetterStackSourceToken)
	}
	if cfg.BetterStackLogLevel != slog.LevelInfo {
		t.Errorf("BetterStackLogLevel = %v, want Info", cfg.BetterStackLogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
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

// Railway injects PORT and routes the public domain to it; when set it must win
// over the static LISTEN_ADDR default so the listener matches where Railway
// sends traffic.
func TestLoadHonorsPort(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("PORT", "7142")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.ListenAddr != ":7142" {
		t.Errorf("ListenAddr = %q, want :7142", cfg.ListenAddr)
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

func TestLoadTinybirdPipeline(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TINYBIRD_APPEND_TOKEN", "append-token")
	t.Setenv("TINYBIRD_LEADERBOARD_TOKEN", "read-token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.TinybirdHost == nil || cfg.TinybirdHost.Host != "api.tinybird.co" {
		t.Errorf("TinybirdHost = %v, want default api.tinybird.co", cfg.TinybirdHost)
	}
	if cfg.TinybirdAppendToken != "append-token" || cfg.TinybirdLeaderboard != "read-token" {
		t.Errorf("Tinybird tokens = %q/%q", cfg.TinybirdAppendToken, cfg.TinybirdLeaderboard)
	}
}

func TestLoadTinybirdHostOverride(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TINYBIRD_APPEND_TOKEN", "append-token")
	t.Setenv("TINYBIRD_LEADERBOARD_TOKEN", "read-token")
	t.Setenv("TINYBIRD_HOST", "https://branch-api.tinybird.co")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := cfg.TinybirdHost.String(); got != "https://branch-api.tinybird.co" {
		t.Errorf("TinybirdHost = %q", got)
	}
}

func TestLoadTinybirdPartialIsError(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TINYBIRD_APPEND_TOKEN", "append-token")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with TINYBIRD_APPEND_TOKEN but no TINYBIRD_LEADERBOARD_TOKEN")
	}
}

func TestLoadRejectsTinybirdAndClickHouseTogether(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("CLICKHOUSE_URL", "https://ch.fly.dev:8443")
	t.Setenv("TINYBIRD_APPEND_TOKEN", "append-token")
	t.Setenv("TINYBIRD_LEADERBOARD_TOKEN", "read-token")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with both ClickHouse and Tinybird backends")
	}
}

func TestLoadBillingModeDefaultAndValid(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	// Default is off.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingMode != BillingOff {
		t.Fatalf("BillingMode = %q, want off", cfg.BillingMode)
	}
	// enforce is accepted now that settlement (Step 4) is wired; only
	// off, observe, and enforce are valid.
	for _, m := range []string{"off", "observe", "enforce"} {
		t.Setenv("BILLING_MODE", m)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("BILLING_MODE=%s: %v", m, err)
		}
		if string(cfg.BillingMode) != m {
			t.Fatalf("BillingMode = %q, want %s", cfg.BillingMode, m)
		}
	}
	t.Setenv("BILLING_MODE", "observe")
	if cfg, _ := Load(); cfg.BillingMode == BillingEnforce {
		t.Fatal("observe must not equal enforce")
	}
}

func TestLoadBillingEnforceAccepted(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_MODE", "enforce")
	t.Setenv("BILLING_FEE_BPS", "250")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("enforce should be accepted now that settlement is wired: %v", err)
	}
	if cfg.BillingMode != BillingEnforce {
		t.Fatalf("BillingMode = %q, want enforce", cfg.BillingMode)
	}
	if cfg.BillingFeeBps != 250 {
		t.Fatalf("BillingFeeBps = %d, want 250", cfg.BillingFeeBps)
	}
	if cfg.BillingSweepInterval <= 0 || cfg.BillingSweepAge <= 0 {
		t.Fatalf("sweep config not set: interval=%v age=%v", cfg.BillingSweepInterval, cfg.BillingSweepAge)
	}
}

func TestLoadBillingModeInvalid(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_MODE", "collect")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid BILLING_MODE")
	}
}

func TestLoadBillingOutputMax(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_OUTPUT_TOKEN_MAX", "8192")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingOutputMax != 8192 {
		t.Fatalf("BillingOutputMax = %d, want 8192", cfg.BillingOutputMax)
	}
}

func TestLoadDepositConfig(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	t.Setenv("BILLING_SOLANA_RPC_URL", "https://rpc.example")
	t.Setenv("BILLING_DEPOSIT_MINT", "So11111111111111111111111111111111111111112")
	t.Setenv("BILLING_DEPOSIT_POLL_INTERVAL", "45s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingTreasuryWallet != "11111111111111111111111111111112" {
		t.Fatalf("treasury = %q", cfg.BillingTreasuryWallet)
	}
	if cfg.BillingSolanaRPC != "https://rpc.example" {
		t.Fatalf("rpc = %q", cfg.BillingSolanaRPC)
	}
	if cfg.BillingDepositMint != "So11111111111111111111111111111111111111112" {
		t.Fatalf("mint = %q", cfg.BillingDepositMint)
	}
	if cfg.BillingDepositPollInterval != 45*time.Second {
		t.Fatalf("poll = %v, want 45s", cfg.BillingDepositPollInterval)
	}
}

func TestLoadDepositConfigDefaultsToFaucetRPC(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	t.Setenv("FAUCET_SOLANA_RPC_URL", "https://faucet.rpc")
	t.Setenv("FAUCET_MINT", "MintBase58")
	t.Setenv("FAUCET_WALLET_KEYPAIR", strings.Repeat("A", 86)+"==") // 64-byte (structurally valid) key
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingSolanaRPC != "https://faucet.rpc" {
		t.Fatalf("rpc should default to faucet: %q", cfg.BillingSolanaRPC)
	}
	if cfg.BillingDepositMint != "MintBase58" {
		t.Fatalf("mint should default to faucet: %q", cfg.BillingDepositMint)
	}
}

func TestLoadDepositConfigMissingRPC(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	// Neither BILLING_SOLANA_RPC_URL nor FAUCET_SOLANA_RPC_URL set.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BILLING_SOLANA_RPC_URL") {
		t.Fatalf("err = %v, want mention of BILLING_SOLANA_RPC_URL", err)
	}
}

func TestLoadDepositConfigMissingMint(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	t.Setenv("BILLING_SOLANA_RPC_URL", "https://rpc.example")
	// Neither BILLING_DEPOSIT_MINT nor FAUCET_MINT set.
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BILLING_DEPOSIT_MINT") {
		t.Fatalf("err = %v, want mention of BILLING_DEPOSIT_MINT", err)
	}
}

func TestLoadDepositConfigOffWhenNoTreasury(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	// No BILLING_TREASURY_WALLET: even without RPC/mint, Load must succeed.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingTreasuryWallet != "" {
		t.Fatalf("treasury should be empty, got %q", cfg.BillingTreasuryWallet)
	}
}

// genTreasury builds a real ed25519 keypair and returns the base64 keypair
// env value plus the matching base58 wallet pubkey, so the withdrawal config
// tests can exercise the on-boot verification.
func genTreasury(t *testing.T) (keypairB64, walletB58 string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(priv), solana.EncodeBase58(pub)
}

func TestLoadWithdrawalKeypairVerified(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	keypairB64, walletB58 := genTreasury(t)
	t.Setenv("BILLING_TREASURY_WALLET", walletB58)
	t.Setenv("BILLING_SOLANA_RPC_URL", "https://rpc.example")
	t.Setenv("BILLING_DEPOSIT_MINT", "So11111111111111111111111111111111111111112")
	t.Setenv("BILLING_TREASURY_KEYPAIR", keypairB64)
	t.Setenv("BILLING_WITHDRAW_POLL_INTERVAL", "7s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BillingTreasuryKeypair == nil {
		t.Fatal("treasury keypair nil")
	}
	if got := solana.EncodeBase58(cfg.BillingTreasuryKeypair.Public().(ed25519.PublicKey)); got != walletB58 {
		t.Fatalf("keypair pubkey = %q, want %q", got, walletB58)
	}
	if cfg.BillingWithdrawPollInterval != 7*time.Second {
		t.Fatalf("poll = %v, want 7s", cfg.BillingWithdrawPollInterval)
	}
	if cfg.BillingWithdrawBlockhashMaxAge <= 0 {
		t.Fatalf("blockhash max age = %v, want > 0", cfg.BillingWithdrawBlockhashMaxAge)
	}
}

func TestLoadWithdrawalKeypairMismatch(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	keypairB64, _ := genTreasury(t)
	// A different wallet than the keypair's public key.
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	t.Setenv("BILLING_SOLANA_RPC_URL", "https://rpc.example")
	t.Setenv("BILLING_DEPOSIT_MINT", "So11111111111111111111111111111111111111112")
	t.Setenv("BILLING_TREASURY_KEYPAIR", keypairB64)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want mention of mismatch", err)
	}
}

func TestLoadWithdrawalKeypairWithoutWallet(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	keypairB64, _ := genTreasury(t)
	// Keypair set but no treasury wallet: should fail (cannot verify owner).
	t.Setenv("BILLING_TREASURY_KEYPAIR", keypairB64)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BILLING_TREASURY_WALLET") {
		t.Fatalf("err = %v, want mention of BILLING_TREASURY_WALLET", err)
	}
}

func TestLoadWithdrawalKeypairBadBase64(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_TREASURY_WALLET", "11111111111111111111111111111112")
	t.Setenv("BILLING_SOLANA_RPC_URL", "https://rpc.example")
	t.Setenv("BILLING_DEPOSIT_MINT", "So11111111111111111111111111111111111111112")
	t.Setenv("BILLING_TREASURY_KEYPAIR", "!!not-base64!!")
	if _, err := Load(); err == nil {
		t.Fatal("Load should reject a non-base64 keypair")
	}
}

// Observability defaults: Better Stack is off, level INFO, stdout JSON. The
// happy path below asserts that a valid token is carried through unchanged and
// that the level/format overrides parse.
func TestLoadObservabilityDefaults(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BETTERSTACK_SOURCE_TOKEN", "")
	t.Setenv("BETTERSTACK_LOG_LEVEL", "")
	t.Setenv("LOG_FORMAT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BetterStackSourceToken != "" {
		t.Errorf("BetterStackSourceToken = %q, want empty", cfg.BetterStackSourceToken)
	}
	if cfg.BetterStackLogLevel != slog.LevelInfo {
		t.Errorf("BetterStackLogLevel = %v, want Info", cfg.BetterStackLogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
}

func TestLoadObservabilityOverrides(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BETTERSTACK_SOURCE_TOKEN", "a-very-long-source-token-value")
	t.Setenv("BETTERSTACK_LOG_LEVEL", "warn")
	t.Setenv("LOG_FORMAT", "text")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.BetterStackSourceToken != "a-very-long-source-token-value" {
		t.Errorf("BetterStackSourceToken = %q, want carried through", cfg.BetterStackSourceToken)
	}
	if cfg.BetterStackLogLevel != slog.LevelWarn {
		t.Errorf("BetterStackLogLevel = %v, want Warn", cfg.BetterStackLogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
}

func TestLoadObservabilityRejectsShortToken(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BETTERSTACK_SOURCE_TOKEN", "short-token")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BETTERSTACK_SOURCE_TOKEN") {
		t.Fatalf("Load() err = %v, want one mentioning BETTERSTACK_SOURCE_TOKEN", err)
	}
}

func TestLoadObservabilityRejectsWhitespaceToken(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BETTERSTACK_SOURCE_TOKEN", "  0123456789012345  ")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BETTERSTACK_SOURCE_TOKEN") {
		t.Fatalf("Load() err = %v, want one mentioning BETTERSTACK_SOURCE_TOKEN", err)
	}
}

func TestLoadObservabilityRejectsBadLevel(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BETTERSTACK_LOG_LEVEL", "verbose")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BETTERSTACK_LOG_LEVEL") {
		t.Fatalf("Load() err = %v, want one mentioning BETTERSTACK_LOG_LEVEL", err)
	}
}

func TestLoadObservabilityRejectsBadFormat(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LOG_FORMAT", "yaml")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("Load() err = %v, want one mentioning LOG_FORMAT", err)
	}
}

func TestLoadSettlementAuthority(t *testing.T) {
	t.Setenv("OPENTELA_UPSTREAM_URL", "https://api.opentela.ai")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")

	t.Run("unset: delegation off", func(t *testing.T) {
		t.Setenv("BILLING_SETTLEMENT_AUTHORITY", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.BillingSettlementAuthority != "" {
			t.Errorf("authority = %q, want empty", cfg.BillingSettlementAuthority)
		}
		if cfg.BillingAllowanceRefresh != 60*time.Second {
			t.Errorf("refresh = %v, want 60s", cfg.BillingAllowanceRefresh)
		}
	})

	t.Run("valid pubkey accepted", func(t *testing.T) {
		t.Setenv("BILLING_SETTLEMENT_AUTHORITY", "LaAGasGwQCLHdUMErLAvqPwULmqhyWYTbM7GoFYJffm")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.BillingSettlementAuthority != "LaAGasGwQCLHdUMErLAvqPwULmqhyWYTbM7GoFYJffm" {
			t.Errorf("authority = %q", cfg.BillingSettlementAuthority)
		}
	})

	t.Run("invalid pubkey rejected", func(t *testing.T) {
		t.Setenv("BILLING_SETTLEMENT_AUTHORITY", "not-a-pubkey with spaces")
		if _, err := Load(); err == nil {
			t.Fatal("invalid authority must fail")
		}
	})
}
