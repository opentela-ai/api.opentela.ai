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
	janitor, err := durationEnv("CACHE_JANITOR_INTERVAL", 1*time.Minute)
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
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, v)
	}
	return d, nil
}
