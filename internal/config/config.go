// Package config loads and validates service configuration from the environment.
package config

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/solana"
)

// Config holds all runtime configuration for the proxy service.
type Config struct {
	UpstreamURL  *url.URL
	DatabaseURL  string
	ListenAddr   string
	CacheTTL     time.Duration
	CacheNegTTL  time.Duration
	JanitorEvery time.Duration

	// Observability (optional). When BETTERSTACK_SOURCE_TOKEN is set,
	// structured logs are also shipped to Better Stack Logs
	// (https://betterstack.com/logs) via the slog-betterstack handler — the
	// Better Stack Go integration. Without a token the service writes JSON
	// to stdout only and makes no network calls. The log level gates every
	// sink (stdout included), and LOG_FORMAT selects the stdout record
	// format (json by default, text for local readability); Better Stack
	// always receives a structured JSON payload. See internal/obs.
	BetterStackSourceToken string
	BetterStackLogLevel    slog.Level
	LogFormat              string

	// Key-management plane (optional). Enabled only when both NeonAuthJWKSURL and
	// NeonAuthIssuer are set.
	NeonAuthJWKSURL    string
	NeonAuthIssuer     string
	NeonAuthAudience   string
	JWKSCacheTTL       time.Duration
	MaxKeysPerUser     int
	CORSAllowedOrigins []string
	KeyMgmtEnabled     bool

	InternalControlToken string
	IdentityMaxAge       time.Duration
	OwnershipMaxAge      time.Duration
	DecisionCacheTTL     time.Duration
	InternalACLEnabled   bool

	// EvaluatorRateLimit caps live v2 evaluations per authenticated caller.
	// Trusted decisions are never cached client-side, so without a cap a
	// single authorized head can drive the control plane at unbounded rate.
	// Zero (the default) disables the limiter.
	EvaluatorRateLimitRPS   float64
	EvaluatorRateLimitBurst int

	NodeCredentialIssuer     string
	NodeCredentialSigningKID string
	NodeCredentialSigningKey ed25519.PrivateKey
	NodeCredentialVerifyKeys map[string]ed25519.PublicKey
	NodeCredentialEnabled    bool

	// OTELA faucet (optional). Enabled when FAUCET_WALLET_KEYPAIR, FAUCET_MINT,
	// and FAUCET_SOLANA_RPC_URL are all set. Verified accounts can claim one
	// FAUCET_AMOUNT payout to their linked wallet's associated token account.
	FaucetEnabled      bool
	FaucetRPCURL       string
	FaucetMint         string
	FaucetTokenProgram string
	FaucetWalletKey    ed25519.PrivateKey
	FaucetAmountRaw    uint64
	FaucetDecimals     int

	// GPU performance pipeline (optional). The proxy samples every routed
	// inference response into an analytics store and the public
	// /v1/leaderboard endpoint is mounted. Two interchangeable backends:
	//   - Tinybird Forward: enabled by TINYBIRD_APPEND_TOKEN +
	//     TINYBIRD_LEADERBOARD_TOKEN (+ optional TINYBIRD_HOST). Managed; the
	//     schema and endpoint live in tinybird/ and deploy via tb deploy.
	//   - Self-managed ClickHouse: enabled by CLICKHOUSE_URL (and the schema
	//     in clickhouse/schema.sql).
	// The backends are mutually exclusive. Everything stays disabled when
	// neither is set; auxiliary settings without their backend switch are
	// rejected below.
	TinybirdHost        *url.URL
	TinybirdAppendToken string
	TinybirdLeaderboard string
	ClickHouseURL       *url.URL
	ClickHouseDatabase  string
	ClickHouseUsername  string
	ClickHousePassword  string
	PerfFlushInterval   time.Duration // sample batch insert cadence
	PerfBatchSize       int           // max samples per insert
	PerfQueueSize       int           // in-memory backlog before dropping
	PerfPeerCacheTTL    time.Duration // node-table GPU inventory cache
	LeaderboardCacheTTL time.Duration // public aggregate cache

	// Billing gates the inference proxy with an off-chain OTELA ledger.
	//   - off:     no billing; the API, catalog, proxy, and UI behave as today.
	//   - observe: the gate and meter run, but no request is rejected and no
	//             balance moves — pricing/usage is validated against real
	//             traffic before enforcing. The forwarded request is never
	//             modified, so observe cannot affect routing.
	//   - enforce: the production posture. The gate reserves before
	//             forwarding and the settlement hook (Step 4) settles exactly
	//             once on a 2xx response with usage, releasing the
	//             reservation otherwise. A recovery worker reclaims
	//             reservations whose hook never ran.
	BillingMode          BillingMode
	BillingOutputMax     int           // conservative per-request output-token ceiling when the body omits one (0 = reject generative routes without one)
	BillingFeeBps        int           // routing fee in basis points (0–10000) credited to the treasury on settlement
	BillingSweepInterval time.Duration // stale-reservation recovery cadence
	BillingSweepAge      time.Duration // a reserved request older than this is reclaimed
	// BillingRequirePricedPeer: when true, a peer with no live ask is excluded
	// from the affordable set for any (service, model) that has at least one
	// published ask, instead of being eligible at a zero quote (free). Routes
	// with no live asks keep the original behavior so a cold market boots.
	BillingRequirePricedPeer bool

	// Deposits (Step 5). The watcher polls the treasury's associated token
	// account for finalized inbound SPL transfers of the OTELA mint and
	// credits the linked owner. BillingTreasuryWallet is the pubkey that owns
	// the treasury ATA (deposits arrive there); BillingSolanaRPC is the RPC
	// endpoint (defaults to the faucet's when unset). The mint and token
	// program default to the faucet's, since devnet deposits use the same
	// network. The watcher runs only when BILLING_MODE != off AND
	// BillingTreasuryWallet is set.
	BillingTreasuryWallet      string
	BillingSolanaRPC           string
	BillingDepositMint         string
	BillingDepositTokenProgram string
	BillingDepositDecimals     int
	BillingDepositPollInterval time.Duration

	// Withdrawals (Step 7). The worker drains durable withdrawal records
	// (reserved -> signed -> broadcast -> finalized) into on-chain SPL
	// transfers signed by BillingTreasuryKeypair. The keypair is OPTIONAL
	// for deposits (which only need the wallet pubkey) but REQUIRED for
	// withdrawals: when it is absent the deposit watcher still runs, but no
	// withdrawal worker is started. On boot the keypair's public key is
	// verified to match BillingTreasuryWallet, so a misconfigured key cannot
	// drain the treasury to the wrong recipient.
	BillingTreasuryKeypair         ed25519.PrivateKey
	BillingWithdrawPollInterval    time.Duration
	BillingWithdrawBlockhashMaxAge time.Duration

	// Delegations (Phase 2, design §11). BillingSettlementAuthority is the
	// base58 pubkey of the settlement authority — the delegate every buyer
	// names in their SPL approve. When set (and billing is on), the allowance
	// poller re-reads each linked wallet's ATA delegated amount every
	// BILLING_ALLOWANCE_REFRESH and mirrors grants/revocations into the
	// registry, so the reserve gate's min(credit, Σ allowance) bound tracks
	// the chain. Unset = delegation disabled; the custodial deposit rail
	// alone backs spend.
	BillingSettlementAuthority string
	BillingAllowanceRefresh    time.Duration

	// Settlement worker (Phase 2, §11.5). The keypair signs transfer_checked
	// batches replaying delegation-backed ledger legs on-chain; it IS the
	// delegate, verified against BillingSettlementAuthority on boot (a
	// mismatch is a config error, not a graceful degradation). The authority
	// keypair also pays transaction fees, so it must hold SOL.
	BillingSettlementKeypair      ed25519.PrivateKey
	BillingSettlementPollInterval time.Duration
}

// BillingMode selects the off-chain billing posture for the inference proxy.
type BillingMode string

const (
	BillingOff     BillingMode = "off"
	BillingObserve BillingMode = "observe"
	BillingEnforce BillingMode = "enforce"
)

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
	janitor, err := durationEnv("CACHE_JANITOR_INTERVAL", 1*time.Minute)
	if err != nil {
		return nil, err
	}

	// Observability (optional). Better Stack ships only when a source token is
	// set; everything else has a sensible default and is independent, so there
	// are no paired-setting rules here.
	betterStackToken := os.Getenv("BETTERSTACK_SOURCE_TOKEN")
	if betterStackToken != "" {
		if strings.TrimSpace(betterStackToken) != betterStackToken {
			return nil, fmt.Errorf("BETTERSTACK_SOURCE_TOKEN must not contain surrounding whitespace")
		}
		if len(betterStackToken) < 16 {
			return nil, fmt.Errorf("BETTERSTACK_SOURCE_TOKEN must be at least 16 bytes")
		}
	}
	betterStackLevel, err := levelEnv("BETTERSTACK_LOG_LEVEL", slog.LevelInfo)
	if err != nil {
		return nil, err
	}
	logFormat := strings.ToLower(stringEnv("LOG_FORMAT", "json"))
	if logFormat != "json" && logFormat != "text" {
		return nil, fmt.Errorf("LOG_FORMAT must be one of json|text, got %q", logFormat)
	}

	jwksTTL, err := durationEnv("NEON_AUTH_JWKS_CACHE_TTL", time.Hour)
	if err != nil {
		return nil, err
	}
	maxKeys, err := intEnv("MAX_KEYS_PER_USER", 10)
	if err != nil {
		return nil, err
	}
	identityMaxAge, err := durationEnv("IDENTITY_MAX_AGE", 30*24*time.Hour)
	if err != nil {
		return nil, err
	}
	ownershipMaxAge, err := durationEnv("OWNERSHIP_MAX_AGE", 30*time.Second)
	if err != nil {
		return nil, err
	}
	decisionTTL, err := durationEnv("DECISION_CACHE_TTL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	evalRPS, err := floatEnv("EVALUATOR_RATE_LIMIT_RPS", 50)
	if err != nil {
		return nil, err
	}
	evalBurst, err := intEnv("EVALUATOR_RATE_LIMIT_BURST", 100)
	if err != nil {
		return nil, err
	}
	internalControlToken := os.Getenv("INTERNAL_CONTROL_TOKEN")
	if internalControlToken != "" {
		if strings.TrimSpace(internalControlToken) != internalControlToken {
			return nil, fmt.Errorf("INTERNAL_CONTROL_TOKEN must not contain surrounding whitespace")
		}
		if len(internalControlToken) < 32 {
			return nil, fmt.Errorf("INTERNAL_CONTROL_TOKEN must be at least 32 bytes")
		}
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

	nodeCredentialIssuer := stringEnv("NODE_CREDENTIAL_ISSUER", "api.opentela.ai")
	nodeCredentialSigningKID := os.Getenv("NODE_CREDENTIAL_SIGNING_KID")
	var nodeCredentialSigningKey ed25519.PrivateKey
	nodeCredentialVerifyKeys := map[string]ed25519.PublicKey{}
	if raw := os.Getenv("NODE_CREDENTIAL_SIGNING_KEY"); raw != "" {
		key, err := nodecred.DecodeSigningKey(raw)
		if err != nil {
			return nil, fmt.Errorf("NODE_CREDENTIAL_SIGNING_KEY is invalid: %w", err)
		}
		nodeCredentialSigningKey = key
		pub := key.Public().(ed25519.PublicKey)
		if nodeCredentialSigningKID == "" {
			return nil, fmt.Errorf("NODE_CREDENTIAL_SIGNING_KID is required when NODE_CREDENTIAL_SIGNING_KEY is set")
		}
		nodeCredentialVerifyKeys[nodeCredentialSigningKID] = pub
	}
	if raw := os.Getenv("NODE_CREDENTIAL_VERIFY_KEYS"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			kid, encoded, ok := strings.Cut(entry, ":")
			if !ok || kid == "" || encoded == "" {
				return nil, fmt.Errorf("NODE_CREDENTIAL_VERIFY_KEYS entry %q must be kid:base64", entry)
			}
			pub, err := nodecred.DecodePublicKey(encoded)
			if err != nil {
				return nil, fmt.Errorf("NODE_CREDENTIAL_VERIFY_KEYS entry %q is invalid: %w", entry, err)
			}
			nodeCredentialVerifyKeys[kid] = pub
		}
	}
	if len(nodeCredentialSigningKey) == ed25519.PrivateKeySize && internalControlToken == "" {
		return nil, fmt.Errorf("INTERNAL_CONTROL_TOKEN is required when NODE_CREDENTIAL_SIGNING_KEY is set")
	}

	// OTELA faucet (optional). All-or-nothing: setting any faucet variable
	// without the wallet keypair, mint, and RPC URL is a config error.
	faucetKeyRaw := os.Getenv("FAUCET_WALLET_KEYPAIR")
	faucetMint := os.Getenv("FAUCET_MINT")
	faucetRPC := os.Getenv("FAUCET_SOLANA_RPC_URL")
	faucetTokenProgram := stringEnv("FAUCET_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	faucetAmount, err := uint64Env("FAUCET_AMOUNT", 1_000_000) // 1 OTELA at 6 decimals
	if err != nil {
		return nil, err
	}
	faucetDecimals, err := intEnv("FAUCET_DECIMALS", 6)
	if err != nil {
		return nil, err
	}
	if faucetKeyRaw != "" || faucetMint != "" || faucetRPC != "" {
		if faucetKeyRaw == "" {
			return nil, fmt.Errorf("FAUCET_WALLET_KEYPAIR is required when the faucet is configured")
		}
		if faucetMint == "" {
			return nil, fmt.Errorf("FAUCET_MINT is required when the faucet is configured")
		}
		if faucetRPC == "" {
			return nil, fmt.Errorf("FAUCET_SOLANA_RPC_URL is required when the faucet is configured")
		}
	}
	var faucetKey ed25519.PrivateKey
	if faucetKeyRaw != "" {
		key, err := nodecred.DecodeSigningKey(faucetKeyRaw)
		if err != nil {
			return nil, fmt.Errorf("FAUCET_WALLET_KEYPAIR is invalid: %w", err)
		}
		faucetKey = key
	}

	var clickHouseURL *url.URL
	if raw := os.Getenv("CLICKHOUSE_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("CLICKHOUSE_URL must be an absolute URL (e.g. http://clickhouse:8123)")
		}
		clickHouseURL = u
	}
	clickHouseDatabase := stringEnv("CLICKHOUSE_DATABASE", "opentela")
	if !validCHIdentifier(clickHouseDatabase) {
		return nil, fmt.Errorf("CLICKHOUSE_DATABASE must match [A-Za-z0-9_]{1,64}")
	}
	clickHouseUsername := os.Getenv("CLICKHOUSE_USERNAME")
	clickHousePassword := os.Getenv("CLICKHOUSE_PASSWORD")
	if clickHouseURL == nil && (clickHouseUsername != "" || clickHousePassword != "") {
		return nil, fmt.Errorf("CLICKHOUSE_USERNAME and CLICKHOUSE_PASSWORD require CLICKHOUSE_URL")
	}
	var tinybirdHost *url.URL
	if raw := stringEnv("TINYBIRD_HOST", "https://api.tinybird.co"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("TINYBIRD_HOST must be an absolute URL (e.g. https://api.tinybird.co)")
		}
		tinybirdHost = u
	}
	tinybirdAppend := os.Getenv("TINYBIRD_APPEND_TOKEN")
	tinybirdRead := os.Getenv("TINYBIRD_LEADERBOARD_TOKEN")
	if (tinybirdAppend == "") != (tinybirdRead == "") {
		return nil, fmt.Errorf("TINYBIRD_APPEND_TOKEN and TINYBIRD_LEADERBOARD_TOKEN must be set together")
	}
	if tinybirdAppend != "" && clickHouseURL != nil {
		return nil, fmt.Errorf("CLICKHOUSE_URL and TINYBIRD_APPEND_TOKEN are mutually exclusive — pick one perf backend")
	}
	perfFlushInterval, err := durationEnv("PERF_FLUSH_INTERVAL", 5*time.Second)
	if err != nil {
		return nil, err
	}
	perfBatchSize, err := intEnv("PERF_BATCH_SIZE", 1024)
	if err != nil {
		return nil, err
	}
	perfQueueSize, err := intEnv("PERF_QUEUE_SIZE", 16384)
	if err != nil {
		return nil, err
	}
	perfPeerCacheTTL, err := durationEnv("PERF_PEER_CACHE_TTL", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	leaderboardCacheTTL, err := durationEnv("LEADERBOARD_CACHE_TTL", 1*time.Minute)
	if err != nil {
		return nil, err
	}

	billingMode := BillingMode(stringEnv("BILLING_MODE", string(BillingOff)))
	switch billingMode {
	case BillingOff, BillingObserve, BillingEnforce:
	default:
		return nil, fmt.Errorf("BILLING_MODE must be one of off|observe|enforce, got %q", billingMode)
	}
	billingOutputMax, err := intEnv("BILLING_OUTPUT_TOKEN_MAX", 0)
	if err != nil {
		return nil, err
	}
	billingFeeBps, err := intEnv("BILLING_FEE_BPS", 0)
	if err != nil {
		return nil, err
	}
	if billingFeeBps < 0 || billingFeeBps > 10000 {
		return nil, fmt.Errorf("BILLING_FEE_BPS must be 0–10000, got %d", billingFeeBps)
	}
	billingSweepInterval, err := durationEnv("BILLING_SWEEP_INTERVAL", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	// A reserved request is stale once it outlives the upstream timeout plus a
	// generous margin for streaming tails; default 15 minutes is well above any
	// reasonable inference request lifetime so in-flight responses are not
	// reclaimed mid-stream.
	billingSweepAge, err := durationEnv("BILLING_SWEEP_AGE", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	// A peer that never published is eligible at a zero quote by default (no
	// price, the request is free). Requiring priced peers stops that default
	// from silently converting a seller whose asks expired (or who never
	// published) into free capacity once any live ask exists for the route.
	billingRequirePricedPeer, err := boolEnv("BILLING_REQUIRE_PRICED_PEER", false)
	if err != nil {
		return nil, err
	}

	// Deposits (Step 5). The treasury wallet owns the associated token
	// account that receives deposits; when unset the watcher stays off. The
	// RPC defaults to the faucet's endpoint so a single devnet RPC serves
	// both, and the mint / token program default to the faucet's since devnet
	// deposits use the same network.
	billingTreasury := os.Getenv("BILLING_TREASURY_WALLET")
	billingSolanaRPC := stringEnv("BILLING_SOLANA_RPC_URL", faucetRPC)
	billingMint := stringEnv("BILLING_DEPOSIT_MINT", faucetMint)
	billingTokenProgram := stringEnv("BILLING_DEPOSIT_TOKEN_PROGRAM", faucetTokenProgram)
	billingPollInterval, err := durationEnv("BILLING_DEPOSIT_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	billingDecimals, err := intEnv("BILLING_DEPOSIT_DECIMALS", faucetDecimals)
	if err != nil {
		return nil, err
	}
	if billingTreasury != "" {
		if billingSolanaRPC == "" {
			return nil, fmt.Errorf("BILLING_TREASURY_WALLET set but BILLING_SOLANA_RPC_URL is empty (and FAUCET_SOLANA_RPC_URL unset)")
		}
		if billingMint == "" {
			return nil, fmt.Errorf("BILLING_TREASURY_WALLET set but BILLING_DEPOSIT_MINT is empty (and FAUCET_MINT unset)")
		}
		if billingDecimals < 0 || billingDecimals > 18 {
			return nil, fmt.Errorf("BILLING_DEPOSIT_DECIMALS must be 0–18, got %d", billingDecimals)
		}
	}

	// Withdrawals (Step 7). The treasury keypair is OPTIONAL for deposits
	// but REQUIRED to start the withdrawal worker. When provided, its public
	// key MUST match BILLING_TREASURY_WALLET — otherwise the worker would
	// sign transfers whose authority is not the wallet the deposit watcher
	// credits, which is a configuration error rather than a graceful
	// degradation. BILLING_WITHDRAW_POLL_INTERVAL bounds the sweep cadence;
	// BILLING_WITHDRAW_BLOCKHASH_MAX_AGE is the conservative wall-clock
	// window after which a non-finalized broadcast is treated as expired.
	var treasuryKeypair ed25519.PrivateKey
	if raw := os.Getenv("BILLING_TREASURY_KEYPAIR"); raw != "" {
		key, err := nodecred.DecodeSigningKey(raw)
		if err != nil {
			return nil, fmt.Errorf("BILLING_TREASURY_KEYPAIR is invalid: %w", err)
		}
		if billingTreasury == "" {
			return nil, fmt.Errorf("BILLING_TREASURY_KEYPAIR set but BILLING_TREASURY_WALLET is empty")
		}
		pub := key.Public().(ed25519.PublicKey)
		if got := solana.EncodeBase58(pub); got != billingTreasury {
			return nil, fmt.Errorf("BILLING_TREASURY_KEYPAIR does not match BILLING_TREASURY_WALLET (got %s, want %s)", got, billingTreasury)
		}
		treasuryKeypair = key
	}
	withdrawPollInterval, err := durationEnv("BILLING_WITHDRAW_POLL_INTERVAL", 5*time.Second)
	if err != nil {
		return nil, err
	}
	withdrawBlockhashMaxAge, err := durationEnv("BILLING_WITHDRAW_BLOCKHASH_MAX_AGE", 90*time.Second)
	if err != nil {
		return nil, err
	}

	// Delegations (Phase 2, §11). BILLING_SETTLEMENT_AUTHORITY must be a
	// valid base58 ed25519 pubkey when set — it names the delegate buyers
	// approve, and a typo here would silently read the wrong token accounts.
	var settlementAuthority string
	if raw := os.Getenv("BILLING_SETTLEMENT_AUTHORITY"); raw != "" {
		key, err := solana.DecodeBase58(raw, 64)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("BILLING_SETTLEMENT_AUTHORITY is not a valid base58 ed25519 pubkey: %q", raw)
		}
		settlementAuthority = raw
	}
	allowanceRefresh, err := durationEnv("BILLING_ALLOWANCE_REFRESH", 60*time.Second)
	if err != nil {
		return nil, err
	}
	if settlementAuthority != "" && allowanceRefresh <= 0 {
		return nil, fmt.Errorf("BILLING_ALLOWANCE_REFRESH must be positive, got %s", allowanceRefresh)
	}

	// Settlement worker keypair: OPTIONAL (delegation read-side can run
	// without the worker), but when present it MUST be the settlement
	// authority's key — it signs transfer_checked as the on-chain delegate.
	var settlementKeypair ed25519.PrivateKey
	if raw := os.Getenv("BILLING_SETTLEMENT_KEYPAIR"); raw != "" {
		key, err := nodecred.DecodeSigningKey(raw)
		if err != nil {
			return nil, fmt.Errorf("BILLING_SETTLEMENT_KEYPAIR is invalid: %w", err)
		}
		if settlementAuthority == "" {
			return nil, fmt.Errorf("BILLING_SETTLEMENT_KEYPAIR set but BILLING_SETTLEMENT_AUTHORITY is empty")
		}
		if got := solana.EncodeBase58(key.Public().(ed25519.PublicKey)); got != settlementAuthority {
			return nil, fmt.Errorf("BILLING_SETTLEMENT_KEYPAIR does not match BILLING_SETTLEMENT_AUTHORITY (got %s, want %s)", got, settlementAuthority)
		}
		settlementKeypair = key
	}
	settlementPoll, err := durationEnv("BILLING_SETTLEMENT_POLL_INTERVAL", 10*time.Second)
	if err != nil {
		return nil, err
	}
	if settlementKeypair != nil && settlementPoll <= 0 {
		return nil, fmt.Errorf("BILLING_SETTLEMENT_POLL_INTERVAL must be positive, got %s", settlementPoll)
	}

	// Listen address: LISTEN_ADDR is the static default (the Dockerfile sets
	// :8080, matching Fly's internal_port). Railway, by contrast, injects PORT
	// and routes the public domain — and the deploy health check — to it, so
	// honor PORT over the default whenever it is set. Fly never sets PORT, so
	// behavior there is unchanged.
	listenAddr := stringEnv("LISTEN_ADDR", ":8080")
	if port := os.Getenv("PORT"); port != "" {
		listenAddr = ":" + strings.TrimPrefix(port, ":")
	}

	return &Config{
		UpstreamURL:  upstream,
		DatabaseURL:  dbURL,
		ListenAddr:   listenAddr,
		CacheTTL:     cacheTTL,
		CacheNegTTL:  negTTL,
		JanitorEvery: janitor,

		BetterStackSourceToken: betterStackToken,
		BetterStackLogLevel:    betterStackLevel,
		LogFormat:              logFormat,

		NeonAuthJWKSURL:                jwksURL,
		NeonAuthIssuer:                 issuer,
		NeonAuthAudience:               os.Getenv("NEON_AUTH_AUDIENCE"),
		JWKSCacheTTL:                   jwksTTL,
		MaxKeysPerUser:                 maxKeys,
		CORSAllowedOrigins:             corsOrigins,
		KeyMgmtEnabled:                 jwksURL != "" && issuer != "",
		InternalControlToken:           internalControlToken,
		IdentityMaxAge:                 identityMaxAge,
		OwnershipMaxAge:                ownershipMaxAge,
		DecisionCacheTTL:               decisionTTL,
		InternalACLEnabled:             internalControlToken != "",
		EvaluatorRateLimitRPS:          evalRPS,
		EvaluatorRateLimitBurst:        evalBurst,
		NodeCredentialIssuer:           nodeCredentialIssuer,
		NodeCredentialSigningKID:       nodeCredentialSigningKID,
		NodeCredentialSigningKey:       nodeCredentialSigningKey,
		NodeCredentialVerifyKeys:       nodeCredentialVerifyKeys,
		NodeCredentialEnabled:          len(nodeCredentialVerifyKeys) > 0 && len(nodeCredentialSigningKey) == ed25519.PrivateKeySize,
		FaucetEnabled:                  faucetKey != nil && faucetMint != "" && faucetRPC != "",
		FaucetRPCURL:                   faucetRPC,
		FaucetMint:                     faucetMint,
		FaucetTokenProgram:             faucetTokenProgram,
		FaucetWalletKey:                faucetKey,
		FaucetAmountRaw:                faucetAmount,
		FaucetDecimals:                 faucetDecimals,
		TinybirdHost:                   tinybirdHost,
		TinybirdAppendToken:            tinybirdAppend,
		TinybirdLeaderboard:            tinybirdRead,
		ClickHouseURL:                  clickHouseURL,
		ClickHouseDatabase:             clickHouseDatabase,
		ClickHouseUsername:             clickHouseUsername,
		ClickHousePassword:             clickHousePassword,
		PerfFlushInterval:              perfFlushInterval,
		PerfBatchSize:                  perfBatchSize,
		PerfQueueSize:                  perfQueueSize,
		PerfPeerCacheTTL:               perfPeerCacheTTL,
		LeaderboardCacheTTL:            leaderboardCacheTTL,
		BillingMode:                    billingMode,
		BillingOutputMax:               billingOutputMax,
		BillingFeeBps:                  billingFeeBps,
		BillingSweepInterval:           billingSweepInterval,
		BillingSweepAge:                billingSweepAge,
		BillingRequirePricedPeer:       billingRequirePricedPeer,
		BillingTreasuryWallet:          billingTreasury,
		BillingSolanaRPC:               billingSolanaRPC,
		BillingDepositMint:             billingMint,
		BillingDepositTokenProgram:     billingTokenProgram,
		BillingDepositDecimals:         billingDecimals,
		BillingDepositPollInterval:     billingPollInterval,
		BillingTreasuryKeypair:         treasuryKeypair,
		BillingWithdrawPollInterval:    withdrawPollInterval,
		BillingWithdrawBlockhashMaxAge: withdrawBlockhashMaxAge,

		BillingSettlementAuthority: settlementAuthority,
		BillingAllowanceRefresh:    allowanceRefresh,

		BillingSettlementKeypair:      settlementKeypair,
		BillingSettlementPollInterval: settlementPoll,
	}, nil
}

func validCHIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func stringEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func boolEnv(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, v)
	}
	return b, nil
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
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, v)
	}
	return d, nil
}

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

func uint64Env(key string, def uint64) (uint64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, v)
	}
	return n, nil
}

func floatEnv(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("%s must be non-negative, got %q", key, v)
	}
	return f, nil
}

// levelEnv parses one of debug|info|warn|error (case-insensitive) into a
// slog.Level, returning def when the variable is unset. An unrecognized value
// is a hard error so a mistyped level is not silently swallowed.
func levelEnv(key string, def slog.Level) (slog.Level, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%s must be one of debug|info|warn|error, got %q", key, v)
	}
}
