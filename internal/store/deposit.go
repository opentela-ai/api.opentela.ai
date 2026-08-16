package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/opentela-ai/api/internal/billing"
)

// depositRef builds the stable ledger reference for one deposit instruction:
// "sol:<signature>:<instructionIndex>". The (ref, leg='deposit') pair is
// globally unique under credit_ledger, so retried crediting is a no-op.
func depositRef(signature string, instructionIndex int) string {
	return "sol:" + signature + ":" + strconv.Itoa(instructionIndex)
}

// InsertDepositEvent persists one inbound SPL transfer instruction into the
// treasury ATA BEFORE any credit is applied, so a crash mid-watch can never
// lose or double-count a deposit. It is idempotent via the
// (transaction_signature, instruction_index) primary key: a re-scan returns
// inserted=false and the pre-existing row is left untouched. The event is
// always recorded with assignment_state='unassigned' (the default); attribution
// is resolved separately by ApplyDeposit / ReconcileDepositsForWallet against
// linked wallets — never from the chain or a memo.
func (p *Postgres) InsertDepositEvent(ctx context.Context, ev billing.DepositEvent) (bool, error) {
	if ev.TransactionSignature == "" {
		return false, fmt.Errorf("store: insert deposit: %w", billing.ErrInvalid)
	}
	if ev.AmountRaw <= 0 {
		return false, fmt.Errorf("store: insert deposit: %w: amount must be positive", billing.ErrInvalid)
	}
	if ev.FromWallet == "" {
		return false, fmt.Errorf("store: insert deposit: %w: from_wallet required", billing.ErrInvalid)
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO deposit_events
		    (transaction_signature, instruction_index, slot, from_wallet, amount_raw, assignment_state, seen_at)
		VALUES ($1, $2, $3, $4, $5, 'unassigned', COALESCE($6, now()))
		ON CONFLICT (transaction_signature, instruction_index) DO NOTHING`,
		ev.TransactionSignature, ev.InstructionIndex, ev.Slot, ev.FromWallet,
		ev.AmountRaw, nullableTime(ev.SeenAt))
	if err != nil {
		return false, fmt.Errorf("store: insert deposit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DepositCursor returns the persisted high-water signature for treasuryATA.
// The empty string means no cursor has been established yet.
func (p *Postgres) DepositCursor(ctx context.Context, treasuryATA string) (string, error) {
	if treasuryATA == "" {
		return "", fmt.Errorf("store: deposit cursor: %w: treasury ATA required", billing.ErrInvalid)
	}
	var signature string
	err := p.pool.QueryRow(ctx, `
		SELECT last_signature
		FROM deposit_cursors
		WHERE treasury_ata = $1`, treasuryATA).Scan(&signature)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: deposit cursor: %w", err)
	}
	return signature, nil
}

// AdvanceDepositCursor compare-and-sets the persisted high-water signature for
// treasuryATA. It advances only when the currently stored signature still
// equals fromSignature, or no row exists and fromSignature is empty.
func (p *Postgres) AdvanceDepositCursor(ctx context.Context, treasuryATA, fromSignature, toSignature string) (bool, error) {
	if treasuryATA == "" || toSignature == "" {
		return false, fmt.Errorf("store: advance deposit cursor: %w: treasury ATA and to-signature required", billing.ErrInvalid)
	}
	if fromSignature == toSignature {
		return true, nil
	}
	if fromSignature == "" {
		tag, err := p.pool.Exec(ctx, `
			INSERT INTO deposit_cursors (treasury_ata, last_signature, updated_at)
			VALUES ($1, $2, now())
			ON CONFLICT (treasury_ata) DO NOTHING`, treasuryATA, toSignature)
		if err != nil {
			return false, fmt.Errorf("store: advance deposit cursor: %w", err)
		}
		return tag.RowsAffected() > 0, nil
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE deposit_cursors
		SET last_signature = $3, updated_at = now()
		WHERE treasury_ata = $1 AND last_signature = $2`, treasuryATA, fromSignature, toSignature)
	if err != nil {
		return false, fmt.Errorf("store: advance deposit cursor: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ApplyDeposit credits one persisted deposit event to accountID in a single
// transaction: it bumps account_credits.credit_raw, inserts a unique
// credit_ledger leg (source='deposit'), and marks the event assigned +
// credited_at. Idempotent — an already-assigned event for the same account is
// a no-op; an already-assigned event for a different account returns
// ErrConflict (a wallet cannot be linked to two accounts, so this guards only
// against a stale re-link race). A 'skipped' event is never credited.
func (p *Postgres) ApplyDeposit(ctx context.Context, signature string, instructionIndex int, accountID string, now time.Time) (billing.DepositEvent, error) {
	if accountID == "" {
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w: account id required", billing.ErrInvalid)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ev, err := lockDepositEvent(ctx, tx, signature, instructionIndex)
	if err != nil {
		return billing.DepositEvent{}, err
	}
	switch ev.AssignmentState {
	case "assigned":
		// Already credited. The wallet is uniquely owned, so the account must
		// match; a mismatch means the wallet was unlinked and re-linked to a
		// different account after a credit — refuse to double-credit.
		if ev.AssignedAccountID == nil || *ev.AssignedAccountID != accountID {
			return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w: deposit already credited to another account", billing.ErrConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", err)
		}
		return ev, nil
	case "skipped":
		// Admin-marked skip: never credit.
		if err := tx.Commit(ctx); err != nil {
			return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", err)
		}
		return ev, nil
	case "unassigned":
		// Fall through and credit.
	default:
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w: unexpected state %q", billing.ErrInvalid, ev.AssignmentState)
	}

	if err := creditDepositLocked(ctx, tx, &ev, accountID, now); err != nil {
		return billing.DepositEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", err)
	}
	return ev, nil
}

// creditDepositLocked credits ev to accountID within the caller's transaction.
// The caller must hold FOR UPDATE on the deposit_events row. It is idempotent
// at the ledger level: a UNIQUE(ref, leg) violation means the credit already
// landed (e.g. a concurrent reconciler won the race before the row lock), in
// which case the event is marked assigned without re-inserting.
func creditDepositLocked(ctx context.Context, tx pgx.Tx, ev *billing.DepositEvent, accountID string, now time.Time) error {
	if _, _, err := applyDelta(ctx, tx, accountID, ev.AmountRaw, 0, now); err != nil {
		return fmt.Errorf("store: credit deposit: %w", err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
		VALUES ($1, $2, 'deposit', 'deposit', $3, $4, $5)`,
		accountID, ev.AmountRaw, ev.FromWallet, depositRef(ev.TransactionSignature, ev.InstructionIndex), now)
	if err != nil {
		if isUniqueViolation(err) {
			// Already credited by a concurrent path; fall through to mark the
			// event assigned so it is not retried.
		} else {
			return fmt.Errorf("store: credit deposit: ledger: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE deposit_events
		SET assignment_state = 'assigned', assigned_account_id = $3, credited_at = $4
		WHERE transaction_signature = $1 AND instruction_index = $2`,
		ev.TransactionSignature, ev.InstructionIndex, accountID, now); err != nil {
		return fmt.Errorf("store: credit deposit: mark: %w", err)
	}
	ev.AssignmentState = "assigned"
	acct := accountID
	ev.AssignedAccountID = &acct
	ev.CreditedAt = &now
	return nil
}

// ReconcileDepositsForWallet credits every not-yet-credited deposit from
// wallet to accountID, returning the count applied. It is called after a
// wallet is linked so transfers that arrived before linkage are not lost, and
// is idempotent: re-running finds no unassigned rows for the wallet.
func (p *Postgres) ReconcileDepositsForWallet(ctx context.Context, wallet, accountID string, now time.Time) (int, error) {
	if wallet == "" || accountID == "" {
		return 0, fmt.Errorf("store: reconcile deposits: %w: wallet and account required", billing.ErrInvalid)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("store: reconcile deposits: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT transaction_signature, instruction_index, slot, from_wallet, amount_raw,
		       assigned_account_id, assignment_state, credited_at, seen_at
		FROM deposit_events
		WHERE from_wallet = $1 AND assignment_state = 'unassigned' AND credited_at IS NULL
		ORDER BY seen_at, transaction_signature, instruction_index
		FOR UPDATE`, wallet)
	if err != nil {
		return 0, fmt.Errorf("store: reconcile deposits: %w", err)
	}
	var events []billing.DepositEvent
	for rows.Next() {
		ev, err := scanDepositEvent(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: reconcile deposits: rows: %w", err)
	}

	// Lock the account once for the batch so concurrent deposits to the same
	// account serialize cleanly.
	if len(events) > 0 {
		if _, _, err := applyDelta(ctx, tx, accountID, 0, 0, now); err != nil {
			return 0, fmt.Errorf("store: reconcile deposits: lock account: %w", err)
		}
	}
	for i := range events {
		if err := creditDepositLocked(ctx, tx, &events[i], accountID, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: reconcile deposits: %w", err)
	}
	return len(events), nil
}

// ListDepositEvents pages over a account's credited deposits, newest-first.
// The cursor is the (seen_at, transaction_signature, instruction_index) of the
// last entry returned; a nil cursor starts from the newest row.
func (p *Postgres) ListDepositEvents(ctx context.Context, accountID string, cursor *billing.DepositCursor, limit int) ([]billing.DepositEvent, *billing.DepositCursor, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = p.pool.Query(ctx, `
			SELECT transaction_signature, instruction_index, slot, from_wallet, amount_raw,
			       assigned_account_id, assignment_state, credited_at, seen_at
			FROM deposit_events
			WHERE assigned_account_id = $1 AND assignment_state = 'assigned'
			ORDER BY seen_at DESC, transaction_signature DESC, instruction_index DESC
			LIMIT $2`, accountID, limit)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT transaction_signature, instruction_index, slot, from_wallet, amount_raw,
			       assigned_account_id, assignment_state, credited_at, seen_at
			FROM deposit_events
			WHERE assigned_account_id = $1 AND assignment_state = 'assigned'
			  AND (seen_at, transaction_signature, instruction_index)
			      < ($2, $3, $4)
			ORDER BY seen_at DESC, transaction_signature DESC, instruction_index DESC
			LIMIT $5`, accountID, cursor.SeenAt, cursor.TransactionSignature, cursor.InstructionIndex, limit)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list deposits: %w", err)
	}
	defer rows.Close()

	var out []billing.DepositEvent
	for rows.Next() {
		ev, err := scanDepositEvent(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list deposits: rows: %w", err)
	}
	var next *billing.DepositCursor
	if len(out) == limit {
		last := out[len(out)-1]
		next = &billing.DepositCursor{
			SeenAt:               last.SeenAt,
			TransactionSignature: last.TransactionSignature,
			InstructionIndex:     last.InstructionIndex,
		}
	}
	return out, next, nil
}

// lockDepositEvent selects and FOR UPDATE locks one deposit event within tx.
func lockDepositEvent(ctx context.Context, tx pgx.Tx, signature string, instructionIndex int) (billing.DepositEvent, error) {
	row := tx.QueryRow(ctx, `
		SELECT transaction_signature, instruction_index, slot, from_wallet, amount_raw,
		       assigned_account_id, assignment_state, credited_at, seen_at
		FROM deposit_events
		WHERE transaction_signature = $1 AND instruction_index = $2
		FOR UPDATE`, signature, instructionIndex)
	ev, err := scanDepositEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", billing.ErrNotFound)
	}
	if err != nil {
		return billing.DepositEvent{}, fmt.Errorf("store: apply deposit: %w", err)
	}
	return ev, nil
}

// scanDepositEvent scans a deposit row from any pgx.Row/Rows. Nullable columns
// (assigned_account_id, credited_at) are scanned into pointers.
func scanDepositEvent(r pgx.Row) (billing.DepositEvent, error) {
	var ev billing.DepositEvent
	var assigned *string
	var credited *time.Time
	if err := r.Scan(&ev.TransactionSignature, &ev.InstructionIndex, &ev.Slot,
		&ev.FromWallet, &ev.AmountRaw, &assigned, &ev.AssignmentState,
		&credited, &ev.SeenAt); err != nil {
		return billing.DepositEvent{}, err
	}
	ev.AssignedAccountID = assigned
	ev.CreditedAt = credited
	return ev, nil
}

// nullableTime returns nil for the zero time so the column falls back to its
// DEFAULT (now()).
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
