package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opentela-ai/api/internal/billing"
)

// §11.5.5 store plumbing: paged ledger leaves → Merkle commitment, the
// row-level inclusion proof, in-flight sums for the registry-vs-chain
// cross-check, and residual exposure over terminally failed settlements.

func TestAccountMerkleSummaryAndProof(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	seedCredit(t, p, "merkle-acct", 50_000)
	// Two settled charges → more ledger rows (grant mirror not present for
	// plain deposits: this is the deposit rail only).
	settleDelegated(t, p, "merkle-req-1", "merkle-acct", "peer-a", "seller-a", 500)
	settleDelegated(t, p, "merkle-req-2", "merkle-acct", "peer-b", "seller-b", 500)

	sum, err := p.AccountMerkleSummary(ctx, "merkle-acct")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.LeafCount == 0 {
		t.Fatal("leaf count must cover ledger rows")
	}

	// Independent recomputation: page the leaves and re-root.
	var leaves [][]byte
	after := int64(0)
	for {
		page, err := p.LedgerLeafPage(ctx, "merkle-acct", after, 1)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		leaves = append(leaves, billing.LedgerLeaf(page[0]))
		after = page[0].ID
	}
	if int64(len(leaves)) != sum.LeafCount {
		t.Fatalf("paged %d leaves, summary says %d", len(leaves), sum.LeafCount)
	}
	recomputed := billing.MerkleRoot(leaves)
	if string(recomputed) != string(sum.Root) {
		t.Fatal("recomputed root differs from the summary root")
	}

	// Inclusion proof for the first row verifies; for the wrong root, fails.
	// Re-fetch the first row's id to address the proof.
	page, err := p.LedgerLeafPage(ctx, "merkle-acct", 0, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("first page: n=%d err=%v", len(page), err)
	}
	leaf, path, root, count, err := p.MerkleProofForLedgerRow(ctx, "merkle-acct", page[0].ID)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if count != int(sum.LeafCount) || string(root) != string(sum.Root) {
		t.Fatalf("proof context mismatch: count %d root %x", count, root[:4])
	}
	if !billing.VerifyMerklePath(leaf, path, 0, count, root) {
		t.Fatal("valid inclusion proof rejected")
	}
	other := leaves[len(leaves)-1]
	if billing.VerifyMerklePath(other, path, 0, count, root) {
		t.Fatal("wrong leaf accepted")
	}

	// Unknown row → ErrNotFound.
	if _, _, _, _, err := p.MerkleProofForLedgerRow(ctx, "merkle-acct", 999_999_999); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("unknown row err = %v, want ErrNotFound", err)
	}
}

func TestLedgerLeafIndexPositions(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()

	seedCredit(t, p, "idx-acct", 10_000)
	settleDelegated(t, p, "idx-req-1", "idx-acct", "peer-a", "seller-a", 500)

	after := int64(0)
	var ids []int64
	for {
		page, err := p.LedgerLeafPage(ctx, "idx-acct", after, 1000)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		ids = append(ids, page[0].ID)
		after = page[0].ID
	}
	if len(ids) < 2 {
		t.Fatalf("need ≥2 ledger rows, got %d", len(ids))
	}
	for i, id := range ids {
		idx, ok, err := p.LedgerLeafIndex(ctx, "idx-acct", id)
		if err != nil || !ok {
			t.Fatalf("row %d: ok=%v err=%v", id, ok, err)
		}
		if idx != i {
			t.Fatalf("row %d index = %d, want %d", id, idx, i)
		}
	}
	if _, ok, err := p.LedgerLeafIndex(ctx, "idx-acct", 424_242); ok || err != nil {
		t.Fatalf("unknown row: ok=%v err=%v", ok, err)
	}
}

func TestInFlightAndExposureSums(t *testing.T) {
	p := newTestStore(t)
	ctx := context.Background()
	delegate := seedPubkey("recon-delegate")
	buyerWallet := seedPubkey("recon-buyer-wallet")
	sellerWallet := seedPubkey("recon-seller-wallet")

	linkWallet(t, p, "recon-buyer", buyerWallet)
	linkWallet(t, p, "recon-seller", sellerWallet)
	seedCredit(t, p, "recon-buyer", 100_000)
	seedCredit(t, p, "recon-seller", 0)
	grantAllowance(t, p, "recon-buyer", delegate, 100_000, "recon-obs-1")

	// cost 4800, seller 4560, fee 240.
	_, sellerRaw, feeRaw := settleDelegated(t, p, "recon-req-1", "recon-buyer", "p1", "recon-seller", 500)

	params := SettlementClaimParams{
		Delegate: delegate, Mint: seedPubkey("recon-mint"),
		TokenProgram: seedPubkey("recon-tp"), TreasuryWallet: seedPubkey("recon-treasury"),
		LegLimit: 100,
	}
	created, err := p.ClaimDelegationSettlements(ctx, params, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var sellerBatch, feeBatch *billing.DelegationSettlement
	for i := range created {
		if created[i].DestinationWallet == sellerWallet {
			sellerBatch = &created[i]
		} else {
			feeBatch = &created[i]
		}
	}
	if sellerBatch == nil || feeBatch == nil {
		t.Fatalf("batches missing: %+v", created)
	}

	// Both pending → in-flight = seller + fee per delegate; no exposure.
	inFlight, err := p.InFlightSettlements(ctx, "recon-buyer")
	if err != nil {
		t.Fatalf("in-flight: %v", err)
	}
	if got := inFlight[delegate]; got != sellerRaw+feeRaw {
		t.Fatalf("in-flight = %d, want %d", got, sellerRaw+feeRaw)
	}
	exp, err := p.SettlementExposure(ctx, "recon-buyer")
	if err != nil {
		t.Fatalf("exposure: %v", err)
	}
	if exp.RestoredCount != 0 || exp.RestoredRaw != 0 {
		t.Fatalf("pre-failure exposure = %+v", exp)
	}

	// Restore the seller batch (terminal collection failure) → exposure.
	if ok, err := p.RestoreSettlement(ctx, sellerBatch.ID, "test-restore", time.Now().UTC()); !ok || err != nil {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	exp, err = p.SettlementExposure(ctx, "recon-buyer")
	if err != nil {
		t.Fatalf("exposure: %v", err)
	}
	if exp.RestoredCount != 1 || exp.RestoredRaw != sellerRaw {
		t.Fatalf("exposure after restore = %+v, want count 1 raw %d", exp, sellerRaw)
	}
	// In-flight shrinks by the restored batch.
	inFlight, err = p.InFlightSettlements(ctx, "recon-buyer")
	if err != nil {
		t.Fatalf("in-flight: %v", err)
	}
	if got := inFlight[delegate]; got != feeRaw {
		t.Fatalf("in-flight after restore = %d, want %d", got, feeRaw)
	}

	// Finalize the fee batch → in-flight empty. The worker's real state
	// path is pending → signed → broadcast → finalized; walk it.
	wire := "d2lyZQ=="
	if ok, err := p.MarkSettlementSigned(ctx, feeBatch.ID, wire, "sig", "bh", 1000, time.Now().UTC().Add(time.Minute), time.Now().UTC()); !ok || err != nil {
		t.Fatalf("sign: ok=%v err=%v", ok, err)
	}
	if ok, err := p.MarkSettlementBroadcast(ctx, feeBatch.ID, time.Now().UTC()); !ok || err != nil {
		t.Fatalf("broadcast: ok=%v err=%v", ok, err)
	}
	if ok, err := p.FinalizeSettlement(ctx, feeBatch.ID, time.Now().UTC()); !ok || err != nil {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	inFlight, err = p.InFlightSettlements(ctx, "recon-buyer")
	if err != nil {
		t.Fatalf("in-flight: %v", err)
	}
	if got := inFlight[delegate]; got != 0 {
		t.Fatalf("in-flight after finalize = %d, want 0", got)
	}
	// Exposure unchanged by finalization.
	if exp, _ = p.SettlementExposure(ctx, "recon-buyer"); exp.RestoredCount != 1 {
		t.Fatalf("exposure changed: %+v", exp)
	}
}
