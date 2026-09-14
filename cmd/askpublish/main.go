// Command askpublish is the reference seller ask-publisher: it obtains a
// pricing-scoped node credential from the API and publishes (republishes)
// the seller's asks on a cadence shorter than the server-assigned TTL, so a
// live seller's prices never go stale.
//
//	askpublish \
//	  -api https://api.opentela.ai \
//	  -control-token "$INTERNAL_CONTROL_TOKEN" \
//	  -peer-key-file ~/.opentela/identity.key \
//	  -asks asks.json
//
// The asks file is a JSON document:
//
//	{"asks":[{"service":"llm","model":"llama3.1-70b","input_per_million":100,
//	          "cached_input_per_million":50,"output_per_million":200}]}
//
// Rows carry only service/model/rates: the server keys the replacement by
// the credential subject, derived here from the node's libp2p identity key.
// The peer key is a base64 libp2p-marshaled Ed25519 private key (the node's
// identity, not the Solana wallet).
//
// The server assigns each publication a 5-minute TTL; run with -interval
// 2m (the default) so asks never expire while the node is live. -once
// publishes a single cycle and exits (for cron/systemd timers).
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/opentela-ai/api/internal/askpublish"
	"github.com/opentela-ai/api/internal/billing"
)

// askTTL mirrors pricingapi's server-assigned expiry; the republish interval
// must stay comfortably below it.
const askTTL = 5 * time.Minute

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	api := flag.String("api", envOr("OPENTELA_API_URL", "https://api.opentela.ai"), "API base URL")
	controlToken := flag.String("control-token", os.Getenv("OPENTELA_CONTROL_TOKEN"), "INTERNAL_CONTROL_TOKEN for the challenge/issue endpoints")
	peerKeyRaw := flag.String("peer-key", os.Getenv("OPENTELA_PEER_KEY"), "base64 libp2p private key (the node identity)")
	peerKeyFile := flag.String("peer-key-file", "", "path to a base64 libp2p private key")
	asksPath := flag.String("asks", "", "path to the asks JSON file")
	interval := flag.Duration("interval", 2*time.Minute, "republish cadence (must be well under the 5m ask TTL)")
	once := flag.Bool("once", false, "publish one cycle and exit")
	flag.Parse()

	if *asksPath == "" {
		log.Fatal("-asks is required")
	}
	if *controlToken == "" {
		log.Fatal("-control-token (or OPENTELA_CONTROL_TOKEN) is required")
	}
	if *interval <= 0 {
		log.Fatal("-interval must be positive")
	}
	if *interval >= askTTL {
		log.Printf("warning: -interval %s is not below the %s ask TTL; asks will expire between publications", *interval, askTTL)
	}

	asks, err := loadAsks(*asksPath)
	if err != nil {
		log.Fatal(err)
	}
	if len(asks) == 0 {
		log.Print("asks file is empty: this CLEARS the peer's published prices")
	}

	key, err := loadPeerKey(*peerKeyRaw, *peerKeyFile)
	if err != nil {
		log.Fatal(err)
	}
	pub := key.GetPublic()
	peerID, err := peer.IDFromPublicKey(pub)
	if err != nil {
		log.Fatalf("derive peer id: %v", err)
	}
	log.Printf("publishing %d ask(s) for peer %s to %s", len(asks), peerID.String(), *api)

	client := &askpublish.Client{BaseURL: *api, ControlToken: *controlToken}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	publish := func() bool {
		out, err := client.Publish(ctx, key, asks)
		if err != nil {
			// A failed cycle is retried on the next tick: a transient API
			// outage or ownership hiccup must not take the seller offline
			// permanently — but note the ask TTL keeps running, so a seller
			// whose republisher fails for longer than the TTL drops out of
			// the priced market (see BILLING_REQUIRE_PRICED_PEER).
			log.Printf("publish failed (retry on next tick): %v", err)
			return false
		}
		log.Printf("published revision %d, expires %s", out.Revision, out.ExpiresAt.Format(time.RFC3339))
		return true
	}

	publish()
	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return
		case <-ticker.C:
			publish()
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadAsks(path string) ([]billing.Ask, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read asks file: %w", err)
	}
	return askpublish.LoadAsksFile(raw)
}

func loadPeerKey(raw, file string) (crypto.PrivKey, error) {
	if raw == "" && file == "" {
		return nil, errors.New("provide -peer-key or -peer-key-file (the node's libp2p identity key)")
	}
	if raw == "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read peer key file: %w", err)
		}
		raw = strings.TrimSpace(string(data))
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("peer key is not base64: %w", err)
		}
	}
	key, err := crypto.UnmarshalPrivateKey(decoded)
	if err != nil {
		return nil, fmt.Errorf("unmarshal libp2p private key: %w", err)
	}
	return key, nil
}
