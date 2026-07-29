package solana

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizeWalletRoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	encoded := EncodeBase58(pub)
	got, err := NormalizeWallet(encoded)
	if err != nil {
		t.Fatalf("NormalizeWallet: %v", err)
	}
	if got != encoded {
		t.Fatalf("NormalizeWallet = %q, want %q", got, encoded)
	}
}

func TestVerifySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	message := BuildChallengeMessage("user-1", EncodeBase58(pub), "nonce", time.Unix(10, 0), time.Unix(20, 0))
	sig := EncodeBase58(ed25519.Sign(priv, []byte(message)))
	if err := VerifySignature(EncodeBase58(pub), message, sig); err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
}

func TestVerifyIdentity(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	wallet := EncodeBase58(pub)
	payload, err := json.Marshal(identityPayload{PeerID: "peer-1", WalletPubkey: wallet, Timestamp: 123})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	att := &IdentityAttestation{
		PeerID:       "peer-1",
		WalletPubkey: wallet,
		Timestamp:    123,
		Signature:    base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload)),
	}
	if err := VerifyIdentity(att); err != nil {
		t.Fatalf("VerifyIdentity: %v", err)
	}
	att.PeerID = "peer-2"
	if err := VerifyIdentity(att); err == nil {
		t.Fatal("VerifyIdentity(tampered) unexpectedly succeeded")
	}
}
