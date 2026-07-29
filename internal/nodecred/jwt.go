package nodecred

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const Audience = "api.opentela.ai/internal/acl"

type Claims struct {
	Subject            string
	Role               string
	Region             string
	MembershipRevision int64
	JTI                string
	IssuedAt           time.Time
	ExpiresAt          time.Time
}

type Signer struct {
	kid        string
	issuer     string
	privateKey ed25519.PrivateKey
}

func NewSigner(kid, issuer string, privateKey ed25519.PrivateKey) *Signer {
	return &Signer{kid: kid, issuer: issuer, privateKey: privateKey}
}

type Verifier struct {
	issuer string
	now    func() time.Time
	keys   map[string]ed25519.PublicKey
}

func NewVerifier(issuer string, keys map[string]ed25519.PublicKey) *Verifier {
	return &Verifier{issuer: issuer, now: time.Now, keys: keys}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iss                string `json:"iss"`
	Sub                string `json:"sub"`
	Aud                string `json:"aud"`
	Exp                int64  `json:"exp"`
	Iat                int64  `json:"iat"`
	Jti                string `json:"jti"`
	Role               string `json:"role"`
	Region             string `json:"region"`
	MembershipRevision int64  `json:"membership_revision"`
}

func (s *Signer) Sign(c Claims) (string, error) {
	headerBytes, err := json.Marshal(jwtHeader{Alg: "EdDSA", Kid: s.kid, Typ: "JWT"})
	if err != nil {
		return "", err
	}
	payloadBytes, err := json.Marshal(jwtClaims{
		Iss:                s.issuer,
		Sub:                c.Subject,
		Aud:                Audience,
		Exp:                c.ExpiresAt.UTC().Unix(),
		Iat:                c.IssuedAt.UTC().Unix(),
		Jti:                c.JTI,
		Role:               c.Role,
		Region:             c.Region,
		MembershipRevision: c.MembershipRevision,
	})
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString(headerBytes)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	signingInput := header + "." + payload
	sig := ed25519.Sign(s.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (v *Verifier) Verify(_ context.Context, raw string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("nodecred: token is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("nodecred: header decode: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, fmt.Errorf("nodecred: header json: %w", err)
	}
	if header.Alg != "EdDSA" {
		return Claims{}, errors.New("nodecred: unexpected alg")
	}
	pub, ok := v.keys[header.Kid]
	if !ok {
		return Claims{}, errors.New("nodecred: unknown kid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("nodecred: signature decode: %w", err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, errors.New("nodecred: signature mismatch")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("nodecred: payload decode: %w", err)
	}
	var payload jwtClaims
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return Claims{}, fmt.Errorf("nodecred: payload json: %w", err)
	}
	if payload.Iss != v.issuer || payload.Aud != Audience || payload.Sub == "" || !validNodeRole(payload.Role) || payload.Region == "" || payload.Jti == "" || payload.MembershipRevision <= 0 || payload.Iat == 0 {
		return Claims{}, errors.New("nodecred: invalid claims")
	}
	now := v.now().UTC()
	issuedAt := time.Unix(payload.Iat, 0).UTC()
	expiresAt := time.Unix(payload.Exp, 0).UTC()
	if payload.Exp == 0 || !now.Before(expiresAt) {
		return Claims{}, errors.New("nodecred: expired")
	}
	if issuedAt.After(now.Add(30*time.Second)) || !expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maxTokenTTL {
		return Claims{}, errors.New("nodecred: invalid lifetime")
	}
	return Claims{
		Subject:            payload.Sub,
		Role:               payload.Role,
		Region:             payload.Region,
		MembershipRevision: payload.MembershipRevision,
		JTI:                payload.Jti,
		IssuedAt:           issuedAt,
		ExpiresAt:          expiresAt,
	}, nil
}

func validNodeRole(role string) bool {
	switch role {
	case "head", "worker", "combined":
		return true
	default:
		return false
	}
}

func DecodeSigningKey(raw string) (ed25519.PrivateKey, error) {
	decoded, err := decodeBase64(raw)
	if err != nil {
		return nil, err
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	default:
		return nil, fmt.Errorf("nodecred: unexpected Ed25519 private key length %d", len(decoded))
	}
}

func DecodePublicKey(raw string) (ed25519.PublicKey, error) {
	decoded, err := decodeBase64(raw)
	if err != nil {
		return nil, err
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("nodecred: unexpected Ed25519 public key length %d", len(decoded))
	}
	return ed25519.PublicKey(decoded), nil
}

func decodeBase64(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("nodecred: empty key")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	return nil, errors.New("nodecred: invalid base64")
}
