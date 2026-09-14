package store

import (
	"context"
	"fmt"

	"github.com/opentela-ai/api/internal/billing"
)

// Reconciliation support (design §11.5.5): paged ledger leaves for Merkle
// commitments, per-delegate in-flight sums for the registry-vs-chain
// cross-check, and the residual-exposure totals for terminally failed
// settlements.

// LedgerLeafPage returns up to limit ledger rows for the account with id >
// afterID, ascending — one page of the Merkle leaf stream. Pages make root
// computation streaming-friendly for large ledgers.
func (p *Postgres) LedgerLeafPage(ctx context.Context, accountID string, afterID, limit int64) ([]billing.LedgerLeafInput, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, delta_raw, source, leg, COALESCE(counterparty, ''),
		       COALESCE(ref, ''), created_at
		FROM credit_ledger
		WHERE account_id = $1 AND id > $2
		ORDER BY id
		LIMIT $3`, accountID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ledger leaf page: %w", err)
	}
	defer rows.Close()
	var out []billing.LedgerLeafInput
	for rows.Next() {
		var l billing.LedgerLeafInput
		if err := rows.Scan(&l.ID, &l.DeltaRaw, &l.Source, &l.Leg, &l.Counterparty, &l.Ref, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: ledger leaf page: scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ledger leaf page: %w", err)
	}
	return out, nil
}

// LedgerLeafIndex returns the 0-based position of the row with id in the
// account's ledger stream (count of rows with smaller id) — the index a
// Merkle proof is addressed by. ok=false when the row does not exist.
func (p *Postgres) LedgerLeafIndex(ctx context.Context, accountID string, leafID int64) (int, bool, error) {
	var n int64
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM credit_ledger
		WHERE account_id = $1 AND id < $2`, accountID, leafID).Scan(&n)
	if err != nil {
		return 0, false, fmt.Errorf("store: ledger leaf index: %w", err)
	}
	var exists bool
	err = p.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM credit_ledger WHERE account_id = $1 AND id = $2)`,
		accountID, leafID).Scan(&exists)
	if err != nil {
		return 0, false, fmt.Errorf("store: ledger leaf index: %w", err)
	}
	if !exists {
		return 0, false, nil
	}
	return int(n), true, nil
}

// SettlementExposure sums the terminally failed settlement batches for the
// account: claimed (allowance consumed, legs anchored) but never finalized
// on-chain. These are the charges the platform credited to sellers but
// could not collect from the buyer — §11.5.5's residual exposure.
func (p *Postgres) SettlementExposure(ctx context.Context, accountID string) (billing.ExposureSummary, error) {
	var out billing.ExposureSummary
	err := p.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount_raw), 0)
		FROM delegation_settlements
		WHERE buyer_account = $1 AND state IN ('restored', 'failed')`,
		accountID).Scan(&out.RestoredCount, &out.RestoredRaw)
	if err != nil {
		return out, fmt.Errorf("store: settlement exposure: %w", err)
	}
	return out, nil
}

// InFlightSettlements sums not-yet-finalized settlement amounts per
// delegate for the account: consumed from the registry (at claim) but not
// yet confirmed on-chain, so the chain's delegated amount should read
// allowance − in-flight. Drives the §11.5.5 cross-check.
func (p *Postgres) InFlightSettlements(ctx context.Context, accountID string) (map[string]int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT delegate, COALESCE(SUM(amount_raw), 0)
		FROM delegation_settlements
		WHERE buyer_account = $1 AND state IN ('pending', 'signed', 'broadcast')
		GROUP BY delegate`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: in-flight settlements: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var delegate string
		var sum int64
		if err := rows.Scan(&delegate, &sum); err != nil {
			return nil, fmt.Errorf("store: in-flight settlements: scan: %w", err)
		}
		out[delegate] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: in-flight settlements: %w", err)
	}
	return out, nil
}

// MerkleProofForLedgerRow builds the inclusion proof for one ledger row:
// pages the account's full ledger (O(n), documented — the console caches
// roots per page), hashes the leaves, and returns the leaf, its audit path,
// the root, and the leaf count.
func (p *Postgres) MerkleProofForLedgerRow(ctx context.Context, accountID string, leafID int64) (leaf []byte, path [][]byte, root []byte, leafCount int, err error) {
	index, ok, err := p.LedgerLeafIndex(ctx, accountID, leafID)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if !ok {
		return nil, nil, nil, 0, billing.ErrNotFound
	}
	var leaves [][]byte
	after := int64(0)
	for {
		page, err := p.LedgerLeafPage(ctx, accountID, after, 1000)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			leaves = append(leaves, billing.LedgerLeaf(row))
		}
		after = page[len(page)-1].ID
		if len(page) < 1000 {
			break
		}
	}
	if index >= len(leaves) {
		return nil, nil, nil, 0, fmt.Errorf("store: merkle proof: index %d beyond %d leaves", index, len(leaves))
	}
	path, err = billing.MerklePath(leaves, index)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("store: merkle proof: %w", err)
	}
	return leaves[index], path, billing.MerkleRoot(leaves), len(leaves), nil
}

// AccountMerkleSummary computes just the root and leaf count (the value the
// reconciliation endpoint reports) by paging the whole ledger.
func (p *Postgres) AccountMerkleSummary(ctx context.Context, accountID string) (billing.MerkleSummary, error) {
	var leaves [][]byte
	after := int64(0)
	var count int64
	for {
		page, err := p.LedgerLeafPage(ctx, accountID, after, 1000)
		if err != nil {
			return billing.MerkleSummary{}, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			leaves = append(leaves, billing.LedgerLeaf(row))
		}
		count += int64(len(page))
		after = page[len(page)-1].ID
		if len(page) < 1000 {
			break
		}
	}
	return billing.MerkleSummary{Root: billing.MerkleRoot(leaves), LeafCount: count}, nil
}
