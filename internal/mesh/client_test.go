package mesh

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/solana"
)

type signedIdentityPayload struct {
	PeerID       string `json:"peer_id"`
	WalletPubkey string `json:"wallet_pubkey"`
	Timestamp    int64  `json:"timestamp"`
}

func signedAttestation(t *testing.T, peerID string) (string, *solana.IdentityAttestation) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	wallet := solana.EncodeBase58(pub)
	payload, err := json.Marshal(signedIdentityPayload{
		PeerID:       peerID,
		WalletPubkey: wallet,
		Timestamp:    1_722_254_400,
	})
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	return wallet, &solana.IdentityAttestation{
		PeerID:       peerID,
		WalletPubkey: wallet,
		Timestamp:    1_722_254_400,
		Signature:    base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
	}
}

func newClient(t *testing.T, body any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dnt/table" {
			t.Fatalf("path=%s, want /v1/dnt/table", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode table: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	upstream, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("Parse URL: %v", err)
	}
	return New(upstream)
}

func TestLookupPeerRejectsEmptyOwnerEvenWithValidAttestation(t *testing.T) {
	_, att := signedAttestation(t, "peer-a")
	client := newClient(t, map[string]peerRecord{
		"peer-a": {Connected: true, Owner: "", IdentityAttestation: att},
	})

	_, err := client.LookupPeer(t.Context(), "peer-a")
	if err != ErrPeerUnverifiable {
		t.Fatalf("err=%v, want ErrPeerUnverifiable", err)
	}
}

func TestLookupPeerRejectsOwnerWalletMismatch(t *testing.T) {
	_, att := signedAttestation(t, "peer-a")
	client := newClient(t, map[string]peerRecord{
		"peer-a": {Connected: true, Owner: "11111111111111111111111111111111", IdentityAttestation: att},
	})

	_, err := client.LookupPeer(t.Context(), "peer-a")
	if err != ErrPeerUnverifiable {
		t.Fatalf("err=%v, want ErrPeerUnverifiable", err)
	}
}

func TestLookupPeerRejectsTamperedPeerID(t *testing.T) {
	wallet, att := signedAttestation(t, "peer-a")
	att.PeerID = "peer-b"
	client := newClient(t, map[string]peerRecord{
		"peer-a": {Connected: true, Owner: wallet, IdentityAttestation: att},
	})

	_, err := client.LookupPeer(t.Context(), "peer-a")
	if err != ErrPeerUnverifiable {
		t.Fatalf("err=%v, want ErrPeerUnverifiable", err)
	}
}

func TestLookupPeersFetchesTableOnceForBatch(t *testing.T) {
	walletA, attA := signedAttestation(t, "peer-a")
	walletB, attB := signedAttestation(t, "peer-b")
	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(map[string]peerRecord{
			"peer-a": {Connected: true, Owner: walletA, IdentityAttestation: attA},
			"peer-b": {Connected: true, Owner: walletB, IdentityAttestation: attB},
		})
	}))
	defer srv.Close()
	upstream, _ := url.Parse(srv.URL)
	client := New(upstream)
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return observedAt }

	obs, err := client.LookupPeers(t.Context(), []string{"peer-a", "peer-b"})
	if err != nil {
		t.Fatalf("LookupPeers: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches=%d, want 1", fetches)
	}
	if len(obs) != 2 {
		t.Fatalf("observations=%v, want 2 peers", obs)
	}
	if !obs["peer-a"].ObservedAt.Equal(observedAt) || !obs["peer-b"].ObservedAt.Equal(observedAt) {
		t.Fatalf("observations=%v, want verified observation time %v", obs, observedAt)
	}
}
