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

	"github.com/opentela-ai/api/internal/aclapi"
	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/cache"
	"github.com/opentela-ai/api/internal/catalog"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/instancesapi"
	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/leaderboard"
	"github.com/opentela-ai/api/internal/manageapi"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/perf"
	"github.com/opentela-ai/api/internal/proxy"
	"github.com/opentela-ai/api/internal/regionsapi"
	"github.com/opentela-ai/api/internal/server"
	"github.com/opentela-ai/api/internal/store"
	"github.com/opentela-ai/api/internal/walletsapi"
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
	var internalACL http.Handler
	var internalACLv2 http.Handler
	var nodeChallenge http.Handler
	var nodeIssue http.Handler
	meshClient := mesh.New(cfg.UpstreamURL)
	if cfg.KeyMgmtEnabled {
		verifier := neonauth.New(cfg.NeonAuthJWKSURL, cfg.NeonAuthIssuer, cfg.NeonAuthAudience, cfg.JWKSCacheTTL)
		svc := keysvc.New(pg, cfg.MaxKeysPerUser)
		keyMgmt = manageapi.Router(svc, walletsapi.New(pg, cfg.IdentityMaxAge), instancesapi.New(pg, meshClient, cfg.IdentityMaxAge, cfg.OwnershipMaxAge), regionsapi.New(pg, meshClient, cfg.OwnershipMaxAge), verifier, pg, cfg.CORSAllowedOrigins)
		log.Printf("key management enabled at /manage/* (issuer %s)", cfg.NeonAuthIssuer)
	}
	var nodeVerifier *nodecred.Verifier
	if len(cfg.NodeCredentialVerifyKeys) > 0 {
		nodeVerifier = nodecred.NewVerifier(cfg.NodeCredentialIssuer, cfg.NodeCredentialVerifyKeys)
	}
	aclSvc := aclapi.NewWithNodeVerifier(pg, meshClient, cfg.InternalControlToken, nodeVerifier, cfg.IdentityMaxAge, cfg.OwnershipMaxAge, cfg.DecisionCacheTTL)
	if cfg.InternalACLEnabled {
		internalACL = aclSvc.HandlerV1()
		internalACLv2 = aclSvc.HandlerV2()
	}
	if cfg.NodeCredentialEnabled {
		signer := nodecred.NewSigner(cfg.NodeCredentialSigningKID, cfg.NodeCredentialIssuer, cfg.NodeCredentialSigningKey)
		nodeSvc := nodecred.NewService(pg, meshClient, signer, nodeVerifier, cfg.OwnershipMaxAge, cfg.InternalControlToken)
		nodeChallenge = nodeSvc.ChallengeHandler()
		nodeIssue = nodeSvc.IssueHandler()
	}
	// Public catalogue: distilled from the upstream node table, cached so a
	// keyless endpoint cannot be used to hammer the node.
	catalogHandler := catalog.NewWithPolicies(cfg.UpstreamURL, catalogCacheTTL, pg)

	// GPU performance pipeline (optional): sample every routed response into
	// ClickHouse and serve the anonymized aggregate at /v1/leaderboard.
	var perfHook func(*http.Response) error
	var leaderboardHandler http.Handler
	var sinkDone chan struct{}
	if cfg.ClickHouseURL != nil {
		sink := perf.NewClickHouseSink(cfg.ClickHouseURL, cfg.ClickHouseDatabase, cfg.ClickHouseUsername, cfg.ClickHousePassword, cfg.PerfFlushInterval, cfg.PerfBatchSize, cfg.PerfQueueSize)
		sinkDone = make(chan struct{})
		go func() {
			defer close(sinkDone)
			sink.Run(ctx)
		}()
		perfHook = perf.Hook(sink, perf.NewResolver(cfg.UpstreamURL, cfg.PerfPeerCacheTTL))
		leaderboardHandler = leaderboard.New(leaderboard.NewClickHouse(cfg.ClickHouseURL, cfg.ClickHouseDatabase, cfg.ClickHouseUsername, cfg.ClickHousePassword), cfg.LeaderboardCacheTTL)
		log.Printf("GPU performance pipeline enabled (ClickHouse %s, database %s)", cfg.ClickHouseURL.Host, cfg.ClickHouseDatabase)
	}
	handler := server.NewWithControlPlanes(validator, proxy.NewWithPerfHook(cfg.UpstreamURL, perfHook), keyMgmt, internalACL, internalACLv2, nodeChallenge, nodeIssue, catalogHandler, leaderboardHandler, cfg.CORSAllowedOrigins)

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
		err := srv.Shutdown(shutdownCtx)
		if sinkDone != nil {
			// Let the perf sink's bounded final flush complete before Postgres
			// closes and the process exits.
			select {
			case <-sinkDone:
			case <-time.After(12 * time.Second):
				log.Println("perf sink flush timed out on shutdown")
			}
		}
		return err
	}
}
