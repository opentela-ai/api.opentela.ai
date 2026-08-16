package faucet

import "github.com/opentela-ai/api/internal/solana"

// PDA / associated-token-address derivation lives in internal/solana now so
// the billing deposit watcher and the faucet share one canonical algorithm.
// These thin wrappers keep the faucet's unexported call sites and golden
// vectors unchanged.

// FindProgramAddress derives the canonical program address for seeds under
// programID. Delegates to solana.FindProgramAddress.
func FindProgramAddress(seeds [][]byte, programID []byte) ([]byte, error) {
	return solana.FindProgramAddress(seeds, programID)
}

// isOnCurve reports whether the 32-byte little-endian compressed point
// decodes to a valid ed25519 curve point. Delegates to solana.IsOnCurve.
func isOnCurve(b []byte) bool { return solana.IsOnCurve(b) }
