package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FaucetClaim records a single OTELA faucet payout to an account. A pending
// claim (tx_signature = ”) is inserted before the on-chain transfer and
// completed via CompleteFaucetClaim once the transaction is confirmed. If the
// server crashes mid-flight the pending row lingers; ClaimFaucet will take it
// over again once it is older than FaucetPendingTTL, by which point the
// original transaction's blockhash has expired and can no longer confirm.
type FaucetClaim struct {
	AccountID   string
	Wallet      string
	AmountRaw   int64
	TxSignature string
	ClaimedAt   time.Time
}

// FaucetPendingTTL is how long a pending claim blocks a retry. It is
// intentionally larger than the Solana blockhash validity window (~60s on
// mainnet) and the API's send timeout (60s): once it elapses, the original
// transaction (if any) can no longer land, so a retry is safe.
const FaucetPendingTTL = 2 * time.Minute

// ClaimFaucet reserves a (possibly pending) faucet claim for accountID. It
// inserts a new pending row (tx_signature = ”) or, when an existing row is a
// stale pending claim older than FaucetPendingTTL, refreshes it so the account
// can retry after a mid-flight crash. A completed claim (non-empty
// tx_signature) or a recent pending claim always returns (false, nil). The
// unique constraint on account_id means only one request at a time can reach
// the on-chain send.
func (p *Postgres) ClaimFaucet(ctx context.Context, accountID, wallet, txSignature string, amountRaw int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO faucet_claims (account_id, wallet, amount_raw, tx_signature)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id) DO UPDATE
		SET tx_signature = '',
		    wallet       = EXCLUDED.wallet,
		    amount_raw   = EXCLUDED.amount_raw,
		    claimed_at   = now()
		WHERE faucet_claims.tx_signature = ''
		  AND faucet_claims.claimed_at < now() - $5::interval`,
		accountID, wallet, amountRaw, txSignature, FaucetPendingTTL.String())
	if err != nil {
		return false, fmt.Errorf("store: claim faucet: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteFaucetClaim sets the transaction signature on a pending claim,
// marking it as completed. It is called after the on-chain transfer succeeds.
// A completed claim (non-empty tx_signature) is never overwritten, so a
// late-arriving retry from a crashed request cannot clobber the real result.
func (p *Postgres) CompleteFaucetClaim(ctx context.Context, accountID, txSignature string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE faucet_claims SET tx_signature = $2
		WHERE account_id = $1 AND tx_signature = ''`,
		accountID, txSignature)
	if err != nil {
		return fmt.Errorf("store: complete faucet claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: complete faucet claim: no pending claim for %s", accountID)
	}
	return nil
}

// ClearFaucetClaim removes a pending claim so the account can retry after an
// on-chain send failure. A completed claim (non-empty tx_signature) is never
// cleared.
func (p *Postgres) ClearFaucetClaim(ctx context.Context, accountID string) error {
	_, err := p.pool.Exec(ctx, `
		DELETE FROM faucet_claims
		WHERE account_id = $1 AND tx_signature = ''`,
		accountID)
	if err != nil {
		return fmt.Errorf("store: clear faucet claim: %w", err)
	}
	return nil
}

// GetFaucetClaim returns the account's claim, or ErrNotFound when the account
// has not claimed yet.
func (p *Postgres) GetFaucetClaim(ctx context.Context, accountID string) (FaucetClaim, error) {
	var out FaucetClaim
	err := p.pool.QueryRow(ctx, `
		SELECT account_id, wallet, amount_raw, tx_signature, claimed_at
		FROM faucet_claims WHERE account_id = $1`, accountID).
		Scan(&out.AccountID, &out.Wallet, &out.AmountRaw, &out.TxSignature, &out.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FaucetClaim{}, ErrNotFound
	}
	if err != nil {
		return FaucetClaim{}, fmt.Errorf("store: get faucet claim: %w", err)
	}
	return out, nil
}
