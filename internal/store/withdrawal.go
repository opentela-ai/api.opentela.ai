package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opentela-ai/api/internal/billing"
)

// scanWithdrawal scans one withdrawal row from any pgx.Row/Rows. The
// signed-wire, signature, blockhash, error, and the per-state timestamps are
// nullable and scanned into pointers so a freshly reserved row (none of the
// downstream fields set yet) decodes cleanly.
func scanWithdrawal(r pgx.Row) (billing.Withdrawal, error) {
	var w billing.Withdrawal
	var signedWire, signature, blockhash, errMsg *string
	var lastValidBlockHeight *int64
	var blockhashExpiresAt, signedAt, broadcastAt, finalizedAt *time.Time
	if err := r.Scan(&w.ID, &w.AccountID, &w.IdempotencyKey, &w.DestinationWallet,
		&w.AmountRaw, &w.State, &signedWire, &signature, &blockhash, &lastValidBlockHeight,
		&blockhashExpiresAt, &errMsg, &w.ReservedAt, &signedAt, &broadcastAt, &finalizedAt); err != nil {
		return billing.Withdrawal{}, err
	}
	w.SignedWire = derefStr(signedWire)
	w.Signature = derefStr(signature)
	w.Blockhash = derefStr(blockhash)
	if lastValidBlockHeight != nil && *lastValidBlockHeight >= 0 {
		v := uint64(*lastValidBlockHeight)
		w.LastValidBlockHeight = &v
	}
	w.Error = derefStr(errMsg)
	w.BlockhashExpiresAt = blockhashExpiresAt
	w.SignedAt = signedAt
	w.BroadcastAt = broadcastAt
	w.FinalizedAt = finalizedAt
	return w, nil
}

// withdrawalColumns is the stable SELECT list shared by every read path, kept
// in scan order so scanWithdrawal never drifts.
const withdrawalColumns = `id, account_id, idempotency_key, destination_wallet,
    amount_raw, state, signed_wire, signature, blockhash, last_valid_block_height,
    blockhash_expires_at, error, reserved_at, signed_at, broadcast_at, finalized_at`

// lockWithdrawal selects one withdrawal FOR UPDATE within tx, returning
// billing.ErrNotFound when the row is absent.
func lockWithdrawal(ctx context.Context, tx pgx.Tx, id int64) (billing.Withdrawal, error) {
	w, err := scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Withdrawal{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: lock withdrawal: %w", err)
	}
	return w, nil
}

// requeryWithdrawal re-reads the committed row so the caller sees the
// downstream fields a state transition just wrote.
func requeryWithdrawal(ctx context.Context, q pgxQuerier, id int64) (billing.Withdrawal, error) {
	w, err := scanWithdrawal(q.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Withdrawal{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: requery withdrawal: %w", err)
	}
	return w, nil
}

// ReserveWithdrawal debits amount_raw from the account's available credit and
// persists a 'reserved' withdrawal. Idempotent on idempotency_key: a duplicate
// returns the existing row (ErrConflict only if a different account owns the
// key). The reserve writes a unique `withdraw` ledger leg (credit_raw -=
// amount) immediately, so the funds leave the off-chain projection and cannot
// be double-spent on inference while the withdrawal is in flight.
func (p *Postgres) ReserveWithdrawal(ctx context.Context, req billing.WithdrawalRequest, now time.Time) (billing.Withdrawal, error) {
	if req.AccountID == "" {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: account id required", billing.ErrInvalid)
	}
	if req.IdempotencyKey == "" {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: idempotency key required", billing.ErrInvalid)
	}
	if req.DestinationWallet == "" {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: destination wallet required", billing.ErrInvalid)
	}
	if req.AmountRaw <= 0 {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: amount must be positive", billing.ErrInvalid)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO withdrawals
		    (account_id, idempotency_key, destination_wallet, amount_raw, state, reserved_at)
		VALUES ($1, $2, $3, $4, 'reserved', $5)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		req.AccountID, req.IdempotencyKey, req.DestinationWallet, req.AmountRaw, now).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && !isUniqueViolation(err) {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: insert: %w", err)
	}

	if id == 0 {
		// ON CONFLICT DO NOTHING: fetch the pre-existing row. If it belongs to
		// a different account, the caller cannot use this key — refuse rather
		// than leak another account's withdrawal.
		w, ferr := scanWithdrawal(tx.QueryRow(ctx,
			`SELECT `+withdrawalColumns+` FROM withdrawals WHERE idempotency_key = $1`,
			req.IdempotencyKey))
		if ferr != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: existing: %w", ferr)
		}
		if w.AccountID != req.AccountID {
			return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: idempotency key in use by another account", billing.ErrConflict)
		}
		// Same account, same key: idempotent return of the original record.
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w", err)
		}
		return w, nil
	}

	// New row inserted. Lock the account, verify available credit covers the
	// amount, then debit credit_raw (NOT reserved_raw: this is a withdrawal,
	// not an inference reservation) and write the unique `withdraw` ledger
	// leg. If credit is insufficient, rollback discards the row and the
	// idempotency key — the client retries after topping up.
	creditRaw, reservedRaw, err := applyDelta(ctx, tx, req.AccountID, -req.AmountRaw, 0, now)
	if err != nil {
		// A CHECK violation (credit would go negative) surfaces here as the
		// underlying pg error; the available-credit guard below is the
		// friendly path. Either way this is insufficient credit.
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w", billing.ErrInsufficientCredit)
	}
	if creditRaw < 0 || reservedRaw > creditRaw {
		return billing.Withdrawal{}, billing.ErrInsufficientCredit
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
		VALUES ($1, $2, 'withdraw', 'withdraw', $3, $4, $5)`,
		req.AccountID, -req.AmountRaw, req.DestinationWallet, billing.WithdrawalRef(id), now); err != nil {
		if isUniqueViolation(err) {
			// Inconceivable for a fresh id, but treat defensively as a
			// duplicate — the money is already debited under this ref.
			return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w: withdraw leg exists", billing.ErrConflict)
		}
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: ledger: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: reserve withdrawal: %w", err)
	}
	return billing.Withdrawal{
		ID:                id,
		AccountID:         req.AccountID,
		IdempotencyKey:    req.IdempotencyKey,
		DestinationWallet: req.DestinationWallet,
		AmountRaw:         req.AmountRaw,
		State:             billing.WithdrawalReserved,
		ReservedAt:        now,
	}, nil
}

// Withdrawal returns one withdrawal by id for the owning account. A row that
// does not exist OR belongs to a different account returns ErrNotFound, so a
// leaked id cannot be used to probe another user's state.
func (p *Postgres) Withdrawal(ctx context.Context, accountID string, id int64) (billing.Withdrawal, error) {
	if accountID == "" || id <= 0 {
		return billing.Withdrawal{}, billing.ErrNotFound
	}
	w, err := scanWithdrawal(p.pool.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Withdrawal{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: withdrawal: %w", err)
	}
	if w.AccountID != accountID {
		return billing.Withdrawal{}, billing.ErrNotFound
	}
	return w, nil
}

// MarkWithdrawalSigned persists the signed wire transaction and its
// deterministic signature before broadcast, transitioning reserved -> signed.
// A row no longer in the 'reserved' state is returned unchanged so the caller
// re-reads the persisted wire (the winning replica's) instead of trusting its
// own computation.
func (p *Postgres) MarkWithdrawalSigned(ctx context.Context, id int64, signedWire, signature, blockhash string, lastValidBlockHeight uint64, blockhashExpiresAt, now time.Time) (billing.Withdrawal, error) {
	if signedWire == "" || signature == "" || blockhash == "" {
		return billing.Withdrawal{}, fmt.Errorf("store: mark signed: %w: signed wire, signature, and blockhash required", billing.ErrInvalid)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark signed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := lockWithdrawal(ctx, tx, id)
	if err != nil {
		return billing.Withdrawal{}, err
	}
	if w.State != billing.WithdrawalReserved {
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: mark signed: %w", err)
		}
		return w, nil
	}
	var expiry any
	if !blockhashExpiresAt.IsZero() {
		expiry = blockhashExpiresAt.UTC()
	}
	var lastValid any = int64(lastValidBlockHeight)
	if _, err := tx.Exec(ctx, `
		UPDATE withdrawals
		SET state = 'signed', signed_wire = $2, signature = $3,
		    blockhash = $4, last_valid_block_height = $5, blockhash_expires_at = $6, signed_at = $7
		WHERE id = $1`,
		id, signedWire, signature, blockhash, lastValid, expiry, now); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark signed: apply: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark signed: %w", err)
	}
	return requeryWithdrawal(ctx, p.pool, id)
}

// MarkWithdrawalBroadcast transitions signed -> broadcast, recording the first
// broadcast time. A row not in the 'signed' state is returned unchanged so a
// late broadcaster that lost the race to finalize/restore skips cleanly.
func (p *Postgres) MarkWithdrawalBroadcast(ctx context.Context, id int64, now time.Time) (billing.Withdrawal, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark broadcast: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := lockWithdrawal(ctx, tx, id)
	if err != nil {
		return billing.Withdrawal{}, err
	}
	if w.State != billing.WithdrawalSigned {
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: mark broadcast: %w", err)
		}
		return w, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawals SET state = 'broadcast', broadcast_at = $2 WHERE id = $1`,
		id, now); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark broadcast: apply: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: mark broadcast: %w", err)
	}
	return requeryWithdrawal(ctx, p.pool, id)
}

// FinalizeWithdrawal transitions a signed/broadcast withdrawal to finalized
// once the on-chain transaction is confirmed. No balance change: the debit was
// applied at reserve time. A row not in an open state is returned unchanged.
func (p *Postgres) FinalizeWithdrawal(ctx context.Context, id int64, now time.Time) (billing.Withdrawal, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: finalize withdrawal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := lockWithdrawal(ctx, tx, id)
	if err != nil {
		return billing.Withdrawal{}, err
	}
	if w.State != billing.WithdrawalSigned && w.State != billing.WithdrawalBroadcast {
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: finalize withdrawal: %w", err)
		}
		return w, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawals SET state = 'finalized', finalized_at = $2 WHERE id = $1`,
		id, now); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: finalize withdrawal: apply: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: finalize withdrawal: %w", err)
	}
	return requeryWithdrawal(ctx, p.pool, id)
}

// RestoreWithdrawal returns the reserved amount to the account (credit_raw +=
// amount, unique `restore` ledger leg) and transitions the row to 'restored'.
// Called by the worker once blockhash expiry proves the transaction cannot
// land, or on an unrecoverable construction/broadcast error before
// finalization. A row not in an open state (reserved/signed/broadcast) is
// returned unchanged, so a concurrent finalize beats a late restore cleanly.
func (p *Postgres) RestoreWithdrawal(ctx context.Context, id int64, reason string, now time.Time) (billing.Withdrawal, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := lockWithdrawal(ctx, tx, id)
	if err != nil {
		return billing.Withdrawal{}, err
	}
	if w.State != billing.WithdrawalReserved && w.State != billing.WithdrawalSigned && w.State != billing.WithdrawalBroadcast {
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: %w", err)
		}
		return w, nil
	}

	if _, _, err := applyDelta(ctx, tx, w.AccountID, w.AmountRaw, 0, now); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: credit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
		VALUES ($1, $2, 'withdraw', 'restore', $3, $4, $5)`,
		w.AccountID, w.AmountRaw, w.DestinationWallet, billing.WithdrawalRef(id), now); err != nil {
		if isUniqueViolation(err) {
			// Already restored by a concurrent path; fall through to mark the
			// row restored without re-inserting the ledger leg.
		} else {
			return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: ledger: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawals SET state = 'restored', error = $2 WHERE id = $1`,
		id, reason); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: mark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: restore withdrawal: %w", err)
	}
	return requeryWithdrawal(ctx, p.pool, id)
}

// FailWithdrawal records an error and transitions the row to 'failed' WITHOUT
// restoring credit, parking it for investigation while the blockhash timer
// handles restoration separately (or an operator restores manually). A row not
// in an open state is returned unchanged.
func (p *Postgres) FailWithdrawal(ctx context.Context, id int64, reason string, now time.Time) (billing.Withdrawal, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: fail withdrawal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	w, err := lockWithdrawal(ctx, tx, id)
	if err != nil {
		return billing.Withdrawal{}, err
	}
	if w.State != billing.WithdrawalReserved && w.State != billing.WithdrawalSigned && w.State != billing.WithdrawalBroadcast {
		if err := tx.Commit(ctx); err != nil {
			return billing.Withdrawal{}, fmt.Errorf("store: fail withdrawal: %w", err)
		}
		return w, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawals SET state = 'failed', error = $2 WHERE id = $1`,
		id, reason); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: fail withdrawal: apply: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Withdrawal{}, fmt.Errorf("store: fail withdrawal: %w", err)
	}
	return requeryWithdrawal(ctx, p.pool, id)
}

// SweepWithdrawalsDue returns withdrawals in an open state (reserved/signed/
// broadcast), oldest first, locked with FOR UPDATE SKIP LOCKED so concurrent
// replicas claim disjoint sets. The lock is held only until the caller's
// transaction ends; the worker performs at most one fast RPC per row and
// applies the transition with a state guard, so a replica that dies mid-work
// releases the row on abort and another claims it next sweep.
func (p *Postgres) SweepWithdrawalsDue(ctx context.Context, limit int) ([]billing.Withdrawal, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: sweep withdrawals: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT `+withdrawalColumns+`
		FROM withdrawals
		WHERE state IN ('reserved', 'signed', 'broadcast')
		ORDER BY reserved_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: sweep withdrawals: %w", err)
	}
	var out []billing.Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: sweep withdrawals: scan: %w", err)
		}
		out = append(out, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sweep withdrawals: rows: %w", err)
	}
	// Release the row locks immediately: the worker re-locks each row in its
	// own short transition transaction. Holding across the RPC would serialize
	// the sweep on slow nodes; releasing keeps recovery responsive while the
	// per-row state guard still prevents duplicate work.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: sweep withdrawals: %w", err)
	}
	return out, nil
}

// ListWithdrawals pages over an account's withdrawals newest-first. A nil
// cursor starts from the newest row.
func (p *Postgres) ListWithdrawals(ctx context.Context, accountID string, cursor *billing.WithdrawalCursor, limit int) ([]billing.Withdrawal, *billing.WithdrawalCursor, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = p.pool.Query(ctx, `
			SELECT `+withdrawalColumns+`
			FROM withdrawals
			WHERE account_id = $1
			ORDER BY reserved_at DESC, id DESC
			LIMIT $2`, accountID, limit)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT `+withdrawalColumns+`
			FROM withdrawals
			WHERE account_id = $1 AND (reserved_at, id) < ($2, $3)
			ORDER BY reserved_at DESC, id DESC
			LIMIT $4`, accountID, cursor.ReservedAt, cursor.ID, limit)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list withdrawals: %w", err)
	}
	defer rows.Close()

	var out []billing.Withdrawal
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("store: list withdrawals: scan: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list withdrawals: rows: %w", err)
	}
	var next *billing.WithdrawalCursor
	if len(out) == limit {
		last := out[len(out)-1]
		next = &billing.WithdrawalCursor{ReservedAt: last.ReservedAt, ID: last.ID}
	}
	return out, next, nil
}
