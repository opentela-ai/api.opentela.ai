package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opentela-ai/api/internal/billing"
)

// TreasuryAccountID is exported for callers (tests, future handlers) that
// import the store package directly. It mirrors billing.TreasuryAccountID.
const TreasuryAccountID = billing.TreasuryAccountID

// maxAsksPerReplacement bounds a single pricing POST (matches the design: no
// more than 256 entries per peer).
const maxAsksPerReplacement = 256

// validateAsks enforces the structural contract the store guarantees even when
// the HTTP layer has already validated: at most 256 entries, no duplicate
// (service, model), non-negative rates bounded by billing.MaxBaseRate. It does
// NOT validate the service/model allowlist — that is the caller's job, since
// the allowlist is derived from the upstream catalog.
func validateAsks(asks []billing.Ask) error {
	if len(asks) > maxAsksPerReplacement {
		return fmt.Errorf("store: asks: %w: %d entries (max %d)", billing.ErrConflict, len(asks), maxAsksPerReplacement)
	}
	seen := make(map[string]bool, len(asks))
	for _, a := range asks {
		if a.Service == "" || a.Model == "" {
			return fmt.Errorf("store: asks: %w: empty service or model", billing.ErrConflict)
		}
		key := a.Service + "\x00" + a.Model
		if seen[key] {
			return fmt.Errorf("store: asks: %w: duplicate (%s, %s)", billing.ErrConflict, a.Service, a.Model)
		}
		seen[key] = true
		if a.InputPerMillion < 0 || a.CachedInputPerMillion < 0 || a.OutputPerMillion < 0 {
			return fmt.Errorf("store: asks: %w: negative rate", billing.ErrConflict)
		}
		if a.InputPerMillion >= billing.MaxBaseRate || a.CachedInputPerMillion >= billing.MaxBaseRate || a.OutputPerMillion >= billing.MaxBaseRate {
			return fmt.Errorf("store: asks: %w: rate exceeds bound", billing.ErrConflict)
		}
	}
	return nil
}

// ReplaceAsks performs a full replacement of the peer's asks within one
// transaction, assigning a single fresh revision across all rows and deleting
// any (service, model) not in the new set. An empty set clears all of the
// peer's asks. The server assigns expires_at = now + ttl; callers republish on
// a shorter cadence so asks never go stale while the peer is live.
func (p *Postgres) ReplaceAsks(ctx context.Context, peerID string, asks []billing.Ask, ttl time.Duration) (int64, error) {
	if peerID == "" {
		return 0, fmt.Errorf("store: replace asks: %w: empty peer id", billing.ErrConflict)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("store: replace asks: %w: non-positive ttl", billing.ErrConflict)
	}
	if err := validateAsks(asks); err != nil {
		return 0, err
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("store: replace asks: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, peerID); err != nil {
		return 0, fmt.Errorf("store: replace asks: lock: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(ttl)

	if len(asks) == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM peer_asks WHERE peer_id = $1`, peerID); err != nil {
			return 0, fmt.Errorf("store: replace asks: clear: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("store: replace asks: %w", err)
		}
		return 0, nil
	}

	var newRev int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(revision), 0) + 1 FROM peer_asks WHERE peer_id = $1`,
		peerID).Scan(&newRev); err != nil {
		return 0, fmt.Errorf("store: replace asks: revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM peer_asks WHERE peer_id = $1`, peerID); err != nil {
		return 0, fmt.Errorf("store: replace asks: delete: %w", err)
	}
	for _, a := range asks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO peer_asks
			    (peer_id, service, model, input_per_million, cached_input_per_million,
			     output_per_million, revision, expires_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			peerID, a.Service, a.Model, a.InputPerMillion, a.CachedInputPerMillion,
			a.OutputPerMillion, newRev, expires, now); err != nil {
			return 0, fmt.Errorf("store: replace asks: insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: replace asks: %w", err)
	}
	return newRev, nil
}

// LiveAsks returns the non-expired asks for (service, model), cheapest input
// rate first. This is a read; the caller (the gate) snapshots eligible peers
// and persists them into billing_requests — never re-reads at settlement.
func (p *Postgres) LiveAsks(ctx context.Context, service, model string, now time.Time) ([]billing.Ask, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT peer_id, service, model, input_per_million, cached_input_per_million,
		       output_per_million, revision, expires_at, updated_at
		FROM peer_asks
		WHERE service = $1 AND model = $2 AND expires_at > $3
		ORDER BY input_per_million ASC, peer_id ASC`,
		service, model, now)
	if err != nil {
		return nil, fmt.Errorf("store: live asks: %w", err)
	}
	defer rows.Close()
	var out []billing.Ask
	for rows.Next() {
		var a billing.Ask
		if err := rows.Scan(&a.PeerID, &a.Service, &a.Model, &a.InputPerMillion,
			&a.CachedInputPerMillion, &a.OutputPerMillion, &a.Revision,
			&a.ExpiresAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: live asks: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: live asks: rows: %w", err)
	}
	return out, nil
}

// LiveAsksByPeers returns the non-expired asks for the given peers, ordered
// by (peer_id, service, model) for stable display. This is the seller-side
// read behind GET /manage/billing/asks (the console pricing panel); the
// buyer-side read is LiveAsks (per service/model).
func (p *Postgres) LiveAsksByPeers(ctx context.Context, peerIDs []string, now time.Time) ([]billing.Ask, error) {
	if len(peerIDs) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT peer_id, service, model, input_per_million, cached_input_per_million,
		       output_per_million, revision, expires_at, updated_at
		FROM peer_asks
		WHERE peer_id = ANY($1) AND expires_at > $2
		ORDER BY peer_id ASC, service ASC, model ASC`,
		peerIDs, now)
	if err != nil {
		return nil, fmt.Errorf("store: live asks by peers: %w", err)
	}
	defer rows.Close()
	var out []billing.Ask
	for rows.Next() {
		var a billing.Ask
		if err := rows.Scan(&a.PeerID, &a.Service, &a.Model, &a.InputPerMillion,
			&a.CachedInputPerMillion, &a.OutputPerMillion, &a.Revision,
			&a.ExpiresAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: live asks by peers: scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: live asks by peers: rows: %w", err)
	}
	return out, nil
}

// EnsureAccountCredit creates the account's zero-balance row if it does not
// already exist. Safe to call concurrently and on every request.
func (p *Postgres) EnsureAccountCredit(ctx context.Context, accountID string) error {
	if accountID == "" {
		return fmt.Errorf("store: ensure account credit: %w: empty account", billing.ErrConflict)
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO account_credits (account_id, updated_at) VALUES ($1, now())
		 ON CONFLICT (account_id) DO NOTHING`, accountID)
	if err != nil {
		return fmt.Errorf("store: ensure account credit: %w", err)
	}
	return nil
}

// AccountCredit returns the projection for accountID, or billing.ErrNotFound.
func (p *Postgres) AccountCredit(ctx context.Context, accountID string) (billing.AccountCredit, error) {
	var c billing.AccountCredit
	var maxIn, maxCin, maxOut *int64
	err := p.pool.QueryRow(ctx, `
		SELECT account_id, credit_raw, reserved_raw,
		       max_input_per_million, max_cached_input_per_million, max_output_per_million,
		       updated_at
		FROM account_credits WHERE account_id = $1`, accountID).
		Scan(&c.AccountID, &c.CreditRaw, &c.ReservedRaw,
			&maxIn, &maxCin, &maxOut, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.AccountCredit{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: account credit: %w", err)
	}
	c.MaxInputPerMillion, c.MaxCachedInputPerMillion, c.MaxOutputPerMillion = maxIn, maxCin, maxOut
	return c, nil
}

// SetAccountCaps updates the buyer's three per-1M-token caps. A nil cap clears
// that dimension to unlimited. It creates the row if absent.
func (p *Postgres) SetAccountCaps(ctx context.Context, accountID string, caps billing.Caps) (billing.AccountCredit, error) {
	if accountID == "" {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: %w: empty account", billing.ErrConflict)
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_credits (account_id, updated_at) VALUES ($1, now())
		 ON CONFLICT (account_id) DO NOTHING`, accountID); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: ensure: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE account_credits
		SET max_input_per_million = $2,
		    max_cached_input_per_million = $3,
		    max_output_per_million = $4,
		    updated_at = now()
		WHERE account_id = $1`,
		accountID, caps.InputPerMillion, caps.CachedInputPerMillion, caps.OutputPerMillion); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: update: %w", err)
	}
	var c billing.AccountCredit
	var maxIn, maxCin, maxOut *int64
	if err := tx.QueryRow(ctx, `
		SELECT account_id, credit_raw, reserved_raw,
		       max_input_per_million, max_cached_input_per_million, max_output_per_million,
		       updated_at
		FROM account_credits WHERE account_id = $1`, accountID).
		Scan(&c.AccountID, &c.CreditRaw, &c.ReservedRaw,
			&maxIn, &maxCin, &maxOut, &c.UpdatedAt); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: scan: %w", err)
	}
	c.MaxInputPerMillion, c.MaxCachedInputPerMillion, c.MaxOutputPerMillion = maxIn, maxCin, maxOut
	if err := tx.Commit(ctx); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: set caps: %w", err)
	}
	return c, nil
}

// ReserveBilling computes the conservative reserve from the immutable quote
// snapshot, checks available credit under a row lock, bumps reserved_raw, and
// persists the request. Returns ErrInsufficientCredit when the account cannot
// cover the reserve and ErrConflict when the request_id already exists or the
// payload is invalid.
func (p *Postgres) ReserveBilling(ctx context.Context, req billing.Reservation) (billing.AccountCredit, error) {
	if req.RequestID == "" || req.BuyerAccountID == "" {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w: missing id", billing.ErrConflict)
	}
	if len(req.Quotes) == 0 {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w: no quotes", billing.ErrConflict)
	}
	for _, q := range req.Quotes {
		if q.PeerID == "" || q.SellerAccountID == "" {
			return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w: quote missing peer or seller", billing.ErrConflict)
		}
		if q.InputPerMillion < 0 || q.CachedInputPerMillion < 0 || q.OutputPerMillion < 0 {
			return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w: negative rate", billing.ErrConflict)
		}
	}
	reserve, err := billing.ReserveAmount(req.Quotes, req.InputCeil, req.OutputCeil)
	if err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w", err)
	}
	now := req.ReservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	quotesJSON, err := json.Marshal(req.Quotes)
	if err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: marshal quotes: %w", err)
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO account_credits (account_id, updated_at) VALUES ($1, $2)
		 ON CONFLICT (account_id) DO NOTHING`, req.BuyerAccountID, now); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: ensure: %w", err)
	}
	var creditRaw, reservedRaw int64
	if err := tx.QueryRow(ctx,
		`SELECT credit_raw, reserved_raw FROM account_credits WHERE account_id = $1 FOR UPDATE`,
		req.BuyerAccountID).Scan(&creditRaw, &reservedRaw); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: lock: %w", err)
	}
	if creditRaw-reservedRaw < reserve {
		return billing.AccountCredit{}, billing.ErrInsufficientCredit
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_requests
		    (request_id, buyer_account_id, service, model, eligible_peers,
		     max_input_per_million, max_cached_input_per_million, max_output_per_million,
		     reserved_raw, state, reserved_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,'reserved',$10)`,
		req.RequestID, req.BuyerAccountID, req.Service, req.Model, quotesJSON,
		req.Caps.InputPerMillion, req.Caps.CachedInputPerMillion, req.Caps.OutputPerMillion,
		reserve, now); err != nil {
		if isUniqueViolation(err) {
			return billing.AccountCredit{}, billing.ErrConflict
		}
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: insert: %w", err)
	}
	if reserve > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE account_credits SET reserved_raw = reserved_raw + $2, updated_at = $3
			 WHERE account_id = $1`, req.BuyerAccountID, reserve, now); err != nil {
			return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: bump: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.AccountCredit{}, fmt.Errorf("store: reserve billing: %w", err)
	}
	return billing.AccountCredit{
		AccountID:   req.BuyerAccountID,
		CreditRaw:   creditRaw,
		ReservedRaw: reservedRaw + reserve,
		UpdatedAt:   now,
	}, nil
}

// BillingRequest returns the persisted reservation/settlement record, or
// billing.ErrNotFound.
func (p *Postgres) BillingRequest(ctx context.Context, requestID string) (billing.Request, error) {
	return scanBillingRequest(ctx, p.pool, requestID)
}

func scanBillingRequest(ctx context.Context, q pgxQuerier, requestID string) (billing.Request, error) {
	var r billing.Request
	var quotesJSON []byte
	var maxIn, maxCin, maxOut *int64
	var servedPeer, servedSeller *string
	var servedRev, servedIn, servedCin, servedOut *int64
	var inT, cinT, outT *int
	var costRaw, feeRaw, sellerRaw *int64
	var settledAt, releasedAt *time.Time
	var releaseReason *string
	err := q.QueryRow(ctx, `
		SELECT request_id, buyer_account_id, service, model, eligible_peers,
		       max_input_per_million, max_cached_input_per_million, max_output_per_million,
		       reserved_raw, state, served_peer_id, served_seller_account_id, served_revision,
		       served_input_per_million, served_cached_input_per_million, served_output_per_million,
		       input_tokens, cached_input_tokens, output_tokens,
		       cost_raw, fee_raw, seller_raw,
		       reserved_at, settled_at, released_at, release_reason
		FROM billing_requests WHERE request_id = $1`, requestID).
		Scan(&r.RequestID, &r.BuyerAccountID, &r.Service, &r.Model, &quotesJSON,
			&maxIn, &maxCin, &maxOut,
			&r.ReservedRaw, &r.State, &servedPeer, &servedSeller, &servedRev,
			&servedIn, &servedCin, &servedOut,
			&inT, &cinT, &outT,
			&costRaw, &feeRaw, &sellerRaw,
			&r.ReservedAt, &settledAt, &releasedAt, &releaseReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Request{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Request{}, fmt.Errorf("store: billing request: %w", err)
	}
	r.SettledAt, r.ReleasedAt = settledAt, releasedAt
	r.Caps = billing.Caps{InputPerMillion: maxIn, CachedInputPerMillion: maxCin, OutputPerMillion: maxOut}
	if servedPeer != nil {
		r.ServedPeerID = *servedPeer
	}
	if servedSeller != nil {
		r.ServedSellerAccountID = *servedSeller
	}
	if servedRev != nil {
		r.ServedRevision = *servedRev
	}
	if servedIn != nil {
		r.ServedInputPerMillion = *servedIn
	}
	if servedCin != nil {
		r.ServedCachedInputPerMillion = *servedCin
	}
	if servedOut != nil {
		r.ServedOutputPerMillion = *servedOut
	}
	if inT != nil {
		r.InputTokens = *inT
	}
	if cinT != nil {
		r.CachedInputTokens = *cinT
	}
	if outT != nil {
		r.OutputTokens = *outT
	}
	if costRaw != nil {
		r.CostRaw = *costRaw
	}
	if feeRaw != nil {
		r.FeeRaw = *feeRaw
	}
	if sellerRaw != nil {
		r.SellerRaw = *sellerRaw
	}
	if releaseReason != nil {
		r.ReleaseReason = *releaseReason
	}
	if len(quotesJSON) > 0 {
		if err := json.Unmarshal(quotesJSON, &r.Quotes); err != nil {
			return billing.Request{}, fmt.Errorf("store: billing request: unmarshal quotes: %w", err)
		}
	}
	return r, nil
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SettleBilling finalizes a reserved request against the served peer resolved
// from the immutable snapshot, computes the integer charge (checked, ceiling),
// debits the buyer, credits the seller and any fee, and writes unique ledger
// legs — all in one transaction. A repeated call for an already-settled or
// already-released request is a no-op. A zero charge releases the reservation.
// The served peer must be present in the snapshot; an unknown peer releases the
// reservation and is never priced at a fresh read.
func (p *Postgres) SettleBilling(ctx context.Context, requestID, servedPeerID string, usage billing.Usage, feeBps int, now time.Time) (billing.Request, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := scanBillingRequest(ctx, tx, requestID)
	if err != nil {
		return billing.Request{}, err
	}
	if r.State != billing.StateReserved {
		return r, nil // already finalized: idempotent no-op
	}

	// Resolve the served peer against the immutable snapshot. An unknown peer
	// is released without charging — never priced at a fresh read.
	quote, ok := findQuote(r.Quotes, servedPeerID)
	if !ok {
		if err := releaseLocked(ctx, tx, &r, "unknown_peer", now); err != nil {
			return billing.Request{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return billing.Request{}, fmt.Errorf("store: settle billing: %w", err)
		}
		return r, nil
	}

	costRaw, err := billing.Cost(usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		quote.InputPerMillion, quote.CachedInputPerMillion, quote.OutputPerMillion)
	if err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: cost: %w", err)
	}
	if costRaw == 0 {
		// Priced peer with no tokens, or unpriced peer: release without charge.
		if err := releaseLocked(ctx, tx, &r, "zero_price", now); err != nil {
			return billing.Request{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return billing.Request{}, fmt.Errorf("store: settle billing: %w", err)
		}
		return r, nil
	}
	feeRaw, sellerRaw, err := billing.Fee(costRaw, feeBps)
	if err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: fee: %w", err)
	}

	// Lock the buyer, then the seller (and the treasury when there is a fee).
	// If buyer and seller are the same account the row is already held and the
	// second UPDATE simply applies to it; the constraints hold throughout.
	if _, _, err := applyDelta(ctx, tx, r.BuyerAccountID, -costRaw, -r.ReservedRaw, now); err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: buyer: %w", err)
	}
	if _, _, err := applyDelta(ctx, tx, quote.SellerAccountID, sellerRaw, 0, now); err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: seller: %w", err)
	}
	if feeRaw > 0 {
		if _, _, err := applyDelta(ctx, tx, billing.TreasuryAccountID, feeRaw, 0, now); err != nil {
			return billing.Request{}, fmt.Errorf("store: settle billing: treasury: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, model,
		     input_per_million, cached_input_per_million, output_per_million,
		     input_tokens, cached_input_tokens, output_tokens, created_at)
		VALUES ($1,$2,'usage','buyer',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		r.BuyerAccountID, -costRaw, quote.PeerID, requestID, r.Model,
		quote.InputPerMillion, quote.CachedInputPerMillion, quote.OutputPerMillion,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, now); err != nil {
		if isUniqueViolation(err) {
			return billing.Request{}, fmt.Errorf("store: settle billing: %w: buyer leg exists", billing.ErrConflict)
		}
		return billing.Request{}, fmt.Errorf("store: settle billing: buyer ledger: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_ledger
		    (account_id, delta_raw, source, leg, counterparty, ref, model,
		     input_per_million, cached_input_per_million, output_per_million,
		     input_tokens, cached_input_tokens, output_tokens, created_at)
		VALUES ($1,$2,'earn','seller',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		quote.SellerAccountID, sellerRaw, r.BuyerAccountID, requestID, r.Model,
		quote.InputPerMillion, quote.CachedInputPerMillion, quote.OutputPerMillion,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, now); err != nil {
		if isUniqueViolation(err) {
			return billing.Request{}, fmt.Errorf("store: settle billing: %w: seller leg exists", billing.ErrConflict)
		}
		return billing.Request{}, fmt.Errorf("store: settle billing: seller ledger: %w", err)
	}
	if feeRaw > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO credit_ledger
			    (account_id, delta_raw, source, leg, counterparty, ref, created_at)
			VALUES ($1,$2,'fee','fee',$3,$4,$5)`,
			billing.TreasuryAccountID, feeRaw, r.BuyerAccountID, requestID, now); err != nil {
			if isUniqueViolation(err) {
				return billing.Request{}, fmt.Errorf("store: settle billing: %w: fee leg exists", billing.ErrConflict)
			}
			return billing.Request{}, fmt.Errorf("store: settle billing: fee ledger: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE billing_requests
		SET state = 'settled',
		    served_peer_id = $2,
		    served_seller_account_id = $3,
		    served_revision = $4,
		    served_input_per_million = $5,
		    served_cached_input_per_million = $6,
		    served_output_per_million = $7,
		    input_tokens = $8,
		    cached_input_tokens = $9,
		    output_tokens = $10,
		    cost_raw = $11,
		    fee_raw = $12,
		    seller_raw = $13,
		    settled_at = $14
		WHERE request_id = $1`,
		requestID, quote.PeerID, quote.SellerAccountID, quote.Revision,
		quote.InputPerMillion, quote.CachedInputPerMillion, quote.OutputPerMillion,
		usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens,
		costRaw, feeRaw, sellerRaw, now); err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: finalize: %w", err)
	}
	r.State = billing.StateSettled
	r.ServedPeerID = quote.PeerID
	r.ServedSellerAccountID = quote.SellerAccountID
	r.ServedRevision = quote.Revision
	r.ServedInputPerMillion = quote.InputPerMillion
	r.ServedCachedInputPerMillion = quote.CachedInputPerMillion
	r.ServedOutputPerMillion = quote.OutputPerMillion
	r.InputTokens, r.CachedInputTokens, r.OutputTokens = usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens
	r.CostRaw, r.FeeRaw, r.SellerRaw = costRaw, feeRaw, sellerRaw
	r.SettledAt = &now
	if err := tx.Commit(ctx); err != nil {
		return billing.Request{}, fmt.Errorf("store: settle billing: %w", err)
	}
	return r, nil
}

// ReleaseBilling releases a reserved reservation without charging. A repeated
// release (or releasing an already-settled request) is a no-op.
func (p *Postgres) ReleaseBilling(ctx context.Context, requestID, reason string, now time.Time) (billing.Request, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Request{}, fmt.Errorf("store: release billing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	r, err := scanBillingRequest(ctx, tx, requestID)
	if err != nil {
		return billing.Request{}, err
	}
	if r.State != billing.StateReserved {
		return r, nil // idempotent
	}
	if err := releaseLocked(ctx, tx, &r, reason, now); err != nil {
		return billing.Request{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Request{}, fmt.Errorf("store: release billing: %w", err)
	}
	return r, nil
}

// releaseLocked decrements the buyer's reserved_raw by the request's
// reservation and finalizes the request as released. The request row must
// already be locked and in the reserved state by the caller.
func releaseLocked(ctx context.Context, tx pgx.Tx, r *billing.Request, reason string, now time.Time) error {
	if r.ReservedRaw > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE account_credits SET reserved_raw = reserved_raw - $2, updated_at = $3
			 WHERE account_id = $1`, r.BuyerAccountID, r.ReservedRaw, now); err != nil {
			return fmt.Errorf("store: release billing: unreserve: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE billing_requests SET state = 'released', released_at = $2, release_reason = $3
		 WHERE request_id = $1`, r.RequestID, now, reason); err != nil {
		return fmt.Errorf("store: release billing: finalize: %w", err)
	}
	r.State = billing.StateReleased
	r.ReleasedAt = &now
	r.ReleaseReason = reason
	return nil
}

// applyDelta applies a credit and a reserved delta to one account under a row
// lock, creating the row if absent. creditDelta/reservedDelta may be negative.
// It returns the new credit_raw and reserved_raw for assertion-free testing.
func applyDelta(ctx context.Context, tx pgx.Tx, accountID string, creditDelta, reservedDelta int64, now time.Time) (int64, int64, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_credits (account_id, updated_at) VALUES ($1, $2)
		 ON CONFLICT (account_id) DO NOTHING`, accountID, now); err != nil {
		return 0, 0, fmt.Errorf("ensure: %w", err)
	}
	var creditRaw, reservedRaw int64
	if err := tx.QueryRow(ctx,
		`SELECT credit_raw, reserved_raw FROM account_credits WHERE account_id = $1 FOR UPDATE`,
		accountID).Scan(&creditRaw, &reservedRaw); err != nil {
		return 0, 0, fmt.Errorf("lock: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE account_credits
		 SET credit_raw = credit_raw + $2, reserved_raw = reserved_raw + $3, updated_at = $4
		 WHERE account_id = $1`,
		accountID, creditDelta, reservedDelta, now); err != nil {
		// A CHECK violation (reserved_raw > credit_raw, or either negative)
		// bubbles up as the underlying pg error; the caller wraps it.
		return 0, 0, fmt.Errorf("apply: %w", err)
	}
	return creditRaw + creditDelta, reservedRaw + reservedDelta, nil
}

// ReconcileAccount verifies account_credits against confirmed ledger movements
// and open reservations. It is the bedrock of the "account_credits is a
// transactional projection" claim: the expected credit is SUM(ledger.delta_raw)
// and the expected reserved is SUM(billing_requests.reserved_raw) over the
// account's open requests.
func (p *Postgres) ReconcileAccount(ctx context.Context, accountID string, now time.Time) (billing.Reconciliation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return billing.Reconciliation{}, fmt.Errorf("store: reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var creditRaw, reservedRaw int64
	err = tx.QueryRow(ctx,
		`SELECT credit_raw, reserved_raw FROM account_credits WHERE account_id = $1 FOR UPDATE`,
		accountID).Scan(&creditRaw, &reservedRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		// An account with no row has, by definition, no movements and no open
		// reservations; everything reconciles to zero.
		if err := tx.Commit(ctx); err != nil {
			return billing.Reconciliation{}, fmt.Errorf("store: reconcile: %w", err)
		}
		return billing.Reconciliation{AccountID: accountID, InvariantHeld: true}, nil
	}
	if err != nil {
		return billing.Reconciliation{}, fmt.Errorf("store: reconcile: lock: %w", err)
	}

	var expectedCredit, ledgerRows int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta_raw), 0), count(*) FROM credit_ledger WHERE account_id = $1`,
		accountID).Scan(&expectedCredit, &ledgerRows); err != nil {
		return billing.Reconciliation{}, fmt.Errorf("store: reconcile: ledger: %w", err)
	}
	var expectedReserved, openRes int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(reserved_raw), 0), count(*)
		 FROM billing_requests
		 WHERE buyer_account_id = $1 AND state = 'reserved'`,
		accountID).Scan(&expectedReserved, &openRes); err != nil {
		return billing.Reconciliation{}, fmt.Errorf("store: reconcile: reservations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return billing.Reconciliation{}, fmt.Errorf("store: reconcile: %w", err)
	}

	rec := billing.Reconciliation{
		AccountID:        accountID,
		CreditRaw:        creditRaw,
		ReservedRaw:      reservedRaw,
		ExpectedCredit:   expectedCredit,
		ExpectedReserved: expectedReserved,
		LedgerRows:       ledgerRows,
		OpenReservations: openRes,
		DriftCredit:      creditRaw - expectedCredit,
		DriftReserved:    reservedRaw - expectedReserved,
		InvariantHeld:    creditRaw >= 0 && reservedRaw >= 0 && reservedRaw <= creditRaw,
	}
	return rec, nil
}

// ListLedger returns one page of immutable balance movements for an account,
// newest-first, plus the cursor for the next page (nil when exhausted). A nil
// cursor starts from the newest row.
func (p *Postgres) ListLedger(ctx context.Context, accountID string, cursor *billing.LedgerCursor, limit int) ([]billing.LedgerEntry, *billing.LedgerCursor, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = p.pool.Query(ctx, `
			SELECT id, account_id, delta_raw, source, leg, counterparty, ref, model,
			       input_per_million, cached_input_per_million, output_per_million,
			       input_tokens, cached_input_tokens, output_tokens, created_at
			FROM credit_ledger
			WHERE account_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2`, accountID, limit)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id, account_id, delta_raw, source, leg, counterparty, ref, model,
			       input_per_million, cached_input_per_million, output_per_million,
			       input_tokens, cached_input_tokens, output_tokens, created_at
			FROM credit_ledger
			WHERE account_id = $1 AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC
			LIMIT $4`, accountID, cursor.CreatedAt, cursor.ID, limit)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list ledger: %w", err)
	}
	defer rows.Close()

	var out []billing.LedgerEntry
	for rows.Next() {
		var e billing.LedgerEntry
		var cp, ref, model *string
		var inR, cinR, outR *int64
		var inT, cinT, outT *int
		if err := rows.Scan(&e.ID, &e.AccountID, &e.DeltaRaw, &e.Source, &e.Leg,
			&cp, &ref, &model, &inR, &cinR, &outR, &inT, &cinT, &outT, &e.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: list ledger: scan: %w", err)
		}
		e.Counterparty, e.Ref, e.Model = derefStr(cp), derefStr(ref), derefStr(model)
		e.InputPerMillion, e.CachedInputPerMillion, e.OutputPerMillion = inR, cinR, outR
		e.InputTokens, e.CachedInputTokens, e.OutputTokens = inT, cinT, outT
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list ledger: rows: %w", err)
	}
	var next *billing.LedgerCursor
	if len(out) == limit {
		last := out[len(out)-1]
		next = &billing.LedgerCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// SweepStaleReservations releases reserved requests older than `before`, after
// confirming (by age) they can no longer settle. It uses FOR UPDATE SKIP LOCKED
// so multiple API replicas can sweep concurrently without contention: each
// replica locks a disjoint batch. Only reserved rows are touched; settled or
// already-released requests are skipped. Returns the released request ids.
//
// The caller chooses `before` to be older than the maximum request lifetime
// (e.g. now - 2× the upstream timeout) so an in-flight response that arrives
// just after the sweep re-reserves normally on retry.
func (p *Postgres) SweepStaleReservations(ctx context.Context, before time.Time, limit int) ([]string, error) {
	if before.IsZero() {
		return nil, fmt.Errorf("store: sweep: %w: zero before", billing.ErrConflict)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT request_id, buyer_account_id, reserved_raw
		  FROM billing_requests
		 WHERE state = 'reserved' AND reserved_at < $1
		 ORDER BY reserved_at
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("store: sweep: select: %w", err)
	}
	type pending struct {
		requestID, buyer string
		reservedRaw      int64
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.requestID, &p.buyer, &p.reservedRaw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: sweep: scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sweep: rows: %w", err)
	}

	released := make([]string, 0, len(batch))
	now := time.Now().UTC()
	for _, p := range batch {
		r := billing.Request{RequestID: p.requestID, BuyerAccountID: p.buyer, ReservedRaw: p.reservedRaw, State: billing.StateReserved}
		if err := releaseLocked(ctx, tx, &r, "swept_stale", now); err != nil {
			return nil, fmt.Errorf("store: sweep: release %s: %w", p.requestID, err)
		}
		released = append(released, p.requestID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: sweep: %w", err)
	}
	return released, nil
}

// findQuote resolves a served peer against the immutable snapshot by peer id.
func findQuote(quotes []billing.EligiblePeerQuote, peerID string) (billing.EligiblePeerQuote, bool) {
	for _, q := range quotes {
		if q.PeerID == peerID {
			return q, true
		}
	}
	return billing.EligiblePeerQuote{}, false
}
