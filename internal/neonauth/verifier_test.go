package neonauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testKID = "test-kid-1"

// jwksServer serves a JWKS containing pub under testKID.
func jwksServer(t *testing.T, pub ed25519.PublicKey) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "kid": testKID,
		"x": base64.RawURLEncoding.EncodeToString(pub),
	}}})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
}

// jwksServerCounting serves the JWKS for pub under testKID and counts requests.
func jwksServerCounting(t *testing.T, pub ed25519.PublicKey, count *int64) *httptest.Server {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "kid": testKID,
		"x": base64.RawURLEncoding.EncodeToString(pub),
	}}})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(count, 1)
		_, _ = w.Write(body)
	}))
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// signJWT builds a signed compact JWT from header + claims maps.
func signJWT(priv ed25519.PrivateKey, header, claims map[string]any) string {
	signingInput := b64(header) + "." + b64(claims)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newVerifier(t *testing.T, srv *httptest.Server, aud string) *Verifier {
	t.Helper()
	v := New(srv.URL, "https://auth.example", aud, time.Hour)
	fixed := time.Unix(1_700_000_000, 0)
	v.now = func() time.Time { return fixed }
	return v
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": "https://auth.example",
		"sub": "user-alice",
		"exp": 1_700_000_000 + 3600,
		"iat": 1_700_000_000 - 10,
	}
}

func TestVerifyValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()
	v := newVerifier(t, srv, "")
	claims := validClaims()
	claims["email"] = "alice@example.com"
	claims["emailVerified"] = true
	tok := signJWT(priv, map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}, claims)

	c, err := v.Verify(context.Background(), tok)
	if err != nil || c.Subject != "user-alice" {
		t.Fatalf("Verify = (%+v, %v), want subject user-alice, nil", c, err)
	}
	if c.Email != "alice@example.com" || !c.EmailVerified {
		t.Fatalf("Verify claims = %+v, want email claims populated", c)
	}
}

func TestVerifyRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()

	hdr := map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}

	cases := map[string]string{
		"alg-none":    signJWT(priv, map[string]any{"alg": "none", "kid": testKID}, validClaims()),
		"wrong-key":   signJWT(otherPriv, hdr, validClaims()),
		"unknown-kid": signJWT(priv, map[string]any{"alg": "EdDSA", "kid": "nope"}, validClaims()),
		"not-three":   "a.b",
		"expired":     signJWT(priv, hdr, mutate(validClaims(), "exp", 1_700_000_000-3600)),
		"future-nbf":  signJWT(priv, hdr, mutate(validClaims(), "nbf", 1_700_000_000+3600)),
		"wrong-iss":   signJWT(priv, hdr, mutate(validClaims(), "iss", "https://evil")),
		"missing-sub": signJWT(priv, hdr, mutate(validClaims(), "sub", "")),
		"missing-exp": signJWT(priv, hdr, without(validClaims(), "exp")),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			v := newVerifier(t, srv, "")
			if _, err := v.Verify(context.Background(), tok); err == nil {
				t.Fatalf("Verify(%s) = nil error, want rejection", name)
			}
		})
	}
}

func TestVerifyAudience(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := jwksServer(t, pub)
	defer srv.Close()
	hdr := map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}

	// Audience required but wrong → reject.
	v := newVerifier(t, srv, "my-api")
	if _, err := v.Verify(context.Background(), signJWT(priv, hdr, mutate(validClaims(), "aud", "other"))); err == nil {
		t.Fatal("Verify with wrong aud: want rejection")
	}
	// Audience required and correct → accept.
	if _, err := v.Verify(context.Background(), signJWT(priv, hdr, mutate(validClaims(), "aud", "my-api"))); err != nil {
		t.Fatalf("Verify with correct aud: %v", err)
	}
}

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var fetches int64
	srv := jwksServerCounting(t, pub, &fetches)
	defer srv.Close()
	v := newVerifier(t, srv, "") // fixed now via the test helper
	v.minRefresh = time.Minute

	// One valid call populates the cache (fetch #1).
	good := signJWT(priv, map[string]any{"alg": "EdDSA", "kid": testKID, "typ": "JWT"}, validClaims())
	if _, err := v.Verify(context.Background(), good); err != nil {
		t.Fatalf("valid Verify: %v", err)
	}
	// A flood of unknown-kid tokens (same fixed now) must NOT trigger more fetches.
	for i := 0; i < 5; i++ {
		bad := signJWT(priv, map[string]any{"alg": "EdDSA", "kid": "random-kid", "typ": "JWT"}, validClaims())
		if _, err := v.Verify(context.Background(), bad); err == nil {
			t.Fatal("Verify with unknown kid: want rejection")
		}
	}
	if n := atomic.LoadInt64(&fetches); n != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (unknown-kid refreshes must be rate-limited)", n)
	}
}

func mutate(m map[string]any, k string, val any) map[string]any {
	out := map[string]any{}
	for kk, vv := range m {
		out[kk] = vv
	}
	if s, ok := val.(string); ok && s == "" {
		delete(out, k)
	} else {
		out[k] = val
	}
	return out
}

func without(m map[string]any, k string) map[string]any { return mutate(m, k, "") }
