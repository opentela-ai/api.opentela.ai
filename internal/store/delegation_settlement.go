package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/opentela-ai/api/internal/billing"
	"github.com/opentela-ai/api/internal/solana"
)

// Delegation settlement (design §11.5): the worker's durable state machine.
//
// Rail decision: a usage charge is delegation-settled iff its BUYER holds an
// active allowance for the settlement authority AT BATCH-CREATION TIME (the
// worker's snapshot). Legs for buyers without one are skipped — the cursor
// advances past them and the deposit rail backs their settlement, exactly as
// before Phase 2. A batch that later fails on-chain restores the buyer's
// registry allowance (the on-chain authority is still there) and leaves the
// legs recorded; §11.5.5 reconciliation owns the residual exposure.
//
// Batching: one settlement row per (buyer, destination) pair per pass —
// earn legs net per seller, fee legs net to the treasury — so each row is
// exactly one transfer_checked instruction. batch_ref is deterministic from
// the settled leg ids (globally unique per ledger row), so a re-claim after
// a crash resolves to the SAME ref and ON CONFLICT DO NOTHING keeps the
// exactly-once guarantee; the leg anchor table's UNIQUE(ledger_leg_id) is
// the hard backstop.

const settlementColumns = `
    id, batch_ref, buyer_account, delegate, source_ata, destination_wallet,
    destination_ata, amount_raw, state, signed_wire, tx_signature, blockhash,
    last_valid_block_height, blockhash_expires_at, fail_reason, created_at`

// SettlementClaimParams bounds one claim pass.
type SettlementClaimParams struct {
	Delegate       string // settlement authority (delegate) pubkey
	Mint           string // OTELA mint, for destination ATA derivation
	TokenProgram   string
	TreasuryWallet string // fee destination; when empty, fees are not settled on-chain
	LegLimit       int    // max earn legs examined per pass (clamped 1..1000)
}

// unsettledLeg is one ledger row awaiting on-chain replay.
type unsettledLeg struct {
	ID        int64
	AccountID string // the leg owner: seller for earn, treasury for fee
	Counter   string // the other side: buyer for both
	DeltaRaw  int64
	Ref       string
}

// settlementBatch is one transfer to build.
type settlementBatch struct {
	Buyer  string
	Source string // buyer's ATA
	Dest   string // destination WALLET (treasury for fee batches)
	Amount int64
	LegIDs []int64 // credit_ledger ids anchored to this batch
	IsFee  bool
}

// ClaimDelegationSettlements creates the next wave of settlement batches
// from unsettled legs and returns the newly created pending rows. One
// transaction does everything — batch rows, leg anchors, allowance
// consumption, cursor advance — so a crash mid-way leaves no half-claimed
// batch and no consumed-but-unrecorded allowance.
func (p *Postgres) ClaimDelegationSettlements(ctx context.Context, params SettlementClaimParams, now time.Time) ([]billing.DelegationSettlement, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if params.Delegate == "" || params.Mint == "" || params.TokenProgram == "" {
		return nil, fmt.Errorf("store: claim settlements: %w: delegate, mint, token program required", billing.ErrInvalid)
	}
	limit := params.LegLimit
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: claim settlements: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Cursor: last leg id examined for this delegate.
	var lastLeg int64
	err = tx.QueryRow(ctx, `
		SELECT last_leg_id FROM delegation_settlement_cursors WHERE delegate = $1`,
		params.Delegate).Scan(&lastLeg)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: claim settlements: cursor: %w", err)
	}

	// 2. Unsettled earn legs, oldest first, whose buyer currently holds an
	// active allowance for this delegate. Legs without one fall out of the
	// window and the cursor advances past them (deposit rail).
	legRows, err := tx.Query(ctx, `
		SELECT l.id, l.account_id, l.counterparty, l.delta_raw, l.ref
		FROM credit_ledger l
		JOIN account_allowances a
		  ON a.account_id = l.counterparty AND a.delegate = $1
		 AND a.revoked_at IS NULL AND a.allowance_raw > 0
		WHERE l.source = 'earn' AND l.leg = 'seller' AND l.id > $2
		ORDER BY l.id
		LIMIT $3`, params.Delegate, lastLeg, limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim settlements: legs: %w", err)
	}
	var legs []unsettledLeg
	for legRows.Next() {
		var l unsettledLeg
		if err := legRows.Scan(&l.ID, &l.AccountID, &l.Counter, &l.DeltaRaw, &l.Ref); err != nil {
			legRows.Close()
			return nil, fmt.Errorf("store: claim settlements: leg scan: %w", err)
		}
		legs = append(legs, l)
	}
	legRows.Close()
	if err := legRows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim settlements: legs: %w", err)
	}
	if len(legs) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("store: claim settlements: %w", err)
		}
		return nil, nil
	}
	newCursor := legs[len(legs)-1].ID

	// 3. Fee legs paired by ref: the platform take moves on-chain in the
	// same pass, so the buyer's outflow equals exactly what their credit
	// was debited (seller share + fee).
	refs := make([]string, 0, len(legs))
	for _, l := range legs {
		refs = append(refs, l.Ref)
	}
	var feeLegs []unsettledLeg
	feeRows, err := tx.Query(ctx, `
		SELECT id, account_id, counterparty, delta_raw, ref
		FROM credit_ledger
		WHERE source = 'fee' AND leg = 'fee' AND ref = ANY($1)`, refs)
	if err != nil {
		return nil, fmt.Errorf("store: claim settlements: fee legs: %w", err)
	}
	for feeRows.Next() {
		var l unsettledLeg
		if err := feeRows.Scan(&l.ID, &l.AccountID, &l.Counter, &l.DeltaRaw, &l.Ref); err != nil {
			feeRows.Close()
			return nil, fmt.Errorf("store: claim settlements: fee scan: %w", err)
		}
		feeLegs = append(feeLegs, l)
	}
	feeRows.Close()
	if err := feeRows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim settlements: fee legs: %w", err)
	}

	// 4. Resolve destinations (seller account → primary wallet; fee →
	// treasury) and the buyer's source ATA. A buyer with any unresolvable
	// wallet is skipped: every transfer needs both ends derivable, and
	// resolving at claim time keeps the signed state machine wallet-free.
	sellerWallet := map[string]string{}
	resolve := func(accountID string) (string, error) {
		if w, ok := sellerWallet[accountID]; ok {
			return w, nil
		}
		w, ok, err := p.PrimaryWalletForAccount(ctx, accountID)
		if err != nil {
			return "", err
		}
		sellerWallet[accountID] = w // "" when unlinked
		if !ok {
			return "", nil
		}
		return w, nil
	}

	type buyerPlan struct {
		total     int64
		batches   []settlementBatch
		skippable bool
	}
	plans := map[string]*buyerPlan{}
	var order []string
	add := func(buyer string) *buyerPlan {
		pl, ok := plans[buyer]
		if !ok {
			pl = &buyerPlan{}
			plans[buyer] = pl
			order = append(order, buyer)
		}
		return pl
	}

	netInto := func(buyer, dest string, amount int64, legID int64, isFee bool, source ...string) {
		pl := add(buyer)
		for i := range pl.batches {
			if pl.batches[i].Dest == dest && pl.batches[i].IsFee == isFee {
				pl.batches[i].Amount += amount
				pl.batches[i].LegIDs = append(pl.batches[i].LegIDs, legID)
				pl.total += amount
				return
			}
		}
		src := ""
		if len(source) > 0 {
			src = source[0]
		}
		pl.batches = append(pl.batches, settlementBatch{
			Buyer: buyer, Source: src, Dest: dest, Amount: amount, LegIDs: []int64{legID}, IsFee: isFee,
		})
		pl.total += amount
	}

	var buyerSource string
	for _, l := range legs {
		buyer := l.Counter
		bw, _, err := p.PrimaryWalletForAccount(ctx, buyer)
		if err != nil {
			return nil, fmt.Errorf("store: claim settlements: buyer wallet: %w", err)
		}
		if bw == "" {
			add(buyer).skippable = true // buyer unlinked: cannot source funds
			continue
		}
		src, err := destinationATA(bw, params.Mint, params.TokenProgram)
		if err != nil {
			return nil, fmt.Errorf("store: claim settlements: buyer ata: %w", err)
		}
		buyerSource = src
		w, err := resolve(l.AccountID)
		if err != nil {
			return nil, fmt.Errorf("store: claim settlements: seller wallet: %w", err)
		}
		if w == "" {
			add(buyer).skippable = true // seller unlinked: buyer skipped
			continue
		}
		netInto(buyer, w, l.DeltaRaw, l.ID, false, src)
	}
	if params.TreasuryWallet != "" {
		for _, l := range feeLegs {
			if _, earn := plans[l.Counter]; !earn {
				continue // fee for a charge not in this pass
			}
			netInto(l.Counter, params.TreasuryWallet, l.DeltaRaw, l.ID, true, buyerSource)
		}
	}

	// 5. Insert: per buyer, lock+check the allowance, insert batch rows and
	// leg anchors, then consume the allowance — all inside this tx.
	var created []billing.DelegationSettlement
	for _, buyer := range order {
		pl := plans[buyer]
		if pl.skippable {
			continue
		}
		ok, err := lockAndConsumeAllowanceTx(ctx, tx, buyer, params.Delegate, pl.total)
		if err != nil {
			return nil, fmt.Errorf("store: claim settlements: consume: %w", err)
		}
		if !ok {
			continue // insufficient authority at snapshot: buyer skipped
		}
		for _, b := range pl.batches {
			ata, err := destinationATA(b.Dest, params.Mint, params.TokenProgram)
			if err != nil {
				return nil, fmt.Errorf("store: claim settlements: dest ata: %w", err)
			}
			ref := settlementRef(params.Delegate, b)
			var id int64
			err = tx.QueryRow(ctx, `
				INSERT INTO delegation_settlements
				    (batch_ref, buyer_account, delegate, source_ata, destination_wallet,
				     destination_ata, amount_raw, state, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'pending',$8,$8)
				ON CONFLICT (batch_ref) DO NOTHING
				RETURNING id`,
				ref, b.Buyer, params.Delegate, b.Source, b.Dest, ata, b.Amount, now).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				continue // a concurrent worker claimed this batch already
			}
			if err != nil {
				return nil, fmt.Errorf("store: claim settlements: insert: %w", err)
			}
			for _, legID := range b.LegIDs {
				if _, err := tx.Exec(ctx, `
					INSERT INTO delegation_settlement_legs (settlement_id, ledger_leg_id)
					VALUES ($1,$2)
					ON CONFLICT (ledger_leg_id) DO NOTHING`, id, legID); err != nil {
					return nil, fmt.Errorf("store: claim settlements: leg anchor: %w", err)
				}
			}
			created = append(created, billing.DelegationSettlement{
				ID: id, BatchRef: ref, BuyerAccount: b.Buyer,
				Delegate: params.Delegate, SourceATA: b.Source,
				DestinationWallet: b.Dest, DestinationATA: ata,
				AmountRaw: b.Amount, State: billing.SettlementPending,
			})
		}
	}

	// 6. Advance the cursor past every leg examined this pass — including
	// skipped ones (their rail decision is final for this worker).
	if _, err := tx.Exec(ctx, `
		INSERT INTO delegation_settlement_cursors (delegate, last_leg_id, updated_at)
		VALUES ($1,$2,$3)
		ON CONFLICT (delegate) DO UPDATE SET last_leg_id = $2, updated_at = $3`,
		params.Delegate, newCursor, now); err != nil {
		return nil, fmt.Errorf("store: claim settlements: cursor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: claim settlements: %w", err)
	}
	return created, nil
}

// settlementRef derives the deterministic batch_ref from the settled leg
// ids: "dset:<delegate8>:<buyer8>:<dest8>[:fee]:<minLeg>-<maxLeg>". Leg ids
// are globally unique per ledger row, so the ref is unique per event and
// stable across re-claims.
func settlementRef(delegate string, b settlementBatch) string {
	min, max := b.LegIDs[0], b.LegIDs[0]
	for _, id := range b.LegIDs {
		if id < min {
			min = id
		}
		if id > max {
			max = id
		}
	}
	kind := "xfer"
	if b.IsFee {
		kind = "fee"
	}
	return fmt.Sprintf("dset:%s:%s:%s:%s:%d-%d",
		short58(delegate), short58(b.Buyer), short58(b.Dest), kind, min, max)
}

func short58(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func destinationATA(wallet, mint, tokenProgram string) (string, error) {
	w, err := solana.DecodeBase58(wallet, solana.PublicKeyBytes)
	if err != nil {
		return "", fmt.Errorf("destination wallet: %w", err)
	}
	m, err := solana.DecodeBase58(mint, solana.PublicKeyBytes)
	if err != nil {
		return "", fmt.Errorf("mint: %w", err)
	}
	tp, err := solana.DecodeBase58(tokenProgram, solana.PublicKeyBytes)
	if err != nil {
		return "", fmt.Errorf("token program: %w", err)
	}
	ata, err := solana.AssociatedTokenAddress(w, m, tp)
	if err != nil {
		return "", err
	}
	return solana.EncodeBase58(ata), nil
}

// lockAndConsumeAllowanceTx locks the buyer's active allowance row and, when
// it covers amount, decrements it. Returns false (consuming nothing) when
// there is no active row or the authority is insufficient. Runs inside the
// caller's transaction: the consumption lands atomically with the batch
// rows, which is what keeps registry == chain − in-flight for the poller.
func lockAndConsumeAllowanceTx(ctx context.Context, tx pgx.Tx, accountID, delegate string, amount int64) (bool, error) {
	if amount <= 0 {
		return false, fmt.Errorf("store: consume allowance: %w: non-positive amount", billing.ErrInvalid)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE account_allowances
		SET allowance_raw = allowance_raw - $3
		WHERE account_id = $1 AND delegate = $2 AND revoked_at IS NULL
		  AND allowance_raw >= $3`,
		accountID, delegate, amount)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SweepDelegationSettlements returns the in-flight settlements
// (pending/signed/broadcast) for the worker to advance, claiming them with
// FOR UPDATE SKIP LOCKED so concurrent replicas never advance the same row.
func (p *Postgres) SweepDelegationSettlements(ctx context.Context, limit int) ([]billing.DelegationSettlement, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := p.pool.Query(ctx, `
		SELECT `+settlementColumns+`
		FROM delegation_settlements
		WHERE state IN ('pending', 'signed', 'broadcast')
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: sweep settlements: %w", err)
	}
	defer rows.Close()
	var out []billing.DelegationSettlement
	for rows.Next() {
		s, err := scanSettlement(rows)
		if err != nil {
			return nil, fmt.Errorf("store: sweep settlements: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sweep settlements: %w", err)
	}
	return out, nil
}

func scanSettlement(row pgx.Row) (billing.DelegationSettlement, error) {
	var s billing.DelegationSettlement
	var state string
	var failReason *string
	// signed_wire / signature / blockhash are NULLable (pending rows) —
	// scan via pointers so a pending restore doesn't crash.
	var signedWire, signature, blockhash *string
	err := row.Scan(&s.ID, &s.BatchRef, &s.BuyerAccount, &s.Delegate, &s.SourceATA, &s.DestinationWallet, &s.DestinationATA,
		&s.AmountRaw, &state, &signedWire, &signature, &blockhash,
		&s.LastValidBlockHeight, &s.BlockhashExpiresAt, &failReason, &s.CreatedAt)
	if err != nil {
		return s, err
	}
	if signedWire != nil {
		s.SignedWire = *signedWire
	}
	if signature != nil {
		s.Signature = *signature
	}
	if blockhash != nil {
		s.Blockhash = *blockhash
	}
	if failReason != nil {
		s.Error = *failReason
	}
	s.State = billing.DelegationSettlementState(state)
	return s, nil
}

// MarkSettlementSigned persists the signed wire (pending → signed). The
// state guard makes a lost race (another replica signed first) a clean
// no-op: only the winning wire is ever broadcast.
func (p *Postgres) MarkSettlementSigned(ctx context.Context, id int64, wireB64, signature, blockhash string, lastValidBlockHeight uint64, expiresAt, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE delegation_settlements
		SET signed_wire = $2, tx_signature = $3, blockhash = $4,
		    last_valid_block_height = $5, blockhash_expires_at = $6,
		    state = 'signed', updated_at = $7
		WHERE id = $1 AND state = 'pending'`,
		id, wireB64, signature, blockhash, lastValidBlockHeight, expiresAt, now)
	if err != nil {
		return false, fmt.Errorf("store: mark settlement signed: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkSettlementBroadcast advances signed → broadcast.
func (p *Postgres) MarkSettlementBroadcast(ctx context.Context, id int64, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE delegation_settlements SET state = 'broadcast', updated_at = $2
		WHERE id = $1 AND state = 'signed'`, id, now)
	if err != nil {
		return false, fmt.Errorf("store: mark settlement broadcast: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// FinalizeSettlement advances broadcast → finalized. Terminal; the legs
// stay anchored forever as the on-chain audit trail.
func (p *Postgres) FinalizeSettlement(ctx context.Context, id int64, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE delegation_settlements SET state = 'finalized', updated_at = $2
		WHERE id = $1 AND state = 'broadcast'`, id, now)
	if err != nil {
		return false, fmt.Errorf("store: finalize settlement: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RestoreSettlement refunds the buyer's registry allowance (the on-chain
// authority is still there — consumption was premature) and marks the batch
// restored with the reason. The legs stay anchored: this batch will never
// re-transfer them; reconciliation reports the residual.
func (p *Postgres) RestoreSettlement(ctx context.Context, id int64, reason string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: restore settlement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var s billing.DelegationSettlement
	var failReason *string
	var signedWire, signature, blockhash *string
	err = tx.QueryRow(ctx, `
		SELECT `+settlementColumns+`
		FROM delegation_settlements WHERE id = $1 FOR UPDATE`, id).Scan(
		&s.ID, &s.BatchRef, &s.BuyerAccount, &s.Delegate, &s.SourceATA, &s.DestinationWallet, &s.DestinationATA,
		&s.AmountRaw, &s.State, &signedWire, &signature, &blockhash,
		&s.LastValidBlockHeight, &s.BlockhashExpiresAt, &failReason, &s.CreatedAt)
	if signedWire != nil {
		s.SignedWire = *signedWire
	}
	if signature != nil {
		s.Signature = *signature
	}
	if blockhash != nil {
		s.Blockhash = *blockhash
	}
	if failReason != nil {
		s.Error = *failReason
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: restore settlement: %w", err)
	}
	st := s.State
	if st != billing.SettlementPending && st != billing.SettlementSigned && st != billing.SettlementBroadcast {
		return false, nil // terminal already
	}
	// Refund the registry allowance the claim consumed (the on-chain
	// authority is still there). If the row was revoked meanwhile, the
	// refund is a no-op — the registry is already 0 and correct, and the
	// poller's absolute observation keeps it that way.
	if _, err := tx.Exec(ctx, `
		UPDATE account_allowances
		SET allowance_raw = allowance_raw + $3
		WHERE account_id = $1 AND delegate = $2 AND revoked_at IS NULL`,
		s.BuyerAccount, s.Delegate, s.AmountRaw); err != nil {
		return false, fmt.Errorf("store: restore settlement: refund: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE delegation_settlements
		SET state = 'restored', fail_reason = $2, updated_at = $3
		WHERE id = $1`, id, reason, now); err != nil {
		return false, fmt.Errorf("store: restore settlement: mark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: restore settlement: %w", err)
	}
	return true, nil
}
