package faucet

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/opentela-ai/api/internal/solana"
)

// Golden vectors computed with @solana/spl-token getAssociatedTokenAddress
// (mint = EsmcTrdLkFqV3mv4CjLF3AmCx132ixfFSYYRWD78cDzR, classic SPL Token
// program). A wrong PDA derivation (seeds, program id, or on-curve check)
// breaks these.
var ataVectors = []struct {
	owner string
	ata   string
}{
	{"wyXQMgDFSzHvCwz1aK79r6Qr5BxqxKFK4Ra8u7xPBhz", "4ZdrF3WBc9aGKdhGCS9UXSGmKV4kcoaRVTRvZrirBSQQ"},
	{"9Sw2Npg2p14KnxomaEqkgUj51CnLkMzqPDCM4495AZNu", "4oL7iXCsG17XKkdM3KYn5vzsHY32b5W9aVxJmy1h6v1z"},
}

const (
	testMint = "EsmcTrdLkFqV3mv4CjLF3AmCx132ixfFSYYRWD78cDzR"
	testSPL  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

func TestAssociatedTokenAddress(t *testing.T) {
	mint := mustDecodeB58(testMint)
	tp := mustDecodeB58(testSPL)
	for _, v := range ataVectors {
		owner := mustDecodeB58(v.owner)
		got, err := associatedTokenAddress(owner, mint, tp)
		if err != nil {
			t.Fatalf("associatedTokenAddress(%s): %v", v.owner, err)
		}
		if enc := solana.EncodeBase58(got); enc != v.ata {
			t.Errorf("associatedTokenAddress(%s) = %s, want %s", v.owner, enc, v.ata)
		}
	}
}

// isOnCurve sanity: y=1 (the identity point) is on the curve; a SHA-256 of
// arbitrary bytes is essentially never a valid point.
func TestIsOnCurveSanity(t *testing.T) {
	identity := make([]byte, 32)
	identity[0] = 1 // y = 1, little-endian
	if !isOnCurve(identity) {
		t.Error("identity point should be on curve")
	}
	hash := sha256.Sum256([]byte("off-curve candidate"))
	if isOnCurve(hash[:]) {
		t.Error("random hash should not be on curve")
	}
}

func TestBuildTransferMessageCreatesATA(t *testing.T) {
	faucet := make([]byte, 32)
	faucet[31] = 1
	owner := make([]byte, 32)
	owner[31] = 2
	mint := make([]byte, 32)
	mint[31] = 3
	source := make([]byte, 32)
	source[31] = 4
	dest := make([]byte, 32)
	dest[31] = 5
	blockhash := make([]byte, 32)
	blockhash[0] = 0xaa
	tp := mustDecodeB58(testSPL)

	msg := buildTransferMessage(faucet, source, dest, owner, mint, tp, true, false, blockhash, 12345)

	// Header: 1 signer, 0 readonly signed, readonly unsigned = owner, mint,
	// system, ata-prog, token-prog, rent = 6.
	if msg[0] != 1 || msg[1] != 0 || msg[2] != 6 {
		t.Fatalf("header = %d,%d,%d, want 1,0,6", msg[0], msg[1], msg[2])
	}
	// Account count: 9 (faucet, source, dest, owner, mint, system, ata-prog,
	// token-prog, rent).
	if msg[3] != 9 {
		t.Fatalf("account count = %d, want 9", msg[3])
	}
	// After header (3) + account count (1) + 9*32 accounts = offset 292,
	// the blockhash.
	if !bytes.Equal(msg[4+9*32:4+9*32+32], blockhash) {
		t.Error("blockhash not placed after account keys")
	}
	// Instructions follow the blockhash: compact-u16 count = 2.
	ixOffset := 4 + 9*32 + 32
	if msg[ixOffset] != 2 {
		t.Fatalf("instruction count = %d, want 2", msg[ixOffset])
	}
	// First instruction: program index 6 (ATA program), 7 accounts, data
	// len 1, data 0.
	if msg[ixOffset+1] != 6 {
		t.Fatalf("ata program index = %d, want 6", msg[ixOffset+1])
	}
	if msg[ixOffset+2] != 7 {
		t.Fatalf("ata account count = %d, want 7", msg[ixOffset+2])
	}
	// create_associated_token_account accounts, matching
	// @solana/spl-token: [payer, ata, owner, mint, system, token-prog, rent].
	wantCreateAccts := []byte{0, 2, 3, 4, 5, 7, 8}
	if got := msg[ixOffset+3 : ixOffset+3+7]; !bytes.Equal(got, wantCreateAccts) {
		t.Fatalf("ata accounts = %v, want %v", got, wantCreateAccts)
	}
	if msg[ixOffset+3+7] != 1 {
		t.Fatalf("ata data len = %d, want 1", msg[ixOffset+3+7])
	}
	// Second instruction starts after instruction 1: program index (1) +
	// account count (1) + 7 indices + data len (1) + data (1) = 11 bytes.
	second := ixOffset + 1 + 11
	if msg[second] != 7 {
		t.Fatalf("token program index = %d, want 7", msg[second])
	}
	if msg[second+1] != 3 {
		t.Fatalf("transfer account count = %d, want 3", msg[second+1])
	}
	if msg[second+1+3+1] != 9 {
		t.Fatalf("transfer data len = %d, want 9", msg[second+1+3+1])
	}
	if msg[second+1+3+2] != 3 {
		t.Fatalf("transfer opcode = %d, want 3", msg[second+1+3+2])
	}
}

func TestBuildTransferMessageNoATA(t *testing.T) {
	faucet := make([]byte, 32)
	owner := make([]byte, 32)
	mint := make([]byte, 32)
	source := make([]byte, 32)
	dest := make([]byte, 32)
	blockhash := make([]byte, 32)
	tp := mustDecodeB58(testSPL)

	msg := buildTransferMessage(faucet, source, dest, owner, mint, tp, false, false, blockhash, 99)
	// Header: readonly unsigned = token program only = 1.
	if msg[0] != 1 || msg[1] != 0 || msg[2] != 1 {
		t.Fatalf("header = %d,%d,%d, want 1,0,1", msg[0], msg[1], msg[2])
	}
	if msg[3] != 4 {
		t.Fatalf("account count = %d, want 4", msg[3])
	}
	// One instruction; token program at index 3.
	ixOffset := 4 + 4*32 + 32
	if msg[ixOffset] != 1 {
		t.Fatalf("instruction count = %d, want 1", msg[ixOffset])
	}
	if msg[ixOffset+1] != 3 {
		t.Fatalf("token program index = %d, want 3", msg[ixOffset+1])
	}
}

func TestSignAndSerializeRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("the signed message")
	wire := signAndSerialize(priv, message)

	// Wire: compact-u16 1 (single byte 0x01), 64-byte signature, then message.
	if len(wire) != 1+ed25519.SignatureSize+len(message) {
		t.Fatalf("wire length = %d", len(wire))
	}
	if wire[0] != 1 {
		t.Fatalf("signature count = %d, want 1", wire[0])
	}
	if !ed25519.Verify(pub, message, wire[1:1+ed25519.SignatureSize]) {
		t.Error("signature does not verify over the message")
	}
	if !bytes.Equal(wire[1+ed25519.SignatureSize:], message) {
		t.Error("message bytes differ after signature")
	}
}
