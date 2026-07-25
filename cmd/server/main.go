// Command server runs the opentela authenticating reverse proxy.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/catalog"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/keysapi"
	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/proxy"
	"github.com/opentela-ai/api/internal/server"
	"github.com/opentela-ai/api/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// catalogCacheTTL bounds how stale the public catalogue can be, and how often a
// flood of anonymous requests can reach the node.
const catalogCacheTTL = 15 * time.Second

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

	var keyMgmt http.Handler
	if cfg.KeyMgmtEnabled {
		verifier := neonauth.New(cfg.NeonAuthJWKSURL, cfg.NeonAuthIssuer, cfg.NeonAuthAudience, cfg.JWKSCacheTTL)
		svc := keysvc.New(pg, cfg.MaxKeysPerUser)
		keyMgmt = keysapi.Router(svc, verifier, cfg.CORSAllowedOrigins)
		log.Printf("key management enabled at /manage/keys (issuer %s)", cfg.NeonAuthIssuer)
	}
	// Public catalogue: distilled from the upstream node table, cached so a
	// keyless endpoint cannot be used to hammer the node.
	catalogHandler := catalog.New(cfg.UpstreamURL, catalogCacheTTL)
	handler := server.New(validator, proxy.New(cfg.UpstreamURL), keyMgmt, catalogHandler, cfg.CORSAllowedOrigins)

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
