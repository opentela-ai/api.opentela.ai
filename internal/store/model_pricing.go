package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/opentela-ai/api/internal/billing"
)

// Per-model buyer caps (§5) and console-managed seller ask config (§11.5
// market surface). Both are owner-editable from the manage API; the ask
// config is republished to the TTL'd market table by the ask refresher.

// ModelCapsForAccount lists the account's per-(service, model) cap sheet.
func (p *Postgres) ModelCapsForAccount(ctx context.Context, accountID string) ([]billing.ModelCaps, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT service, model, max_input_per_million,
		       max_cached_input_per_million, max_output_per_million
		FROM account_model_caps
		WHERE account_id = $1
		ORDER BY service, model`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: model caps: %w", err)
	}
	defer rows.Close()
	var out []billing.ModelCaps
	for rows.Next() {
		var r billing.ModelCaps
		if err := rows.Scan(&r.Service, &r.Model, &r.InputPerMillion,
			&r.CachedInputPerMillion, &r.OutputPerMillion); err != nil {
			return nil, fmt.Errorf("store: model caps: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model caps: %w", err)
	}
	return out, nil
}

// ReplaceModelCaps atomically replaces the account's whole per-model cap
// sheet (the console saves the table as one unit). An empty sheet clears it.
func (p *Postgres) ReplaceModelCaps(ctx context.Context, accountID string, rows []billing.ModelCaps) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: replace model caps: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM account_model_caps WHERE account_id = $1`, accountID); err != nil {
		return fmt.Errorf("store: replace model caps: %w", err)
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_model_caps
				(account_id, service, model, max_input_per_million,
				 max_cached_input_per_million, max_output_per_million)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			accountID, r.Service, r.Model, r.InputPerMillion,
			r.CachedInputPerMillion, r.OutputPerMillion); err != nil {
			return fmt.Errorf("store: replace model caps: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: replace model caps: %w", err)
	}
	return nil
}

// AskConfigForPeers returns the durable ask config for each of the given
// peers that has one. Missing peers are absent from the map.
func (p *Postgres) AskConfigForPeers(ctx context.Context, peerIDs []string) (map[string][]billing.Ask, error) {
	out := make(map[string][]billing.Ask, len(peerIDs))
	if len(peerIDs) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT peer_id, service, model, input_per_million,
		       cached_input_per_million, output_per_million, updated_at
		FROM peer_ask_config
		WHERE peer_id = ANY($1)
		ORDER BY peer_id, service, model`, peerIDs)
	if err != nil {
		return nil, fmt.Errorf("store: ask config: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var peerID string
		var a billing.Ask
		if err := rows.Scan(&peerID, &a.Service, &a.Model, &a.InputPerMillion,
			&a.CachedInputPerMillion, &a.OutputPerMillion, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: ask config: scan: %w", err)
		}
		out[peerID] = append(out[peerID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ask config: %w", err)
	}
	return out, nil
}

// ReplaceAskConfig atomically replaces one peer's durable ask config (full
// replacement — the console saves the peer's price sheet as one unit). An
// empty sheet clears the config; live market rows then expire naturally.
func (p *Postgres) ReplaceAskConfig(ctx context.Context, peerID string, asks []billing.Ask) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: replace ask config: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM peer_ask_config WHERE peer_id = $1`, peerID); err != nil {
		return fmt.Errorf("store: replace ask config: %w", err)
	}
	for _, a := range asks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO peer_ask_config
				(peer_id, service, model, input_per_million,
				 cached_input_per_million, output_per_million)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			peerID, a.Service, a.Model, a.InputPerMillion,
			a.CachedInputPerMillion, a.OutputPerMillion); err != nil {
			return fmt.Errorf("store: replace ask config: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: replace ask config: %w", err)
	}
	return nil
}

// AllAskConfigs returns every peer's durable ask config, grouped by peer —
// the ask refresher's scan set.
func (p *Postgres) AllAskConfigs(ctx context.Context) (map[string][]billing.Ask, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT peer_id, service, model, input_per_million,
		       cached_input_per_million, output_per_million, updated_at
		FROM peer_ask_config
		ORDER BY peer_id, service, model`)
	if err != nil {
		return nil, fmt.Errorf("store: all ask configs: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]billing.Ask)
	for rows.Next() {
		var peerID string
		var a billing.Ask
		if err := rows.Scan(&peerID, &a.Service, &a.Model, &a.InputPerMillion,
			&a.CachedInputPerMillion, &a.OutputPerMillion, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: all ask configs: scan: %w", err)
		}
		out[peerID] = append(out[peerID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: all ask configs: %w", err)
	}
	return out, nil
}
