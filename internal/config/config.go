// Package config loads and validates service configuration from the environment.
package config

import (
	"crypto/ed25519"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opentela-ai/api/internal/nodecred"
)

// Config holds all runtime configuration for the proxy service.
type Config struct {
	UpstreamURL  *url.URL
	DatabaseURL  string
	ListenAddr   string
	CacheTTL     time.Duration
	CacheNegTTL  time.Duration
	JanitorEvery time.Duration

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

	NodeCredentialIssuer     string
	NodeCredentialSigningKID string
	NodeCredentialSigningKey ed25519.PrivateKey
	NodeCredentialVerifyKeys map[string]ed25519.PublicKey
	NodeCredentialEnabled    bool
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
	janitor, err := durationEnv("CACHE_JANITOR_INTERVAL", 1*time.Minute)
	if err != nil {
		return nil, err
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

	return &Config{
		UpstreamURL:  upstream,
		DatabaseURL:  dbURL,
		ListenAddr:   stringEnv("LISTEN_ADDR", ":8080"),
		CacheTTL:     cacheTTL,
		CacheNegTTL:  negTTL,
		JanitorEvery: janitor,

		NeonAuthJWKSURL:          jwksURL,
		NeonAuthIssuer:           issuer,
		NeonAuthAudience:         os.Getenv("NEON_AUTH_AUDIENCE"),
		JWKSCacheTTL:             jwksTTL,
		MaxKeysPerUser:           maxKeys,
		CORSAllowedOrigins:       corsOrigins,
		KeyMgmtEnabled:           jwksURL != "" && issuer != "",
		InternalControlToken:     internalControlToken,
		IdentityMaxAge:           identityMaxAge,
		OwnershipMaxAge:          ownershipMaxAge,
		DecisionCacheTTL:         decisionTTL,
		InternalACLEnabled:       internalControlToken != "",
		NodeCredentialIssuer:     nodeCredentialIssuer,
		NodeCredentialSigningKID: nodeCredentialSigningKID,
		NodeCredentialSigningKey: nodeCredentialSigningKey,
		NodeCredentialVerifyKeys: nodeCredentialVerifyKeys,
		NodeCredentialEnabled:    len(nodeCredentialVerifyKeys) > 0 && len(nodeCredentialSigningKey) == ed25519.PrivateKeySize,
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
