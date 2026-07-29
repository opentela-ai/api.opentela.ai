package solana

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	PublicKeyBytes = ed25519.PublicKeySize
	SignatureBytes = ed25519.SignatureSize
)

type identityPayload struct {
	PeerID       string `json:"peer_id"`
	WalletPubkey string `json:"wallet_pubkey"`
	Timestamp    int64  `json:"timestamp"`
}

// IdentityAttestation matches OpenTela's signed identity payload.
type IdentityAttestation struct {
	PeerID       string `json:"peer_id"`
	WalletPubkey string `json:"wallet_pubkey"`
	Timestamp    int64  `json:"timestamp"`
	Signature    string `json:"signature"`
}

func NormalizeWallet(v string) (string, error) {
	decoded, err := DecodeBase58(v, PublicKeyBytes)
	if err != nil {
		return "", err
	}
	if len(decoded) != PublicKeyBytes {
		return "", fmt.Errorf("wallet public key must decode to %d bytes", PublicKeyBytes)
	}
	return EncodeBase58(decoded), nil
}

func DecodeSignature(v string) ([]byte, error) {
	decoded, err := DecodeBase58(v, SignatureBytes)
	if err != nil {
		return nil, err
	}
	if len(decoded) != SignatureBytes {
		return nil, fmt.Errorf("signature must decode to %d bytes", SignatureBytes)
	}
	return decoded, nil
}

func VerifySignature(wallet, message, signature string) error {
	pub, err := DecodeBase58(wallet, PublicKeyBytes)
	if err != nil {
		return fmt.Errorf("wallet: %w", err)
	}
	if len(pub) != PublicKeyBytes {
		return fmt.Errorf("wallet public key must decode to %d bytes", PublicKeyBytes)
	}
	sig, err := DecodeSignature(signature)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(message), sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

func VerifyIdentity(att *IdentityAttestation) error {
	if att == nil {
		return fmt.Errorf("nil attestation")
	}
	wallet, err := NormalizeWallet(att.WalletPubkey)
	if err != nil {
		return fmt.Errorf("wallet pubkey: %w", err)
	}
	payload, err := json.Marshal(identityPayload{
		PeerID:       att.PeerID,
		WalletPubkey: wallet,
		Timestamp:    att.Timestamp,
	})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	rawSig, err := base64.StdEncoding.DecodeString(att.Signature)
	if err != nil {
		return fmt.Errorf("signature encoding: %w", err)
	}
	if len(rawSig) != SignatureBytes {
		return fmt.Errorf("signature must decode to %d bytes", SignatureBytes)
	}
	pub, _ := DecodeBase58(wallet, PublicKeyBytes)
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, rawSig) {
		return fmt.Errorf("identity signature verification failed")
	}
	return nil
}

func NewChallengeID() (string, error) {
	return randomURLToken(18)
}

func NewChallengeNonce() (string, error) {
	return randomURLToken(24)
}

func BuildChallengeMessage(accountID, wallet, nonce string, issuedAt, expiresAt time.Time) string {
	return fmt.Sprintf(
		"api.opentela.ai wallet-link v1\nsub:%s\nwallet:%s\nnonce:%s\nissued_at:%s\nexpires_at:%s",
		accountID,
		wallet,
		nonce,
		issuedAt.UTC().Format(time.RFC3339),
		expiresAt.UTC().Format(time.RFC3339),
	)
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
