package billing

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

// Merkle reconciliation (design §11.5.5): a deterministic hash tree over an
// account's credit_ledger rows lets the user verify their balance
// independently — recompute the root from their own ledger copy (the manage
// API serves every row) and compare it with the root the API reports. The
// tree rules are deliberately simple and fully specified:
//
//   leaf  = SHA256("otela-ledger-leaf:v1" || encode(row))
//   node  = SHA256("otela-merkle-node:v1" || left || right)
//   pad   = SHA256("otela-merkle-pad:v1")  (all virtual leaves)
//
// The leaf count is padded up to the next power of two with the pad hash;
// a proof is the standard audit path, where the verifier substitutes the
// pad hash for siblings beyond leafCount.

const (
	domainLedgerLeaf = "otela-ledger-leaf:v1"
	domainMerkleNode = "otela-merkle-node:v1"
	domainMerklePad  = "otela-merkle-pad:v1"
)

// LedgerLeafInput is one credit_ledger row reduced to the fields a balance
// verification needs: identity (id, ref), amount (delta_raw), direction
// (source, leg), counterparty, and time. Two rows that agree on all of these
// are the same movement for reconciliation purposes.
type LedgerLeafInput struct {
	ID           int64
	DeltaRaw     int64
	Source       string
	Leg          string
	Counterparty string
	Ref          string
	CreatedAt    time.Time
}

// LedgerLeaf serializes one row (length-prefixed, unambiguous) and hashes it
// into a Merkle leaf.
func LedgerLeaf(row LedgerLeafInput) []byte {
	h := sha256.New()
	h.Write([]byte(domainLedgerLeaf))
	putI64 := func(v int64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v))
		h.Write(b[:])
	}
	putStr := func(s string) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(s)))
		h.Write(b[:])
		h.Write([]byte(s))
	}
	putI64(row.ID)
	putI64(row.DeltaRaw)
	putStr(row.Source)
	putStr(row.Leg)
	putStr(row.Counterparty)
	putStr(row.Ref)
	ts, _ := row.CreatedAt.UTC().MarshalText()
	putStr(string(ts))
	sum := h.Sum(nil)
	return sum
}

func merkleNode(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte(domainMerkleNode))
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

var merklePadHash = func() []byte {
	h := sha256.Sum256([]byte(domainMerklePad))
	return h[:]
}()

// MerkleRoot computes the tree root over leaves. Empty input hashes to the
// pad hash alone (a well-defined empty root). Leaves are used in the given
// order — callers pass ledger rows in ascending id.
func MerkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		out := make([]byte, sha256.Size)
		copy(out, merklePadHash)
		return out
	}
	level := make([][]byte, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, merklePadHash)
		}
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			next = append(next, merkleNode(level[i], level[i+1]))
		}
		level = next
	}
	return level[0]
}

// MerklePath returns the audit path for the leaf at index. The caller must
// pass the FULL leaf list (in ledger id order); index must be in range.
func MerklePath(leaves [][]byte, index int) ([][]byte, error) {
	if index < 0 || index >= len(leaves) {
		return nil, fmt.Errorf("merkle: index %d out of range (%d leaves)", index, len(leaves))
	}
	padded := make([][]byte, len(leaves))
	copy(padded, leaves)
	var path [][]byte
	level := 0
	for len(padded) > 1 {
		if len(padded)%2 == 1 {
			padded = append(padded, merklePadHash)
		}
		sibling := index ^ 1
		path = append(path, padded[sibling])
		index /= 2
		next := make([][]byte, 0, len(padded)/2)
		for i := 0; i < len(padded); i += 2 {
			next = append(next, merkleNode(padded[i], padded[i+1]))
		}
		padded = next
		level++
	}
	return path, nil
}

// VerifyMerklePath recomputes the root from one leaf and its audit path —
// the user-side check: leafCount is the UNPADDED leaf count, index the
// leaf's 0-based position (its rank among the account's ledger rows), and
// root the value reported by the API. At each level the sibling is the pad
// hash exactly when its position is beyond that level's real node count
// (ceil-halving from the unpadded count); otherwise it comes from the path.
func VerifyMerklePath(leaf []byte, path [][]byte, index, leafCount int, root []byte) bool {
	if leafCount <= 0 || index < 0 || index >= leafCount {
		return false
	}
	cur := leaf
	idx := index
	for level := 0; level < len(path); level++ {
		siblingIdx := idx ^ 1
		var sib []byte
		if siblingIdx >= sizeAtLevel(leafCount, level) {
			sib = merklePadHash
		} else {
			if level >= len(path) {
				return false
			}
			sib = path[level]
		}
		if idx%2 == 0 {
			cur = merkleNode(cur, sib)
		} else {
			cur = merkleNode(sib, cur)
		}
		idx /= 2
	}
	if len(cur) != len(root) {
		return false
	}
	for i := range cur {
		if cur[i] != root[i] {
			return false
		}
	}
	return true
}

// sizeAtLevel returns the number of real-or-pad nodes at the given level of
// a tree whose leaf level holds `leaves` slots (padded to a power of two by
// the caller's loop). It counts how many siblings are real at that level so
// the verifier knows when to substitute the pad hash.
func sizeAtLevel(leaves, level int) int {
	// At each level up, the count of nodes is ceil(count/2) starting from
	// the UNPADDED count — pads never promote beyond what parity requires.
	n := leaves
	for i := 0; i < level; i++ {
		n = (n + 1) / 2
	}
	return n
}
