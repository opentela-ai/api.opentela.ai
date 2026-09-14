package billing

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"
)

func testLeaves(n int) [][]byte {
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		row := LedgerLeafInput{
			ID:           int64(100 + i),
			DeltaRaw:     int64(-100 * (i + 1)),
			Source:       "usage",
			Leg:          "buyer",
			Counterparty: "peer-1",
			Ref:          "req-" + string(rune('a'+i)),
			CreatedAt:    time.Unix(1_700_000_000+int64(i), 0).UTC(),
		}
		out[i] = LedgerLeaf(row)
	}
	return out
}

// TestLedgerLeafIsDeterministicAndDistinct: same row → same leaf; any field
// change → different leaf; the encoding is unambiguous (length-prefixed).
func TestLedgerLeafIsDeterministicAndDistinct(t *testing.T) {
	row := LedgerLeafInput{
		ID: 1, DeltaRaw: -500, Source: "usage", Leg: "buyer",
		Counterparty: "p1", Ref: "r1", CreatedAt: time.Unix(0, 0).UTC(),
	}
	a := LedgerLeaf(row)
	b := LedgerLeaf(row)
	if !bytes.Equal(a, b) {
		t.Fatal("same row must hash identically")
	}
	row.DeltaRaw = -501
	if bytes.Equal(a, LedgerLeaf(row)) {
		t.Fatal("amount change must change the leaf")
	}
	row = LedgerLeafInput{ID: 1, DeltaRaw: -500, Source: "usage", Leg: "buyer",
		Counterparty: "p1", Ref: "r1", CreatedAt: time.Unix(0, 0).UTC()}
	// Ambiguity probe: ("ab","c") vs ("a","bc") must differ.
	x := LedgerLeaf(LedgerLeafInput{ID: 1, DeltaRaw: 0, Source: "ab", Leg: "c",
		Counterparty: "", Ref: "", CreatedAt: time.Unix(0, 0).UTC()})
	y := LedgerLeaf(LedgerLeafInput{ID: 1, DeltaRaw: 0, Source: "a", Leg: "bc",
		Counterparty: "", Ref: "", CreatedAt: time.Unix(0, 0).UTC()})
	if bytes.Equal(x, y) {
		t.Fatal("length-prefix collision: concatenation ambiguity")
	}
}

// TestMerkleRootMatchesIndependentComputation: a known 4-leaf tree computed
// by hand with the documented rules.
func TestMerkleRootMatchesIndependentComputation(t *testing.T) {
	leaves := testLeaves(4)
	root := MerkleRoot(leaves)

	node := func(l, r []byte) []byte {
		h := sha256.New()
		h.Write([]byte("otela-merkle-node:v1"))
		h.Write(l)
		h.Write(r)
		return h.Sum(nil)
	}
	l0 := node(leaves[0], leaves[1])
	l1 := node(leaves[2], leaves[3])
	want := node(l0, l1)
	if !bytes.Equal(root, want) {
		t.Fatal("root does not match the documented tree rules")
	}
}

func TestMerkleRootEmpty(t *testing.T) {
	root := MerkleRoot(nil)
	if len(root) != sha256.Size {
		t.Fatalf("empty root len = %d", len(root))
	}
	pad := sha256.Sum256([]byte("otela-merkle-pad:v1"))
	if !bytes.Equal(root, pad[:]) {
		t.Fatal("empty root must be the pad hash")
	}
}

// TestMerkleProofsRoundTrip: for every tree size 1..16 and every leaf
// index, the audit path must verify against the root — and fail against a
// tampered root, leaf, or index.
func TestMerkleProofsRoundTrip(t *testing.T) {
	for n := 1; n <= 16; n++ {
		leaves := testLeaves(n)
		root := MerkleRoot(leaves)
		for i := 0; i < n; i++ {
			path, err := MerklePath(leaves, i)
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if !VerifyMerklePath(leaves[i], path, i, n, root) {
				t.Fatalf("n=%d i=%d: valid proof rejected", n, i)
			}
			// Tampered root.
			badRoot := append([]byte(nil), root...)
			badRoot[0] ^= 1
			if VerifyMerklePath(leaves[i], path, i, n, badRoot) {
				t.Fatalf("n=%d i=%d: proof accepted against tampered root", n, i)
			}
			// Wrong leaf.
			badLeaf := append([]byte(nil), leaves[i]...)
			badLeaf[0] ^= 1
			if VerifyMerklePath(badLeaf, path, i, n, root) {
				t.Fatalf("n=%d i=%d: tampered leaf accepted", n, i)
			}
			// Wrong index (swap parity when possible).
			if n > 1 {
				j := i ^ 1
				if VerifyMerklePath(leaves[i], path, j, n, root) {
					t.Fatalf("n=%d i=%d: proof accepted at wrong index %d", n, i, j)
				}
			}
		}
	}
}

func TestMerklePathOutOfRange(t *testing.T) {
	leaves := testLeaves(3)
	if _, err := MerklePath(leaves, 3); err == nil {
		t.Fatal("out-of-range index must fail")
	}
	if _, err := MerklePath(leaves, -1); err == nil {
		t.Fatal("negative index must fail")
	}
	if VerifyMerklePath(leaves[0], nil, 0, 0, MerkleRoot(leaves)) {
		t.Fatal("zero leafCount must fail verification")
	}
}

// TestMerkleRootSensitivity: any single-row change changes the root.
func TestMerkleRootSensitivity(t *testing.T) {
	leaves := testLeaves(7)
	root := MerkleRoot(leaves)
	for i := 0; i < 7; i++ {
		tampered := make([][]byte, 7)
		copy(tampered, leaves)
		tampered[i] = append([]byte(nil), tampered[i]...)
		tampered[i][0] ^= 1
		if bytes.Equal(MerkleRoot(tampered), root) {
			t.Fatalf("tampering leaf %d left the root unchanged", i)
		}
	}
}
