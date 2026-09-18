package leaderboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// TokenUsage implements UsageQuerier by aggregating perf_samples directly:
// the hourly rollup keeps only status counts, so the settlement predicate
// (2xx, no client abort, final token counts present) must be applied at row
// level here. The public handler caps days at 30, matching the datasource
// TTL, so the scan stays bounded.
func (c *ClickHouse) TokenUsage(ctx context.Context, days int, service, model string, successOnly bool) ([]UsageRow, error) {
	var cond strings.Builder
	cond.WriteString("ts >= now() - INTERVAL ")
	cond.WriteString(strconv.Itoa(days))
	cond.WriteString(" DAY AND model != ''")
	if successOnly {
		cond.WriteString(" AND status >= 200 AND status < 300 AND NOT client_abort" +
			" AND (input_tokens > 0 OR cached_input_tokens > 0 OR output_tokens > 0)")
	}
	if service != "" {
		fmt.Fprintf(&cond, " AND service = %s", quote(service))
	}
	if model != "" {
		fmt.Fprintf(&cond, " AND model = %s", quote(model))
	}

	query := fmt.Sprintf(`SELECT
	toDate(ts) AS day,
	model,
	count() AS requests,
	sum(input_tokens) AS input_tokens,
	sum(cached_input_tokens) AS cached_input_tokens,
	sum(output_tokens) AS output_tokens
FROM perf_samples
WHERE %s
GROUP BY day, model
ORDER BY day DESC, model
FORMAT JSONEachRow`, cond.String())

	u := *c.url
	q := u.Query()
	q.Set("database", c.database)
	// UInt64 columns arrive unquoted so the standard decoder can read them.
	q.Set("output_format_json_quote_64bit_integers", "0")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if c.username != "" {
		req.Header.Set("X-ClickHouse-User", c.username)
	}
	if c.password != "" {
		req.Header.Set("X-ClickHouse-Key", c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ClickHouse status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var rows []UsageRow
	dec := json.NewDecoder(resp.Body)
	for {
		var raw struct {
			Day               string `json:"day"`
			Model             string `json:"model"`
			Requests          uint64 `json:"requests"`
			InputTokens       uint64 `json:"input_tokens"`
			CachedInputTokens uint64 `json:"cached_input_tokens"`
			OutputTokens      uint64 `json:"output_tokens"`
		}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding ClickHouse response: %w", err)
		}
		rows = append(rows, UsageRow{
			Day: raw.Day, Model: raw.Model,
			Requests: raw.Requests, InputTokens: raw.InputTokens,
			CachedInputTokens: raw.CachedInputTokens, OutputTokens: raw.OutputTokens,
		})
	}
	return rows, nil
}
