package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/opentela-ai/api/internal/solana"
)

var (
	ErrPeerUnavailable  = errors.New("mesh: peer unavailable")
	ErrPeerUnverifiable = errors.New("mesh: peer unverifiable")
)

type Client struct {
	upstream *url.URL
	client   *http.Client
	now      func() time.Time
}

type tableFetcher interface {
	fetchTable(ctx context.Context) (map[string]peerRecord, error)
}

type PeerObservation struct {
	PeerID     string
	Wallet     string
	ObservedAt time.Time
	Online     bool
}

type peerRecord struct {
	Connected           bool                        `json:"connected"`
	Owner               string                      `json:"owner"`
	IdentityAttestation *solana.IdentityAttestation `json:"identity_attestation"`
}

func New(upstream *url.URL) *Client {
	return &Client{
		upstream: upstream,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
	}
}

func (c *Client) LookupPeer(ctx context.Context, peerID string) (PeerObservation, error) {
	observations, err := c.LookupPeers(ctx, []string{peerID})
	if err != nil {
		return PeerObservation{}, err
	}
	obs, ok := observations[peerID]
	if !ok {
		return PeerObservation{}, ErrPeerUnavailable
	}
	return obs, nil
}

func (c *Client) LookupPeers(ctx context.Context, peerIDs []string) (map[string]PeerObservation, error) {
	table, err := c.fetchTable(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]PeerObservation, len(peerIDs))
	for _, peerID := range peerIDs {
		peer, ok := table[peerID]
		if !ok || !peer.Connected {
			continue
		}
		if peer.IdentityAttestation == nil {
			return nil, ErrPeerUnverifiable
		}
		if err := solana.VerifyIdentity(peer.IdentityAttestation); err != nil {
			return nil, ErrPeerUnverifiable
		}
		wallet, err := solana.NormalizeWallet(peer.IdentityAttestation.WalletPubkey)
		if err != nil {
			return nil, ErrPeerUnverifiable
		}
		if peer.Owner == "" || peer.Owner != wallet {
			return nil, ErrPeerUnverifiable
		}
		if peer.IdentityAttestation.PeerID != peerID {
			return nil, ErrPeerUnverifiable
		}
		out[peerID] = PeerObservation{
			PeerID: peerID,
			Wallet: wallet,
			// Ownership freshness is the age of this verified observation, not
			// the age of the node's startup attestation. OpenTela signs its
			// identity at startup, so using the signed timestamp here would make
			// healthy long-running peers unverifiable after OWNERSHIP_MAX_AGE.
			ObservedAt: c.now().UTC(),
			Online:     true,
		}
	}
	return out, nil
}

func (c *Client) OnlineStatus(ctx context.Context, peerIDs []string) (map[string]bool, error) {
	table, err := c.fetchTable(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(peerIDs))
	for _, id := range peerIDs {
		out[id] = table[id].Connected
	}
	return out, nil
}

func (c *Client) fetchTable(ctx context.Context) (map[string]peerRecord, error) {
	target := c.upstream.JoinPath("v1", "dnt", "table")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("mesh: request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mesh: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh: status %d", resp.StatusCode)
	}
	var table map[string]peerRecord
	if err := json.NewDecoder(resp.Body).Decode(&table); err != nil {
		return nil, fmt.Errorf("mesh: decode: %w", err)
	}
	return table, nil
}
