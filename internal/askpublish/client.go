// Package askpublish is the reference seller ask-publisher client: it obtains
// a pricing-scoped node credential from the API's challenge/issue endpoints
// (signed with the node's libp2p identity key) and publishes the seller's
// asks to POST /internal/pricing.
//
// Asks carry a server-assigned TTL (5 minutes in pricingapi), so a live
// seller must republish on a shorter cadence — the cmd/askpublish command
// wraps this client in exactly that loop. This package exists so the
// credential flow (challenge → sign → issue → publish) is testable and
// reusable (e.g. from the CLI) without shelling out.
package askpublish

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/opentela-ai/api/internal/billing"
)

// Default paths on the API server. The challenge and issue endpoints share
// the internal control plane (Bearer INTERNAL_CONTROL_TOKEN); publication
// authenticates with the pricing-scoped credential the issue endpoint mints.
const (
	challengePath = "/internal/pricing/challenges"
	issuePath     = "/internal/pricing/issue"
	publishPath   = "/internal/pricing"
)

// Client publishes seller asks against one API deployment.
type Client struct {
	// BaseURL is the API origin, e.g. https://api.opentela.ai (no trailing slash).
	BaseURL string
	// ControlToken is the API's INTERNAL_CONTROL_TOKEN, required by the
	// challenge and issue endpoints.
	ControlToken string
	// HTTP is the HTTP client; nil means http.DefaultClient.
	HTTP *http.Client
}

// challengeResponse mirrors nodecred's PricingChallengeHandler response.
type challengeResponse struct {
	ChallengeID string `json:"challenge_id"`
	PeerID      string `json:"peer_id"`
	Audience    string `json:"audience"`
	Nonce       string `json:"nonce"`
	Message     string `json:"message"`
	ExpiresAt   string `json:"expires_at"`
}

// issueResponse mirrors nodecred's PricingIssueHandler response.
type issueResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	KID       string `json:"kid"`
}

// publishResponse mirrors pricingapi's askResponse.
type publishResponse struct {
	Revision  int64     `json:"revision"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Publish performs one full publication cycle for the given node key:
//
//  1. request a pricing-scoped challenge for the peer,
//  2. sign the challenge message with the node's libp2p identity key,
//  3. exchange the signed proof for a pricing credential,
//  4. POST the asks (a full replacement of the peer's asks).
//
// It returns the revision and expiry the server assigned. The peer_id field
// of each ask is ignored: the server keys the replacement by the credential
// subject, which this client derives from the node key.
func (c *Client) Publish(ctx context.Context, priv crypto.PrivKey, asks []billing.Ask) (publishResponse, error) {
	if priv == nil {
		return publishResponse{}, fmt.Errorf("askpublish: nil node key")
	}
	pub := priv.GetPublic()
	pubRaw, err := crypto.MarshalPublicKey(pub) // protobuf-marshaled: what UnmarshalPublicKey expects
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: marshal public key: %w", err)
	}
	peerID, err := peer.IDFromPublicKey(pub)
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: derive peer id: %w", err)
	}

	ch, err := c.postJSON(ctx, challengePath, c.ControlToken, map[string]string{"peer_id": peerID.String()})
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: challenge: %w", err)
	}
	var chResp challengeResponse
	if err := json.Unmarshal(ch, &chResp); err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: challenge response: %w", err)
	}
	if chResp.ChallengeID == "" || chResp.Nonce == "" || chResp.Message == "" {
		return publishResponse{}, fmt.Errorf("askpublish: incomplete challenge response")
	}

	sig, err := priv.Sign([]byte(chResp.Message))
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: sign challenge: %w", err)
	}
	isRespRaw, err := c.postJSON(ctx, issuePath, c.ControlToken, map[string]string{
		"challenge_id": chResp.ChallengeID,
		"peer_id":      peerID.String(),
		"nonce":        chResp.Nonce,
		"public_key":   base64.StdEncoding.EncodeToString(pubRaw),
		"signature":    base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: issue credential: %w", err)
	}
	var isResp issueResponse
	if err := json.Unmarshal(isRespRaw, &isResp); err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: issue response: %w", err)
	}
	if isResp.Token == "" {
		return publishResponse{}, fmt.Errorf("askpublish: issue response carried no token")
	}

	body := map[string]any{"asks": asks}
	pubRawResp, err := c.postJSON(ctx, publishPath, isResp.Token, body)
	if err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: publish asks: %w", err)
	}
	var out publishResponse
	if err := json.Unmarshal(pubRawResp, &out); err != nil {
		return publishResponse{}, fmt.Errorf("askpublish: publish response: %w", err)
	}
	return out, nil
}

// postJSON posts a JSON body with a Bearer token and returns the raw
// response body, turning non-2xx responses into descriptive errors.
func (c *Client) postJSON(ctx context.Context, path, token string, body any) ([]byte, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			// The most common operator error: a missing or wrong
			// INTERNAL_CONTROL_TOKEN on the challenge/issue calls.
			return nil, fmt.Errorf("%s: 401 unauthorized (control token rejected)", path)
		default:
			return nil, fmt.Errorf("%s: %d %s", path, resp.StatusCode, msg)
		}
	}
	return data, nil
}

// LoadAsksFile parses an asks file:
//
//	{"asks": [{"service":"llm","model":"m1","input_per_million":100, ...}]}
//
// Rows carry only service/model/rates — the server keys the replacement by
// the credential subject, so peer_id is ignored. The same checks the server
// applies (non-empty service/model, unique pairs, non-negative rates) are
// repeated here for fast local feedback.
func LoadAsksFile(raw []byte) ([]billing.Ask, error) {
	var file struct {
		Asks []billing.Ask `json:"asks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("askpublish: asks file: %w", err)
	}
	seen := make(map[string]bool, len(file.Asks))
	for i := range file.Asks {
		a := &file.Asks[i]
		a.PeerID = "" // keyed by credential subject server-side
		if a.Service == "" || a.Model == "" {
			return nil, fmt.Errorf("askpublish: asks[%d]: service and model are required", i)
		}
		key := a.Service + "\x00" + a.Model
		if seen[key] {
			return nil, fmt.Errorf("askpublish: asks[%d]: duplicate (%s, %s)", i, a.Service, a.Model)
		}
		seen[key] = true
		if a.InputPerMillion < 0 || a.CachedInputPerMillion < 0 || a.OutputPerMillion < 0 {
			return nil, fmt.Errorf("askpublish: asks[%d]: rates must be non-negative", i)
		}
		if a.InputPerMillion > billing.MaxBaseRate || a.CachedInputPerMillion > billing.MaxBaseRate || a.OutputPerMillion > billing.MaxBaseRate {
			return nil, fmt.Errorf("askpublish: asks[%d]: rate exceeds bound %d", i, billing.MaxBaseRate)
		}
	}
	return file.Asks, nil
}
