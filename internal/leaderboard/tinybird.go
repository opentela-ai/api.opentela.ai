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

// tinybirdPipe is the published endpoint that aggregates perf_samples
// (tinybird/endpoints/gpu_leaderboard.pipe).
const tinybirdPipe = "gpu_leaderboard"

// Tinybird queries the published gpu_leaderboard endpoint of a Tinybird
// Forward workspace instead of a self-managed ClickHouse. The static token
// carries only READ scope on the pipe, and the public handler already caps
// the window — this surface can never expose raw samples or peer identity.
type Tinybird struct {
	host   *url.URL
	token  string
	client *http.Client
}

// NewTinybird builds a querier for the Tinybird host (e.g.
// https://api.tinybird.co) and a pipe-READ-scoped token.
func NewTinybird(host *url.URL, token string) *Tinybird {
	return &Tinybird{host: host, token: token, client: &http.Client{Timeout: 15 * time.Second}}
}

// Leaderboard implements Querier.
func (t *Tinybird) Leaderboard(ctx context.Context, hours int, service, model string) ([]Row, error) {
	u := *t.host
	u.Path = "/v0/pipes/" + tinybirdPipe + ".json"
	q := u.Query()
	q.Set("token", t.token)
	q.Set("hours", strconv.Itoa(hours))
	if service != "" {
		q.Set("service", service)
	}
	if model != "" {
		q.Set("model", model)
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
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding Tinybird response: %w", err)
	}

	rows := make([]Row, 0, len(payload.Data))
	for _, raw := range payload.Data {
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
