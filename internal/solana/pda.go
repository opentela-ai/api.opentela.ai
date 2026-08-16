package solana

import (
	"crypto/sha256"
	"errors"
	"math/big"
)

// Program-derived address derivation, extracted from internal/faucet so the
// deposit watcher and the faucet share one canonical ATA computation. The
// algorithm matches the Solana runtime's find_program_address / create_with_seed
// domain separation.

// Well-known Solana program ids (base58). Decoded once at init via the shared
// base58 helper.
var (
	SystemProgramID    = mustDecodeConst("11111111111111111111111111111111")
	TokenProgramID     = mustDecodeConst("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	Token2022ProgramID = mustDecodeConst("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	ATAProgramID       = mustDecodeConst("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	RentSysvarID       = mustDecodeConst("SysvarRent111111111111111111111111111111111")
)

func mustDecodeConst(s string) []byte {
	b, err := DecodeBase58(s, PublicKeyBytes)
	if err != nil || len(b) != PublicKeyBytes {
		panic("solana: invalid built-in program id " + s)
	}
	return b
}

// Field arithmetic for the ed25519 curve, needed only to check whether a
// candidate derived address lands on the curve (on-curve addresses are not
// usable as program-derived addresses, so PDA derivation bumps the seed until
// the hash is off-curve).
var (
	// fieldP = 2^255 - 19
	fieldP = func() *big.Int {
		p := new(big.Int).Lsh(big.NewInt(1), 255)
		return p.Sub(p, big.NewInt(19))
	}()

	// curveD = -121665 / 121666 mod p
	curveD = func() *big.Int {
		num := new(big.Int).Mod(big.NewInt(-121665), fieldP)
		den := new(big.Int).ModInverse(big.NewInt(121666), fieldP)
		return new(big.Int).Mod(num.Mul(num, den), fieldP)
	}()

	// sqrtM1 = 2^((p-1)/4) mod p, a square root of -1
	sqrtM1 = func() *big.Int {
		e := new(big.Int).Sub(fieldP, big.NewInt(1))
		e.Rsh(e, 2)
		return new(big.Int).Exp(big.NewInt(2), e, fieldP)
	}()
)

// IsOnCurve reports whether the 32-byte little-endian compressed point
// decodes to a valid ed25519 curve point. A point on the curve cannot be a
// program-derived address.
func IsOnCurve(b []byte) bool {
	return isOnCurve(b)
}

// isOnCurve is the unexported implementation, kept here so the faucet can
// delegate without naming the curve internals.
func isOnCurve(b []byte) bool {
	y := new(big.Int).SetBytes(reverseBytes(b))
	// Clear the sign bit (the top bit of the last byte) — only the y
	// coordinate and x parity matter for the curve equation.
	y.And(y, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1)))

	// x^2 = (y^2 - 1) / (d*y^2 + 1) mod p
	y2 := new(big.Int).Mod(new(big.Int).Mul(y, y), fieldP)
	num := new(big.Int).Mod(new(big.Int).Sub(y2, big.NewInt(1)), fieldP)
	den := new(big.Int).Add(new(big.Int).Mul(curveD, y2), big.NewInt(1))
	den.Mod(den, fieldP)
	den.ModInverse(den, fieldP)
	x2 := new(big.Int).Mod(new(big.Int).Mul(num, den), fieldP)

	// x = x2^((p+3)/8); if x^2 != x2, try x *= sqrt(-1) once more.
	e := new(big.Int).Rsh(new(big.Int).Add(fieldP, big.NewInt(3)), 3)
	x := new(big.Int).Exp(x2, e, fieldP)
	if !squareEq(x, x2) {
		x.Mod(x.Mul(x, sqrtM1), fieldP)
		if !squareEq(x, x2) {
			return false
		}
	}
	return true
}

func squareEq(x, target *big.Int) bool {
	return new(big.Int).Mod(new(big.Int).Mul(x, x), fieldP).Cmp(target) == 0
}

func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// CreateProgramAddress derives a program address for seeds and an explicit
// bump byte, matching the Solana runtime's create_program_address. It returns
// an error when the digest lands on the ed25519 curve (the address would be
// indistinguishable from a real key pair and is therefore invalid).
func CreateProgramAddress(seeds [][]byte, programID []byte, bump byte) ([]byte, error) {
	h := sha256.New()
	for _, s := range seeds {
		h.Write(s)
	}
	h.Write([]byte{bump})
	h.Write(programID)
	h.Write([]byte("ProgramDerivedAddress"))
	sum := h.Sum(nil)
	if isOnCurve(sum) {
		return nil, errors.New("solana: address lands on the ed25519 curve")
	}
	return sum, nil
}

// FindProgramAddress derives the canonical program address for seeds under
// programID, using the same bump search as the Solana runtime
// (find_program_address): bump from 255 downward, take the first SHA-256
// digest that does not land on the ed25519 curve.
func FindProgramAddress(seeds [][]byte, programID []byte) ([]byte, error) {
	for bump := byte(255); ; bump-- {
		addr, err := CreateProgramAddress(seeds, programID, bump)
		if err == nil {
			return addr, nil
		}
		if bump == 0 {
			break
		}
	}
	return nil, errors.New("solana: unable to find a program address for the given seeds")
}

// AssociatedTokenAddress derives the SPL associated token account for
// (owner, mint) under tokenProgram, matching
// getAssociatedTokenAddress(owner, mint, false, tokenProgram). The seeds are
// [owner, tokenProgram, mint] and the program is the SPL Associated Token
// Account program.
func AssociatedTokenAddress(owner, mint, tokenProgram []byte) ([]byte, error) {
	return FindProgramAddress([][]byte{owner, tokenProgram, mint}, ATAProgramID)
}
