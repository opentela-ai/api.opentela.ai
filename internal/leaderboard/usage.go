package leaderboard

import "context"

// Bounds on the usage window. perf_samples rolls over after 30 days
// (datasource TTL), so a wider window would silently return partial data.
const (
	defaultWindowDays = 30
	maxWindowDays     = 30
)

// UsageRow is one aggregate line: day × served model over the window.
type UsageRow struct {
	Day               string
	Model             string
	Requests          uint64
	InputTokens       uint64
	CachedInputTokens uint64
	OutputTokens      uint64
}

// UsageQuerier aggregates one token-usage window. Implemented by Tinybird
// (token_usage_daily pipe) and ClickHouse (perf_samples directly).
type UsageQuerier interface {
	TokenUsage(ctx context.Context, days int, service, model string, successOnly bool) ([]UsageRow, error)
}

type usageEntry struct {
	Day               string `json:"day"`
	Model             string `json:"model"`
	Requests          uint64 `json:"requests"`
	InputTokens       uint64 `json:"input_tokens"`
	CachedInputTokens uint64 `json:"cached_input_tokens"`
	OutputTokens      uint64 `json:"output_tokens"`
}

// windowDays maps the leaderboard's hour window onto the usage pipe's day
// window, capped at the 30-day sample retention.
func windowDays(hours int) int {
	days := (hours + 23) / 24
	if days < 1 {
		days = 1
	}
	if days > maxWindowDays {
		days = maxWindowDays
	}
	return days
}
