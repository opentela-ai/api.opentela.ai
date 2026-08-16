package nodecred

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignerAndVerifierRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer := NewSigner("kid-current", "api.opentela.ai", priv)
	verifier := NewVerifier("api.opentela.ai", map[string]ed25519.PublicKey{"kid-current": pub})
	verifier.now = func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }

	token, err := signer.Sign(Claims{
		Subject:            "12D3KooWpeer",
		Role:               "head",
		Region:             "research-eu",
		MembershipRevision: 7,
		JTI:                "token-1",
		IssuedAt:           time.Date(2026, 7, 29, 11, 55, 0, 0, time.UTC),
		ExpiresAt:          time.Date(2026, 7, 29, 12, 10, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "12D3KooWpeer" || claims.Role != "head" || claims.Region != "research-eu" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestVerifierRejectsAudienceMismatch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	headerBytes, _ := json.Marshal(jwtHeader{Alg: "EdDSA", Kid: "kid-current", Typ: "JWT"})
	payloadBytes, _ := json.Marshal(jwtClaims{
		Iss: "api.opentela.ai", Sub: "peer-a", Aud: "wrong-audience", Exp: time.Date(2026, 7, 29, 12, 10, 0, 0, time.UTC).Unix(),
		Iat: time.Date(2026, 7, 29, 11, 55, 0, 0, time.UTC).Unix(), Jti: "token-2", Role: "head", Region: "research-eu",
	})
	header := base64.RawURLEncoding.EncodeToString(headerBytes)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := header + "." + payload
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signingInput)))

	verifier := NewVerifier("api.opentela.ai", map[string]ed25519.PublicKey{"kid-current": pub})
	verifier.now = func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
	if _, err := verifier.Verify(context.Background(), token); err == nil || !strings.Contains(err.Error(), "invalid claims") {
		t.Fatalf("err=%v, want invalid claims", err)
	}
}

func TestVerifierRejectsInvalidTemporalAndRoleClaims(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer := NewSigner("kid-current", "api.opentela.ai", priv)
	verifier := NewVerifier("api.opentela.ai", map[string]ed25519.PublicKey{"kid-current": pub})
	verifier.now = func() time.Time { return now }

	tests := []struct {
		name   string
		claims Claims
		want   string
	}{
		{
			name: "expired",
			claims: Claims{Subject: "peer-a", Role: "worker", Region: "research-eu", MembershipRevision: 1, JTI: "expired",
				IssuedAt: now.Add(-15 * time.Minute), ExpiresAt: now},
			want: "expired",
		},
		{
			name: "wrong role",
			claims: Claims{Subject: "peer-a", Role: "relay", Region: "research-eu", MembershipRevision: 1, JTI: "role",
				IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
			want: "invalid claims",
		},
		{
			name: "future issued at",
			claims: Claims{Subject: "peer-a", Role: "worker", Region: "research-eu", MembershipRevision: 1, JTI: "future",
				IssuedAt: now.Add(time.Minute), ExpiresAt: now.Add(2 * time.Minute)},
			want: "invalid lifetime",
		},
		{
			name: "overlong lifetime",
			claims: Claims{Subject: "peer-a", Role: "worker", Region: "research-eu", MembershipRevision: 1, JTI: "overlong",
				IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(maxTokenTTL)},
			want: "invalid lifetime",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := signer.Sign(tt.claims)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if _, err := verifier.Verify(context.Background(), token); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeSigningKeyAcceptsSeed(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(priv.Seed())
	decoded, err := DecodeSigningKey(raw)
	if err != nil {
		t.Fatalf("DecodeSigningKey: %v", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		t.Fatalf("private key size=%d", len(decoded))
	}
}

func TestPricingAudienceIsolation(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keys := map[string]ed25519.PublicKey{"kid-p": pub}
	// A pricing-scoped signer stamps the pricing audience.
	pSigner := NewSignerWithAudience("kid-p", "api.opentela.ai", PricingAudience, priv)
	aclVerifier := NewVerifier("api.opentela.ai", keys)
	pricingVerifier := NewVerifierWithAudience("api.opentela.ai", PricingAudience, keys)
	aclVerifier.now = func() time.Time { return now }
	pricingVerifier.now = func() time.Time { return now }

	tok, err := pSigner.Sign(Claims{
		Subject: "peer-seller", Role: "worker", Region: "research-eu",
		MembershipRevision: 1, JTI: "p1",
		IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// The pricing verifier accepts it and reports the pricing audience.
	got, err := pricingVerifier.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("pricing Verify: %v", err)
	}
	if got.Audience != PricingAudience {
		t.Fatalf("Audience = %q, want %q", got.Audience, PricingAudience)
	}

	// The ACL verifier rejects it: a pricing credential must not authorize
	// ACL access, and an ACL credential must not authorize pricing.
	if _, err := aclVerifier.Verify(context.Background(), tok); err == nil {
		t.Fatal("ACL verifier accepted a pricing-scoped token")
	}

	// The mirror case: an ACL-scoped token is rejected by the pricing verifier.
	aclSigner := NewSigner("kid-a", "api.opentela.ai", priv)
	aclTok, err := aclSigner.Sign(Claims{
		Subject: "peer-seller", Role: "worker", Region: "research-eu",
		MembershipRevision: 1, JTI: "a1",
		IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ACL Sign: %v", err)
	}
	if _, err := pricingVerifier.Verify(context.Background(), aclTok); err == nil {
		t.Fatal("pricing verifier accepted an ACL-scoped token")
	}
}
