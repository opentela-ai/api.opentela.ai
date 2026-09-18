package leaderboard

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
)

// Bounds on the public usage surface. perf_samples rolls over after 30 days
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

// UsageHandler serves GET /v1/token-usage with in-process TTL caching.
type UsageHandler struct {
	q   UsageQuerier
	ttl time.Duration

	mu      sync.Mutex
	entries map[usageCacheKey]usageCacheEntry

	now func() time.Time // tests
}

type usageCacheKey struct {
	days        int
	successOnly bool
	service     string
	model       string
}

type usageCacheEntry struct {
	payload usageResponse
	expires time.Time
}

type usageResponse struct {
	GeneratedAt string       `json:"generated_at"`
	WindowDays  int          `json:"window_days"`
	SuccessOnly bool         `json:"success_only"`
	Entries     []usageEntry `json:"entries"`
}

type usageEntry struct {
	Day               string `json:"day"`
	Model             string `json:"model"`
	Requests          uint64 `json:"requests"`
	InputTokens       uint64 `json:"input_tokens"`
	CachedInputTokens uint64 `json:"cached_input_tokens"`
	OutputTokens      uint64 `json:"output_tokens"`
}

// NewUsage builds a usage handler querying q and caching each distinct
// window for ttl.
func NewUsage(q UsageQuerier, ttl time.Duration) *UsageHandler {
	return &UsageHandler{q: q, ttl: ttl, entries: make(map[usageCacheKey]usageCacheEntry), now: time.Now}
}

func (h *UsageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	days, err := parseDays(r.URL.Query().Get("days"))
	if err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid days (1-30)"})
		return
	}
	successOnly := true // match the billing settlement predicate by default
	switch raw := r.URL.Query().Get("success_only"); raw {
	case "", "1", "true":
	case "0", "false":
		successOnly = false
	default:
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid success_only (expected 0 or 1)"})
		return
	}
	service, model := r.URL.Query().Get("service"), r.URL.Query().Get("model")
	if !validFilter(service) || !validFilter(model) {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service or model filter"})
		return
	}

	key := usageCacheKey{days: days, successOnly: successOnly, service: service, model: model}
	h.mu.Lock()
	if ce, ok := h.entries[key]; ok && h.now().Before(ce.expires) {
		payload := ce.payload
		h.mu.Unlock()
		httputil.WriteJSON(w, http.StatusOK, payload)
		return
	}
	h.mu.Unlock()

	rows, err := h.q.TokenUsage(r.Context(), days, service, model, successOnly)
	if err != nil {
		// Aggregate store unavailable: generic 503, never blank success.
		httputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "usage temporarily unavailable"})
		return
	}

	payload := usageResponse{
		GeneratedAt: h.now().UTC().Format(time.RFC3339),
		WindowDays:  days,
		SuccessOnly: successOnly,
		Entries:     make([]usageEntry, 0, len(rows)),
	}
	for _, row := range rows {
		payload.Entries = append(payload.Entries, usageEntry{
			Day:               row.Day,
			Model:             row.Model,
			Requests:          row.Requests,
			InputTokens:       row.InputTokens,
			CachedInputTokens: row.CachedInputTokens,
			OutputTokens:      row.OutputTokens,
		})
	}

	h.mu.Lock()
	// Opportunistic sweep keeps the map bounded without a janitor goroutine.
	cutoff := h.now()
	for k, ce := range h.entries {
		if cutoff.After(ce.expires) {
			delete(h.entries, k)
		}
	}
	h.entries[key] = usageCacheEntry{payload: payload, expires: cutoff.Add(h.ttl)}
	h.mu.Unlock()

	httputil.WriteJSON(w, http.StatusOK, payload)
}

func parseDays(raw string) (int, error) {
	if raw == "" {
		return defaultWindowDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > maxWindowDays {
		return 0, strconv.ErrSyntax
	}
	return days, nil
}
