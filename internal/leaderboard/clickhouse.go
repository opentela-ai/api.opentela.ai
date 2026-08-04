package leaderboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ClickHouse queries the perf_hourly rollup (clickhouse/schema.sql) over the
// HTTP interface: TDigest/unique states merge across hours, so percentile and
// provider counts stay exact-to-the-digest at any window. Stdlib only.
type ClickHouse struct {
	url      *url.URL
	database string
	username string
	password string
	client   *http.Client
}

func NewClickHouse(chURL *url.URL, database, username, password string) *ClickHouse {
	return &ClickHouse{
		url:      chURL,
		database: database,
		username: username,
		password: password,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Leaderboard implements Querier.
func (c *ClickHouse) Leaderboard(ctx context.Context, hours int, service, model string) ([]Row, error) {
	var cond strings.Builder
	cond.WriteString("hour >= now() - INTERVAL ")
	cond.WriteString(strconv.Itoa(hours))
	cond.WriteString(" HOUR AND gpu_model != '' AND model != ''")
	if service != "" {
		fmt.Fprintf(&cond, " AND service = %s", quote(service))
	}
	if model != "" {
		fmt.Fprintf(&cond, " AND model = %s", quote(model))
	}

	query := fmt.Sprintf(`SELECT
	gpu_model,
	model,
	sum(requests) AS requests,
	sum(errors) AS server_errors,
	sum(client_aborts) AS client_aborts,
	uniqMerge(provider_uniq) AS providers,
	round(if(sum(gen_ms) > 0, sum(output_tokens) * 1000.0 / sum(gen_ms), 0), 2) AS avg_tps,
	round(quantileTDigestMerge(0.5)(tps_q), 2) AS p50_tps,
	round(quantileTDigestMerge(0.5)(ttft_q), 1) AS ttft_p50_ms,
	round(quantileTDigestMerge(0.9)(ttft_q), 1) AS ttft_p90_ms,
	round(quantileTDigestMerge(0.99)(ttft_q), 1) AS ttft_p99_ms
FROM perf_hourly
WHERE %s
GROUP BY gpu_model, model
ORDER BY avg_tps DESC
LIMIT %d
FORMAT JSONEachRow`, cond.String(), maxEntries)

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

	var rows []Row
	dec := json.NewDecoder(resp.Body)
	for {
		var raw struct {
			GPUModel     string  `json:"gpu_model"`
			Model        string  `json:"model"`
			Requests     uint64  `json:"requests"`
			ServerErrors uint64  `json:"server_errors"`
			ClientAborts uint64  `json:"client_aborts"`
			Providers    uint64  `json:"providers"`
			AvgTPS       float64 `json:"avg_tps"`
			P50TPS       float64 `json:"p50_tps"`
			TTFTP50Ms    float64 `json:"ttft_p50_ms"`
			TTFTP90Ms    float64 `json:"ttft_p90_ms"`
			TTFTP99Ms    float64 `json:"ttft_p99_ms"`
		}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding ClickHouse response: %w", err)
		}
		rows = append(rows, Row{
			GPUModel: raw.GPUModel, Model: raw.Model,
			Requests: raw.Requests, ServerErrors: raw.ServerErrors, ClientAborts: raw.ClientAborts,
			Providers:       raw.Providers,
			AvgTokensPerSec: clean(raw.AvgTPS), P50TokensPerSec: clean(raw.P50TPS),
			TTFTP50Ms: clean(raw.TTFTP50Ms), TTFTP90Ms: clean(raw.TTFTP90Ms), TTFTP99Ms: clean(raw.TTFTP99Ms),
		})
	}
	return rows, nil
}

// quote escapes a validated (validFilter-charset) filter value for ClickHouse.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// clean normalizes non-finite aggregates (empty digest windows) to zero so
// the public JSON never carries NaN/Inf.
func clean(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
