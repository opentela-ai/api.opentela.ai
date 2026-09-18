package leaderboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// usagePipe is the published endpoint that aggregates perf_samples into
// daily token totals (tinybird/endpoints/token_usage_daily.pipe).
const usagePipe = "token_usage_daily"

// UsageTinybird queries the published token_usage_daily endpoint of a
// Tinybird Forward workspace. It is separate from Tinybird because it
// carries its own pipe-READ-scoped token (TINYBIRD_USAGE_TOKEN), so the
// leaderboard and usage credentials never overlap in scope.
type UsageTinybird struct {
	host   *url.URL
	token  string
	client *http.Client
}

// NewUsageTinybird builds a querier for the Tinybird host (e.g.
// https://api.tinybird.co) and a token_usage_daily-READ-scoped token.
func NewUsageTinybird(host *url.URL, token string) *UsageTinybird {
	return &UsageTinybird{host: host, token: token, client: &http.Client{Timeout: 15 * time.Second}}
}

// TokenUsage implements UsageQuerier.
func (t *UsageTinybird) TokenUsage(ctx context.Context, days int, service, model string, successOnly bool) ([]UsageRow, error) {
	u := *t.host
	u.Path = "/v0/pipes/" + usagePipe + ".json"
	q := u.Query()
	q.Set("token", t.token)
	q.Set("days", strconv.Itoa(days))
	if service != "" {
		q.Set("service", service)
	}
	if model != "" {
		q.Set("model", model)
	}
	if !successOnly {
		q.Set("success_only", "0")
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tinybird status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			Day               string `json:"day"`
			Model             string `json:"model"`
			Requests          uint64 `json:"requests"`
			InputTokens       uint64 `json:"input_tokens"`
			CachedInputTokens uint64 `json:"cached_input_tokens"`
			OutputTokens      uint64 `json:"output_tokens"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding Tinybird response: %w", err)
	}

	rows := make([]UsageRow, 0, len(payload.Data))
	for _, raw := range payload.Data {
		rows = append(rows, UsageRow{
			Day: raw.Day, Model: raw.Model,
			Requests: raw.Requests, InputTokens: raw.InputTokens,
			CachedInputTokens: raw.CachedInputTokens, OutputTokens: raw.OutputTokens,
		})
	}
	return rows, nil
}
