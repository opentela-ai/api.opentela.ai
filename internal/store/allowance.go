package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opentela-ai/api/internal/billing"
)

// Allowance registry (Phase 2, design §11). The registry mirrors on-chain SPL
// delegations; a grant is credited to the account through exactly-once ledger
// legs so account_credits stays a transactional projection of the ledger
// (§11.4.1). All writers take the (account, delegate) registry row FOR UPDATE,
// so the poller and the (future) settlement worker serialize cleanly.

// UpsertAllowance implements billing.BillingStore.
func (p *Postgres) UpsertAllowance(ctx context.Context, ch billing.AllowanceChange) (billing.AllowanceChangeResult, error) {
	if ch.AccountID == "" || ch.Delegate == "" {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: %w: missing account or delegate", billing.ErrConflict)
	}
	if ch.AllowanceRaw < 0 {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: %w: negative allowance", billing.ErrInvalid)
	}
	if ch.Ref == "" {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: %w: missing observation ref", billing.ErrInvalid)
	}
	now := ch.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The FK requires the credit row; EnsureAccountCredit semantics inline.
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_credits (account_id, updated_at) VALUES ($1, $2)
		 ON CONFLICT (account_id) DO NOTHING`, ch.AccountID, now); err != nil {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: ensure credit: %w", err)
	}

	var prev int64
	err = tx.QueryRow(ctx,
		`SELECT allowance_raw FROM account_allowances
		 WHERE account_id = $1 AND delegate = $2 FOR UPDATE`,
		ch.AccountID, ch.Delegate).Scan(&prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		prev = 0
	case err != nil:
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: lock: %w", err)
	}

	delta := ch.AllowanceRaw - prev
	result := billing.AllowanceChangeResult{PrevAllowanceRaw: prev, AppliedRaw: delta}

	var revokedAt *time.Time
	if ch.AllowanceRaw == 0 {
		revokedAt = &now
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_allowances (account_id, delegate, allowance_raw, approved_at, revoked_at)
		VALUES ($1, $2, $3::bigint, $4, $5)
		ON CONFLICT (account_id, delegate) DO UPDATE SET
		    allowance_raw = EXCLUDED.allowance_raw,
		    approved_at   = EXCLUDED.approved_at,
		    revoked_at    = CASE WHEN EXCLUDED.allowance_raw > 0 THEN NULL ELSE EXCLUDED.approved_at END`,
		ch.AccountID, ch.Delegate, ch.AllowanceRaw, now, revokedAt); err != nil {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: registry: %w", err)
	}

	if delta != 0 {
		// Mirror the delta into the credit projection. A revocation is
		// clamped at reserved_raw: open reservations must keep their backing
		// (the account_credits CHECK forbids reserved > credit); the amount
		// that could not be debited is reported as shortfall — platform
		// exposure that ReconcileAccount surfaces once the reservations
		// settle. The ledger leg always records the FULL delta so the ledger
		// remains the source of truth and the drift is detectable.
		leg := "grant"
		if delta < 0 {
			leg = "revoke"
		}
		// exactly-once: a replayed observation ref must not re-apply.
		tag, err := tx.Exec(ctx, `
			INSERT INTO credit_ledger
			    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
			VALUES ($1, $2, 'delegation', $3, $4, $5, $6)
			ON CONFLICT (ref, leg) DO NOTHING`,
			ch.AccountID, delta, leg, ch.Delegate, ch.Ref, now)
		if err != nil {
			return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: ledger: %w", err)
		}
		if tag.RowsAffected() > 0 {
			var creditRaw, reservedRaw int64
			if err := tx.QueryRow(ctx,
				`SELECT credit_raw, reserved_raw FROM account_credits WHERE account_id = $1 FOR UPDATE`,
				ch.AccountID).Scan(&creditRaw, &reservedRaw); err != nil {
				return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: lock credit: %w", err)
			}
			newCredit := creditRaw + delta
			if newCredit < reservedRaw {
				result.ShortfallRaw = reservedRaw - newCredit
				newCredit = reservedRaw
			}
			if _, err := tx.Exec(ctx,
				`UPDATE account_credits SET credit_raw = $2, updated_at = $3 WHERE account_id = $1`,
				ch.AccountID, newCredit, now); err != nil {
				return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: mirror: %w", err)
			}
		} else {
			// Replayed ref: the delta already landed once. Report no applied
			// delta so the caller does not double-count.
			result.AppliedRaw = 0
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return billing.AllowanceChangeResult{}, fmt.Errorf("store: upsert allowance: commit: %w", err)
	}
	return result, nil
}

// AccountAllowances implements billing.BillingStore: every registry row for
// the account (active and revoked), newest-approved first.
func (p *Postgres) AccountAllowances(ctx context.Context, accountID string) ([]billing.Allowance, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT account_id, delegate, allowance_raw, approved_at, revoked_at
		FROM account_allowances
		WHERE account_id = $1
		ORDER BY approved_at DESC, delegate`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: account allowances: %w", err)
	}
	return scanAllowances(rows)
}

// DelegateAllowances implements billing.BillingStore: the ACTIVE allowances
// naming `delegate`, largest first — the settlement worker's spend plan.
func (p *Postgres) DelegateAllowances(ctx context.Context, delegate string) ([]billing.Allowance, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT account_id, delegate, allowance_raw, approved_at, revoked_at
		FROM account_allowances
		WHERE delegate = $1 AND revoked_at IS NULL AND allowance_raw > 0
		ORDER BY allowance_raw DESC, account_id`, delegate)
	if err != nil {
		return nil, fmt.Errorf("store: delegate allowances: %w", err)
	}
	return scanAllowances(rows)
}

func scanAllowances(rows pgx.Rows) ([]billing.Allowance, error) {
	defer rows.Close()
	var out []billing.Allowance
	for rows.Next() {
		var a billing.Allowance
		if err := rows.Scan(&a.AccountID, &a.Delegate, &a.AllowanceRaw, &a.ApprovedAt, &a.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: scan allowance: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan allowances: %w", err)
	}
	return out, nil
}

// sumActiveAllowances returns Σ active allowance for the account. Called
// inside the reserve transaction while the credit row is locked.
func sumActiveAllowances(ctx context.Context, q pgxQuerier, accountID string) (int64, error) {
	var sum int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(allowance_raw), 0) FROM account_allowances
		WHERE account_id = $1 AND revoked_at IS NULL AND allowance_raw > 0`, accountID).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("store: sum allowances: %w", err)
	}
	return sum, nil
}
