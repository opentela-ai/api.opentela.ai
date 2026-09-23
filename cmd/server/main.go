// Command server runs the opentela authenticating reverse proxy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os/signal"
	"syscall"
	"time"

	"github.com/opentela-ai/api/internal/aclapi"
	"github.com/opentela-ai/api/internal/askrefresh"
	"github.com/opentela-ai/api/internal/auth"
	"github.com/opentela-ai/api/internal/billingapi"
	"github.com/opentela-ai/api/internal/billinggate"
	"github.com/opentela-ai/api/internal/catalog"
	"github.com/opentela-ai/api/internal/config"
	"github.com/opentela-ai/api/internal/delegations"
	"github.com/opentela-ai/api/internal/deploykeys"
	"github.com/opentela-ai/api/internal/deposits"
	"github.com/opentela-ai/api/internal/faucet"
	"github.com/opentela-ai/api/internal/faucetapi"
	"github.com/opentela-ai/api/internal/instancesapi"
	"github.com/opentela-ai/api/internal/keysvc"
	"github.com/opentela-ai/api/internal/leaderboard"
	"github.com/opentela-ai/api/internal/manageapi"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/neonauth"
	"github.com/opentela-ai/api/internal/nodecred"
	"github.com/opentela-ai/api/internal/obs"
	"github.com/opentela-ai/api/internal/peers"
	"github.com/opentela-ai/api/internal/perf"
	"github.com/opentela-ai/api/internal/pricingapi"
	"github.com/opentela-ai/api/internal/proxy"
	"github.com/opentela-ai/api/internal/regionsapi"
	"github.com/opentela-ai/api/internal/server"
	"github.com/opentela-ai/api/internal/settlement"
	"github.com/opentela-ai/api/internal/solana"
	"github.com/opentela-ai/api/internal/solrpcproxy"
	"github.com/opentela-ai/api/internal/store"
	"github.com/opentela-ai/api/internal/walletsapi"
	"github.com/opentela-ai/api/internal/withdraw"
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
	// Hoisted to run()-scope: the §11.5.5 reconciler (wired in the
	// delegations section below) attaches to the same management Service
	// that the manage router mounts above.
	var billingRoutes *billingapi.Service

	// Observability is the first thing wired so every line below — and,
	// via the standard library's log bridge, the existing log.Printf call
	// sites across packages — reaches the configured sinks. slog.SetDefault
	// also clears log flags and routes the stdlib log package through this
	// logger, so legacy call sites emit INFO records on stdout and, when a
	// Better Stack token is set, there too. No call site needs to change.
	logger := obs.New(obs.Options{
		Token:  cfg.BetterStackSourceToken,
		Level:  cfg.BetterStackLogLevel,
		Format: cfg.LogFormat,
	})
	slog.SetDefault(logger)
	if cfg.BetterStackSourceToken != "" {
		log.Printf("better stack shipping enabled (level %s)", cfg.BetterStackLogLevel)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()

	c := auth.NewValidationCache(cfg.JanitorEvery)
	defer c.Close()

	validator := auth.NewValidator(pg, c, cfg.CacheTTL, cfg.CacheNegTTL)

	var keyMgmt http.Handler
	var internalACL http.Handler
	var internalACLv2 http.Handler
	var nodeChallenge http.Handler
	var nodeIssue http.Handler
	var nodePricingChallenge http.Handler
	var nodePricingIssue http.Handler
	var linkChallenge http.Handler
	var linkIssue http.Handler
	meshClient := mesh.New(cfg.UpstreamURL)
	if cfg.KeyMgmtEnabled {
		verifier := neonauth.New(cfg.NeonAuthJWKSURL, cfg.NeonAuthIssuer, cfg.NeonAuthAudience, cfg.JWKSCacheTTL)
		svc := keysvc.New(pg, cfg.MaxKeysPerUser, c)
		var faucetRoutes manageapi.FaucetRoutes
		if cfg.FaucetEnabled {
			faucetSvc, err := faucet.New(cfg.FaucetRPCURL, cfg.FaucetMint, cfg.FaucetTokenProgram, cfg.FaucetWalletKey, cfg.FaucetAmountRaw)
			if err != nil {
				return err
			}
			faucetRoutes = faucetapi.New(pg, faucetSvc, cfg.FaucetAmountRaw, cfg.FaucetDecimals)
			log.Printf("OTELA faucet enabled at /manage/faucet (mint %s, %d base units per claim)", cfg.FaucetMint, cfg.FaucetAmountRaw)
		}
		ws := walletsapi.New(pg, cfg.IdentityMaxAge)
		// Step 5: when billing is active, linking a wallet immediately credits
		// any deposits it received before linkage (the watcher recorded them
		// 'unassigned'). Billing-off → no reconciler; ReconcileDepositsForWallet
		// is a harmless no-op on an empty deposit_events table anyway, but we
		// keep the hook opt-in for clarity.
		if cfg.BillingMode != config.BillingOff {
			ws = ws.WithReconciler(depositReconciler{pg})
		}
		// The /manage/billing management endpoint is always mounted, even when
		// billing is off, so the wallet page can render a graceful "off" state
		// instead of a 404. The Service returns a zero-cost payload without
		// touching the billing tables in that mode. The treasury ATA, the
		// withdrawal flag, and — below — the billing gate, deposit watcher, and
		// withdrawal worker remain opt-in: only the management route is
		// unconditional.
		var treasuryATA string
		var withdrawalsEnabled bool
		if cfg.BillingMode != config.BillingOff {
			withdrawalsEnabled = cfg.BillingTreasuryKeypair != nil
			if cfg.BillingTreasuryWallet != "" {
				owner, err := solana.DecodeBase58(cfg.BillingTreasuryWallet, solana.PublicKeyBytes)
				if err != nil {
					return fmt.Errorf("BILLING_TREASURY_WALLET is not a valid base58 pubkey: %w", err)
				}
				mint, err := solana.DecodeBase58(cfg.BillingDepositMint, solana.PublicKeyBytes)
				if err != nil {
					return fmt.Errorf("BILLING_DEPOSIT_MINT is not a valid base58 pubkey: %w", err)
				}
				tp, err := solana.DecodeBase58(cfg.BillingDepositTokenProgram, solana.PublicKeyBytes)
				if err != nil {
					return fmt.Errorf("BILLING_DEPOSIT_TOKEN_PROGRAM is not a valid base58 pubkey: %w", err)
				}
				ata, err := solana.AssociatedTokenAddress(owner, mint, tp)
				if err != nil {
					return fmt.Errorf("derive treasury ATA: %w", err)
				}
				treasuryATA = solana.EncodeBase58(ata)
			}
		}
		billingRoutes = billingapi.New(pg, cfg.BillingMode, treasuryATA, cfg.BillingTreasuryWallet, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram, cfg.BillingDepositDecimals, withdrawalsEnabled, meshClient)
		log.Printf("OTELA billing management at /manage/billing (mode %s)", cfg.BillingMode)
		keyMgmt = manageapi.Router(svc, ws, instancesapi.New(pg, meshClient, cfg.IdentityMaxAge, cfg.OwnershipMaxAge), regionsapi.New(pg, meshClient, cfg.OwnershipMaxAge), faucetRoutes, billingRoutes, deploykeys.NewLinkService(pg, meshClient, cfg.OwnershipMaxAge, cfg.MaxDeployKeysPerUser), verifier, pg, cfg.CORSAllowedOrigins)
		linkSvc := deploykeys.NewLinkService(pg, meshClient, cfg.OwnershipMaxAge, cfg.MaxDeployKeysPerUser)
		linkChallenge = linkSvc.LinkChallengeHandler()
		linkIssue = linkSvc.LinkHandler()
		log.Printf("OTELA deploy-key management at /manage/deploy-keys; instance link at /internal/instances/link")
		log.Printf("key management enabled at /manage/* (issuer %s)", cfg.NeonAuthIssuer)
	}
	var nodeVerifier *nodecred.Verifier
	if len(cfg.NodeCredentialVerifyKeys) > 0 {
		nodeVerifier = nodecred.NewVerifier(cfg.NodeCredentialIssuer, cfg.NodeCredentialVerifyKeys)
	}
	aclSvc := aclapi.NewWithNodeVerifier(pg, meshClient, cfg.InternalControlToken, nodeVerifier, cfg.IdentityMaxAge, cfg.OwnershipMaxAge, cfg.DecisionCacheTTL)
	if cfg.EvaluatorRateLimitRPS > 0 {
		aclSvc = aclSvc.WithEvaluatorRateLimit(cfg.EvaluatorRateLimitRPS, cfg.EvaluatorRateLimitBurst)
	}
	if cfg.InternalACLEnabled {
		internalACL = aclSvc.HandlerV1()
		internalACLv2 = aclSvc.HandlerV2()
	}
	if cfg.NodeCredentialEnabled {
		signer := nodecred.NewSigner(cfg.NodeCredentialSigningKID, cfg.NodeCredentialIssuer, cfg.NodeCredentialSigningKey)
		nodeSvc := nodecred.NewService(pg, meshClient, signer, nodeVerifier, cfg.OwnershipMaxAge, cfg.InternalControlToken)
		nodeChallenge = nodeSvc.ChallengeHandler()
		nodeIssue = nodeSvc.IssueHandler()
		// A pricing-scoped signer (distinct audience) lets the same node
		// credential service mint seller-ask credentials. It is only mounted
		// when billing is active, below.
		if cfg.BillingMode != config.BillingOff {
			pricingSigner := nodecred.NewSignerWithAudience(cfg.NodeCredentialSigningKID, cfg.NodeCredentialIssuer, nodecred.PricingAudience, cfg.NodeCredentialSigningKey)
			pnode := nodeSvc.WithPricing(pricingSigner)
			nodePricingChallenge = pnode.PricingChallengeHandler()
			nodePricingIssue = pnode.PricingIssueHandler()
		}
	}
	// Public catalogue: distilled from the upstream node table, cached so a
	// keyless endpoint cannot be used to hammer the node. It shares the one
	// policy-filtered peers.Service with the billing gate (created below) so
	// the market view and the routing constraint observe the same providers.
	peerSnap := peers.New(cfg.UpstreamURL, catalogCacheTTL, pg)
	catalogHandler := catalog.NewWithSnapshot(peerSnap, catalogCacheTTL)
	// Step 6: when billing is active, the public catalogue also serves a
	// `market` array (price discovery) distilled from the same live asks the
	// billing gate quotes against, so buyers and sellers see one truth. With
	// billing off the response keeps its original services-only shape.
	if cfg.BillingMode != config.BillingOff {
		catalogHandler = catalogHandler.WithAsks(pg)
	}

	// GPU performance pipeline (optional): sample every routed response into
	// the configured analytics store (Tinybird Forward, or self-managed
	// ClickHouse) and serve the anonymized leaderboard — performance
	// percentiles plus the daily token-usage section — at /v1/leaderboard.
	var perfHook func(*http.Response) error
	var leaderboardHandler http.Handler
	var sink interface {
		perf.Recorder
		Run(context.Context)
	}
	switch {
	case cfg.TinybirdAppendToken != "":
		s := perf.NewTinybirdSink(cfg.TinybirdHost, cfg.TinybirdAppendToken, cfg.PerfFlushInterval, cfg.PerfBatchSize, cfg.PerfQueueSize)
		lq := leaderboard.NewTinybird(cfg.TinybirdHost, cfg.TinybirdLeaderboard)
		sink = s
		if cfg.TinybirdUsageToken != "" {
			leaderboardHandler = leaderboard.NewWithUsage(lq, leaderboard.NewUsageTinybird(cfg.TinybirdHost, cfg.TinybirdUsageToken), cfg.LeaderboardCacheTTL)
		} else {
			leaderboardHandler = leaderboard.New(lq, cfg.LeaderboardCacheTTL)
		}
		log.Printf("GPU performance pipeline enabled (Tinybird %s)", cfg.TinybirdHost.Host)
	case cfg.ClickHouseURL != nil:
		s := perf.NewClickHouseSink(cfg.ClickHouseURL, cfg.ClickHouseDatabase, cfg.ClickHouseUsername, cfg.ClickHousePassword, cfg.PerfFlushInterval, cfg.PerfBatchSize, cfg.PerfQueueSize)
		ch := leaderboard.NewClickHouse(cfg.ClickHouseURL, cfg.ClickHouseDatabase, cfg.ClickHouseUsername, cfg.ClickHousePassword)
		sink = s
		leaderboardHandler = leaderboard.NewWithUsage(ch, ch, cfg.LeaderboardCacheTTL)
		log.Printf("GPU performance pipeline enabled (ClickHouse %s, database %s)", cfg.ClickHouseURL.Host, cfg.ClickHouseDatabase)
	}
	var sweeperDone chan struct{}
	var askRefreshDone chan struct{}
	var depositDone chan struct{}
	var withdrawDone chan struct{}
	var delegDone chan struct{}
	var settleDone chan struct{}
	var sinkDone chan struct{}
	if sink != nil {
		sinkDone = make(chan struct{})
		go func() {
			defer close(sinkDone)
			sink.Run(ctx)
		}()
	}
	billingOpts := auth.Options{EnforceAccount: cfg.BillingMode == config.BillingEnforce}

	// The billing gate wraps the inference proxy when billing is active. It
	// resolves affordable live peers, reserves conservatively, and stamps
	// X-Otela-Allowed-Peers before forwarding. In off mode the gate is nil,
	// so the proxy is passed through unchanged.
	var billingGate http.Handler
	var pricingHandler http.Handler
	if cfg.BillingMode != config.BillingOff {
		gateSvc := billinggate.New(pg, peerSnap, cfg.BillingMode, cfg.BillingOutputMax, cfg.BillingFeeBps, cfg.BillingRequirePricedPeer)

		// Settlement finalizes each reserved request against the exact usage
		// parsed by the perf hook (no second body parse). The settler runs
		// inside the response hook, so it shares the same *measureBody parser
		// state that produces the perf sample.
		settler := settlement.New(pg, cfg.BillingFeeBps, time.Time{})
		resolve := perf.NewResolver(cfg.UpstreamURL, cfg.PerfPeerCacheTTL)
		switch {
		case sink != nil:
			perfHook = perf.HookWithSettle(sink, resolve, settler.Callback)
		default:
			perfHook = perf.HookWithSettle(perf.Null(), resolve, settler.Callback)
		}

		billingGate = gateSvc.Middleware(http.Handler(proxy.NewWithPerfHook(cfg.UpstreamURL, perfHook)))

		// Seller ask publication: POST /internal/pricing, authenticated by a
		// pricing-scoped node credential (distinct audience from ACL).
		pricingVerifier := nodecred.NewVerifierWithAudience(cfg.NodeCredentialIssuer, nodecred.PricingAudience, cfg.NodeCredentialVerifyKeys)
		pricingSvc := pricingapi.New(pg, meshClient, pricingVerifier, catalogAllowlist(catalogHandler), cfg.OwnershipMaxAge)
		pricingHandler = pricingSvc.Handler()

		// Recovery worker: reclaim reservations whose response hook never ran
		// (crashed proxy, lost connection). SKIP LOCKED keeps multiple replicas
		// safe; the age is well above the upstream request timeout.
		sweeperDone = make(chan struct{})
		go func() {
			defer close(sweeperDone)
			settlement.NewSweeper(pg, cfg.BillingSweepInterval, cfg.BillingSweepAge, 100).Run(ctx)
		}()

		// Ask refresher (§6): republishes console-managed seller asks
		// (peer_ask_config) into the TTL'd market while the live mesh
		// observation matches the owner — the console user's republish
		// cadence. Sellers who prefer file-based config run cmd/askpublish
		// instead; the two sources must not drive the same peer.
		askRefresh := askrefresh.New(pg, meshClient, func(format string, args ...any) {
			log.Printf("askrefresh: "+format, args...)
		})
		askRefreshDone := make(chan struct{})
		go func() {
			defer close(askRefreshDone)
			askRefresh.Run(ctx)
		}()

		// Deposit watcher (Step 5): poll the treasury associated token
		// account at finalized commitment, persist every inbound SPL transfer,
		// and credit the linked owner. Transfers from wallets that are not yet
		// linked are left 'unassigned' and reconciled automatically once the
		// wallet is linked (walletsapi.Service). The watcher reuses the faucet
		// mint/token-program when BILLING_DEPOSIT_MINT is unset, since devnet
		// deposits use the same network. Requires BILLING_TREASURY_WALLET.
		if cfg.BillingTreasuryWallet != "" {
			rpcClient := solana.NewRPCClient(cfg.BillingSolanaRPC, solana.WithRPCTimeout(20*time.Second))
			watcher, err := deposits.New(rpcClient, pg, pg, cfg.BillingTreasuryWallet, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram, cfg.BillingDepositPollInterval)
			if err != nil {
				return fmt.Errorf("deposit watcher: %w", err)
			}
			watcher.SetLogger(func(format string, args ...any) { log.Printf("deposits: "+format, args...) })
			depositDone = make(chan struct{})
			go func() {
				defer close(depositDone)
				watcher.Run(ctx)
			}()
			log.Printf("deposit watcher enabled (treasury %s, mint %s, poll %s)", cfg.BillingTreasuryWallet, cfg.BillingDepositMint, cfg.BillingDepositPollInterval)
		}

		// Withdrawal worker (Step 7): drain durable withdrawal records
		// (reserved -> signed -> broadcast -> finalized) into on-chain SPL
		// transfers signed by the treasury keypair. The worker starts only
		// when the keypair is configured AND verified to own
		// BILLING_TREASURY_WALLET (config.Load rejects a mismatch on boot),
		// so a deposit-only deployment with no keypair keeps the worker off.
		if cfg.BillingTreasuryKeypair != nil {
			withdrawRPC := solana.NewRPCClient(cfg.BillingSolanaRPC, solana.WithRPCTimeout(20*time.Second))
			worker, err := withdraw.New(withdrawRPC, pg, cfg.BillingTreasuryKeypair, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram, cfg.BillingWithdrawPollInterval, cfg.BillingWithdrawBlockhashMaxAge)
			if err != nil {
				return fmt.Errorf("withdrawal worker: %w", err)
			}
			worker.SetLogger(func(format string, args ...any) { log.Printf("withdraw: "+format, args...) })
			withdrawDone = make(chan struct{})
			go func() {
				defer close(withdrawDone)
				worker.Run(ctx)
			}()
			log.Printf("withdrawal worker enabled (treasury %s, mint %s, poll %s)", cfg.BillingTreasuryWallet, cfg.BillingDepositMint, cfg.BillingWithdrawPollInterval)
		}

		// Allowance poller (Phase 2, design §11.3): mirror SPL delegations to
		// BILLING_SETTLEMENT_AUTHORITY into the registry so the reserve gate's
		// min(credit, Σ allowance) bound tracks the chain. Runs only when the
		// authority is configured; unset means the custodial deposit rail
		// alone backs spend and the poller stays off.
		if cfg.BillingSettlementAuthority != "" {
			delegRPC := solana.NewRPCClient(cfg.BillingSolanaRPC, solana.WithRPCTimeout(20*time.Second))
			poller, err := delegations.New(delegRPC, pg, cfg.BillingSettlementAuthority, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram, cfg.BillingAllowanceRefresh)
			if err != nil {
				return fmt.Errorf("allowance poller: %w", err)
			}
			poller.SetLogger(func(format string, args ...any) { log.Printf("delegations: "+format, args...) })

			// Reconciler (§11.5.5): backs GET /manage/billing/reconciliation
			// and /manage/billing/merkle-proof — the ledger's Merkle
			// commitment, the registry-vs-chain cross-check, and residual
			// exposure reporting. On-demand (cron/console driven), no loop.
			reconciler, err := delegations.NewReconciler(pg, delegRPC, cfg.BillingSettlementAuthority, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram)
			if err != nil {
				return fmt.Errorf("billing reconciler: %w", err)
			}
			reconciler.SetLogger(func(format string, args ...any) { log.Printf("reconcile: "+format, args...) })
			billingRoutes.SetReconciler(reconciler)
			log.Printf("billing reconciliation enabled (authority %s)", cfg.BillingSettlementAuthority)

			delegDone = make(chan struct{})
			go func() {
				defer close(delegDone)
				poller.Run(ctx)
			}()
			log.Printf("allowance poller enabled (authority %s, mint %s, refresh %s)", cfg.BillingSettlementAuthority, cfg.BillingDepositMint, cfg.BillingAllowanceRefresh)

			// Settlement worker (Phase 2, §11.5): replay delegation-backed ledger
			// legs as batched transfer_checked transfers signed by the settlement
			// authority. Starts only when the keypair is configured AND verified
			// to be the authority (config.Load rejects a mismatch on boot).
			if cfg.BillingSettlementKeypair != nil {
				settleRPC := solana.NewRPCClient(cfg.BillingSolanaRPC, solana.WithRPCTimeout(20*time.Second))
				worker, err := delegations.NewSettlementWorker(settleRPC, pg, cfg.BillingSettlementKeypair, cfg.BillingSettlementAuthority, cfg.BillingDepositMint, cfg.BillingDepositTokenProgram, cfg.BillingTreasuryWallet, cfg.BillingDepositDecimals, cfg.BillingSettlementPollInterval, cfg.BillingWithdrawBlockhashMaxAge)
				if err != nil {
					return fmt.Errorf("settlement worker: %w", err)
				}
				worker.SetLogger(func(format string, args ...any) { log.Printf("settlement: "+format, args...) })
				settleDone = make(chan struct{})
				go func() {
					defer close(settleDone)
					worker.Run(ctx)
				}()
				log.Printf("settlement worker enabled (authority %s, mint %s, poll %s)", cfg.BillingSettlementAuthority, cfg.BillingDepositMint, cfg.BillingSettlementPollInterval)
			}
		}
	} else if sink != nil {
		perfHook = perf.Hook(sink, perf.NewResolver(cfg.UpstreamURL, cfg.PerfPeerCacheTTL))
	}

	proxyHandler := http.Handler(proxy.NewWithPerfHook(cfg.UpstreamURL, perfHook))
	if billingGate != nil {
		proxyHandler = billingGate
	}
	// Solana RPC proxy (browser plane): forwards allowed JSON-RPC for the
	// dApps — the public mainnet RPC rejects browser Origins. Upstream
	// defaults to the billing/faucet node; unset everything disables the
	// route entirely.
	var solRPCHandler http.Handler
	if cfg.SolRPCProxyUpstream != "" {
		upstream, err := url.Parse(cfg.SolRPCProxyUpstream)
		if err != nil {
			return fmt.Errorf("solana rpc proxy upstream: %w", err)
		}
		solRPCHandler = solrpcproxy.New(upstream, cfg.SolRPCProxyMethods,
			solrpcproxy.WithRateLimit(cfg.SolRPCProxyRateRPS, cfg.SolRPCProxyBurst),
			solrpcproxy.WithLogger(func(format string, args ...any) { log.Printf("solrpc: "+format, args...) }),
		)
	}
	handler := server.NewWithBillingOpts(validator, proxyHandler, keyMgmt, internalACL, internalACLv2, nodeChallenge, nodeIssue, catalogHandler, leaderboardHandler, cfg.CORSAllowedOrigins, billingOpts, billingGate, pricingHandler, nodePricingChallenge, nodePricingIssue, solRPCHandler, linkChallenge, linkIssue)
	if cfg.BillingMode != config.BillingOff {
		log.Printf("billing mode %s (output token max %d)", cfg.BillingMode, cfg.BillingOutputMax)
	}

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
		if askRefreshDone != nil {
			select {
			case <-askRefreshDone:
			case <-time.After(5 * time.Second):
			}
		}
		if sweeperDone != nil {
			select {
			case <-sweeperDone:
			case <-time.After(2 * time.Second):
			}
		}
		if depositDone != nil {
			select {
			case <-depositDone:
			case <-time.After(2 * time.Second):
				log.Println("deposit watcher did not stop within 2s on shutdown")
			}
		}
		if withdrawDone != nil {
			select {
			case <-withdrawDone:
			case <-time.After(2 * time.Second):
				log.Println("withdrawal worker did not stop within 2s on shutdown")
			}
		}
		return err
	}
}

// serviceCatalog is the subset of *catalog.Handler that catalogAllowlist
// reads. Defined as a local interface so the adapter (and its tests) need
// neither a live upstream mesh nor the catalog package's caching internals.
type serviceCatalog interface {
	ServicesForPricing(ctx context.Context) ([]catalog.Service, error)
}

// catalogAllowlist builds the seller-pricing allowlist from the shared catalog
// handler's distilled service list. It converts []catalog.Service →
// []pricingapi.AllowEntry so the pricing package has no catalog dependency.
func catalogAllowlist(c serviceCatalog) pricingapi.Allowlist {
	return pricingapi.CatalogAllowlist(func(ctx context.Context) ([]pricingapi.AllowEntry, error) {
		services, err := c.ServicesForPricing(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]pricingapi.AllowEntry, len(services))
		for i, svc := range services {
			out[i] = pricingapi.AllowEntry{Name: svc.Name, Models: svc.Models}
		}
		return out, nil
	})
}

var _ serviceCatalog = (*catalog.Handler)(nil)

// depositReconcilerStore is the subset of *store.Postgres used to reconcile
// deposits. Defined as a local interface so the adapter (and its tests) need
// no live Postgres.
type depositReconcilerStore interface {
	ReconcileDepositsForWallet(ctx context.Context, wallet, accountID string, now time.Time) (int, error)
}

// depositReconciler adapts depositReconcilerStore.ReconcileDepositsForWallet
// (which takes an explicit now for deterministic tests) to the
// walletsapi.Reconciler interface, supplying the current time.
type depositReconciler struct{ s depositReconcilerStore }

func (a depositReconciler) ReconcileDepositsForWallet(ctx context.Context, wallet, accountID string) (int, error) {
	return a.s.ReconcileDepositsForWallet(ctx, wallet, accountID, time.Now().UTC())
}

var _ depositReconcilerStore = (*store.Postgres)(nil)
