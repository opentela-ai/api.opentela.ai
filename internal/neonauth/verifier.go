// Package neonauth verifies Neon Auth Ed25519 (EdDSA) JWTs against the project's
// JWKS using only the standard library. It never logs token contents.
package neonauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// leeway tolerates small clock skew on exp/nbf.
const leeway = 60 * time.Second

// Claims is the subset of verified JWT claims callers need.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
}

// Verifier validates compact JWTs signed with EdDSA (Ed25519).
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string
	cacheTTL time.Duration

	now    func() time.Time
	client *http.Client

	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey // kid -> public key
	fetchedAt time.Time

	minRefresh  time.Duration // min interval between unknown-kid-triggered refreshes
	lastRefresh time.Time     // time of the last refresh attempt (success or failure)
}

// New builds a Verifier. audience is enforced only when non-empty.
func New(jwksURL, issuer, audience string, cacheTTL time.Duration) *Verifier {
	return &Verifier{
		jwksURL:    jwksURL,
		issuer:     issuer,
		audience:   audience,
		cacheTTL:   cacheTTL,
		now:        time.Now,
		client:     &http.Client{Timeout: 10 * time.Second},
		minRefresh: time.Minute,
	}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// audienceClaim accepts either a string or an array of strings.
type audienceClaim []string

func (a *audienceClaim) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = []string{s}
		return nil
	}
	var ss []string
	if err := json.Unmarshal(b, &ss); err == nil {
		*a = ss
		return nil
	}
	return errors.New("aud: not a string or array")
}

type jwtClaims struct {
	Iss           string        `json:"iss"`
	Sub           string        `json:"sub"`
	Email         string        `json:"email"`
	EmailVerified bool          `json:"emailVerified"`
	Aud           audienceClaim `json:"aud"`
	Exp           int64         `json:"exp"`
	Nbf           int64         `json:"nbf"`
}

// Verify checks the signature and claims of raw, returning its Claims on success.
// Any structural, signature, or claim failure returns a non-nil error.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("neonauth: token is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: header: %w", err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return Claims{}, fmt.Errorf("neonauth: header json: %w", err)
	}
	if hdr.Alg != "EdDSA" {
		return Claims{}, fmt.Errorf("neonauth: unexpected alg %q", hdr.Alg)
	}

	pub, err := v.keyFor(ctx, hdr.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: signature: %w", err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, errors.New("neonauth: signature mismatch")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("neonauth: payload: %w", err)
	}
	var c jwtClaims
	if err := json.Unmarshal(payloadBytes, &c); err != nil {
		return Claims{}, fmt.Errorf("neonauth: payload json: %w", err)
	}
	if err := v.validateClaims(c); err != nil {
		return Claims{}, err
	}
	return Claims{
		Subject:       c.Sub,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
	}, nil
}

func (v *Verifier) validateClaims(c jwtClaims) error {
	now := v.now()
	if c.Exp == 0 {
		return errors.New("neonauth: missing exp")
	}
	if now.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return errors.New("neonauth: token expired")
	}
	if c.Nbf != 0 && now.Before(time.Unix(c.Nbf, 0).Add(-leeway)) {
		return errors.New("neonauth: token not yet valid")
	}
	if c.Iss != v.issuer {
		return fmt.Errorf("neonauth: unexpected issuer %q", c.Iss)
	}
	if c.Sub == "" {
		return errors.New("neonauth: missing sub")
	}
	if v.audience != "" && !contains(c.Aud, v.audience) {
		return errors.New("neonauth: audience mismatch")
	}
	return nil
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// keyFor returns the public key for kid, refreshing the JWKS if the cache is
// stale or the kid is unknown. Unknown-kid refreshes are rate-limited to at most
// one per minRefresh so a flood of random kids cannot force one fetch each; a
// genuinely rotated kid is absorbed at the next minRefresh boundary (or sooner
// once the cache passes cacheTTL).
func (v *Verifier) keyFor(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	_, have := v.keys[kid]
	stale := v.keys == nil || now.Sub(v.fetchedAt) > v.cacheTTL
	// Refresh when the cache is stale, or when the kid is unknown AND we have not
	// attempted a refresh within minRefresh — so a flood of tokens bearing random
	// kids cannot force one upstream fetch each.
	if stale || (!have && now.Sub(v.lastRefresh) >= v.minRefresh) {
		v.lastRefresh = now
		if err := v.refreshLocked(ctx); err != nil {
			// Serve a cached key if we still have one; otherwise fail.
			if k, ok := v.keys[kid]; ok {
				return k, nil
			}
			return nil, err
		}
	}
	k, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("neonauth: no key for kid %q", kid)
	}
	return k, nil
}

type jwksDoc struct {
	Keys []struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		X   string `json:"x"`
	} `json:"keys"`
}

const maxJWKSBytes = 1 << 20 // 1 MiB

func (v *Verifier) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("neonauth: jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("neonauth: jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("neonauth: jwks status %d", resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return fmt.Errorf("neonauth: jwks decode: %w", err)
	}
	keys := make(map[string]ed25519.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}
	if len(keys) == 0 {
		return errors.New("neonauth: jwks contained no usable Ed25519 keys")
	}
	v.keys = keys
	v.fetchedAt = v.now()
	return nil
}
