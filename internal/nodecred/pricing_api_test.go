package nodecred

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	crypto "github.com/libp2p/go-libp2p/core/crypto"
	peer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/opentela-ai/api/internal/mesh"
	"github.com/opentela-ai/api/internal/store"
)

func pricingSVC(t *testing.T, inst store.InstanceInfo, obs mesh.PeerObservation) *Service {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	svc := NewService(challengeStoreStub{instance: inst}, challengeMeshStub{observation: obs}, nil, nil, time.Minute, "internal-secret")
	svc.now = func() time.Time { return obs.ObservedAt }
	return svc.WithPricing(NewSignerWithAudience("kid-p", "api.opentela.ai", PricingAudience, priv))
}

func TestPricingChallengeForBillableProvider(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc := pricingSVC(t,
		store.InstanceInfo{PeerID: "peer-seller", AccountID: "acct", OwnerWallet: "wallet-owner"},
		mesh.PeerObservation{PeerID: "peer-seller", Wallet: "wallet-owner", ObservedAt: now})

	req := httptest.NewRequest(http.MethodPost, "/internal/pricing/challenges", strings.NewReader(`{"peer_id":"peer-seller"}`))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.PricingChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp challengeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Audience != PricingAudience || resp.PeerID != "peer-seller" || resp.Nonce == "" || resp.Message == "" {
		t.Fatalf("response=%+v", resp)
	}
	// The signed message embeds the pricing audience, not the ACL one.
	if !strings.Contains(resp.Message, "audience="+PricingAudience) {
		t.Fatalf("message not pricing-scoped: %q", resp.Message)
	}
}

func TestPricingChallengeRejectsNonBillable(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for _, inst := range []store.InstanceInfo{
		{PeerID: "p"},                    // no account, no wallet
		{PeerID: "p", AccountID: "acct"}, // account but no wallet
		{PeerID: "p", OwnerWallet: "w"},  // wallet but no account
	} {
		svc := pricingSVC(t, inst, mesh.PeerObservation{PeerID: "p", Wallet: inst.OwnerWallet, ObservedAt: now})
		req := httptest.NewRequest(http.MethodPost, "/internal/pricing/challenges", strings.NewReader(`{"peer_id":"p"}`))
		req.Header.Set("Authorization", "Bearer internal-secret")
		rec := httptest.NewRecorder()
		svc.PricingChallengeHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("inst=%+v: code=%d, want 409", inst, rec.Code)
		}
	}
}

func TestPricingChallengeRejectsWalletMismatch(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc := pricingSVC(t,
		store.InstanceInfo{PeerID: "p", AccountID: "acct", OwnerWallet: "wallet-real"},
		mesh.PeerObservation{PeerID: "p", Wallet: "wallet-other", ObservedAt: now})
	req := httptest.NewRequest(http.MethodPost, "/internal/pricing/challenges", strings.NewReader(`{"peer_id":"p"}`))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.PricingChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 (wallet mismatch)", rec.Code)
	}
}

func TestPricingChallengeWithoutSigner(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	svc := NewService(challengeStoreStub{instance: store.InstanceInfo{PeerID: "p", AccountID: "a", OwnerWallet: "w"}},
		challengeMeshStub{observation: mesh.PeerObservation{PeerID: "p", Wallet: "w", ObservedAt: now}}, nil, nil, time.Minute, "internal-secret")
	svc.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/internal/pricing/challenges", strings.NewReader(`{"peer_id":"p"}`))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.PricingChallengeHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503 (no pricing signer)", rec.Code)
	}
}

// TestPricingIssueRoundTrip mints a pricing credential for a billable,
// permissionless provider (no membership) and verifies the issued token is
// accepted only by a pricing-scoped verifier.
func TestPricingIssueRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	// Peer keypair (libp2p), so decodePeerProof's UnmarshalPublicKey accepts it.
	peerPriv, _, err := crypto.GenerateEd25519Key(crand.Reader)
	if err != nil {
		t.Fatalf("peer genkey: %v", err)
	}
	peerPub := peerPriv.GetPublic()
	peerID, err := peer.IDFromPublicKey(peerPub)
	if err != nil {
		t.Fatalf("peer id: %v", err)
	}
	pubBytes, err := crypto.MarshalPublicKey(peerPub)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pubBytes)

	inst := store.InstanceInfo{PeerID: peerID.String(), AccountID: "acct", OwnerWallet: "wallet-owner"}
	nonce := "nonce-roundtrip"
	challengeID := "chid-roundtrip"
	expiresAt := now.Add(challengeTTL)
	message := canonicalChallengeMessage(PricingAudience, challengeID, peerID.String(), "", "", nonce, now, expiresAt)
	sig, err := peerPriv.Sign([]byte(message))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	st := challengeStoreStub{
		instance: inst,
		consume: store.NodeCredentialChallenge{
			ID: challengeID, PeerID: peerID.String(), Audience: PricingAudience,
			NonceHash: store.HashKey(nonce), ChallengeMessage: message, IssuedAt: now, ExpiresAt: expiresAt,
		},
	}
	mx := challengeMeshStub{observation: mesh.PeerObservation{PeerID: peerID.String(), Wallet: "wallet-owner", ObservedAt: now}}

	svcPub, svcPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("svc genkey: %v", err)
	}
	svc := NewService(st, mx, nil, nil, time.Minute, "internal-secret")
	svc.now = func() time.Time { return now }
	svc = svc.WithPricing(NewSignerWithAudience("kid-p", "api.opentela.ai", PricingAudience, svcPriv))

	body := fmt.Sprintf(`{"challenge_id":%q,"peer_id":%q,"nonce":%q,"public_key":%q,"signature":%q}`,
		challengeID, peerID.String(), nonce, pubB64, base64.RawURLEncoding.EncodeToString(sig))
	req := httptest.NewRequest(http.MethodPost, "/internal/pricing/issue", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.PricingIssueHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp issueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The pricing verifier (no membership required) accepts it.
	pricingV := NewVerifierWithAudience("api.opentela.ai", PricingAudience, map[string]ed25519.PublicKey{"kid-p": svcPub})
	pricingV.now = func() time.Time { return now }
	claims, err := pricingV.Verify(context.Background(), resp.Token)
	if err != nil {
		t.Fatalf("pricing Verify: %v", err)
	}
	if claims.Subject != peerID.String() || claims.Audience != PricingAudience {
		t.Fatalf("claims=%+v", claims)
	}
	// The ACL verifier (membership required) must reject the pricing token.
	aclV := NewVerifier("api.opentela.ai", map[string]ed25519.PublicKey{"kid-p": svcPub})
	aclV.now = func() time.Time { return now }
	if _, err := aclV.Verify(context.Background(), resp.Token); err == nil {
		t.Fatal("ACL verifier accepted a pricing-scoped token")
	}
}

// TestPricingIssueRejectsACLAudienceChallenge proves a challenge created for
// the ACL audience cannot be redeemed for a pricing credential.
func TestPricingIssueRejectsACLAudienceChallenge(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	peerPriv, _, _ := crypto.GenerateEd25519Key(crand.Reader)
	peerPub := peerPriv.GetPublic()
	peerID, _ := peer.IDFromPublicKey(peerPub)
	st := challengeStoreStub{
		instance: store.InstanceInfo{PeerID: peerID.String(), AccountID: "acct", OwnerWallet: "w"},
		consume: store.NodeCredentialChallenge{
			ID: "chid-acl", PeerID: peerID.String(), Audience: Audience, // ACL audience, not pricing
			NonceHash: store.HashKey("n"), ChallengeMessage: "msg", IssuedAt: now, ExpiresAt: now.Add(challengeTTL),
		},
	}
	mx := challengeMeshStub{observation: mesh.PeerObservation{PeerID: peerID.String(), Wallet: "w", ObservedAt: now}}
	_, svcPriv, _ := ed25519.GenerateKey(crand.Reader)
	svc := NewService(st, mx, nil, nil, time.Minute, "internal-secret")
	svc.now = func() time.Time { return now }
	svc = svc.WithPricing(NewSignerWithAudience("kid-p", "api.opentela.ai", PricingAudience, svcPriv))

	body := fmt.Sprintf(`{"challenge_id":"chid-acl","peer_id":%q,"nonce":"n","public_key":"","signature":""}`, peerID.String())
	req := httptest.NewRequest(http.MethodPost, "/internal/pricing/issue", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer internal-secret")
	rec := httptest.NewRecorder()
	svc.PricingIssueHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404 (ACL challenge must not yield a pricing token)", rec.Code)
	}
}
