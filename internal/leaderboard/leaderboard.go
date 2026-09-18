// Package leaderboard serves the public GPU performance leaderboard: an
// anonymized, aggregated view of how fast each GPU model serves each model
// across the mesh, computed from the perf samples recorded in ClickHouse —
// plus, when a usage querier is configured, the daily token-usage section of
// the same response.
//
// The endpoint is permissionless (like the service catalogue), so it applies
// the same abuse posture: short in-process caching, hard caps on the query
// window, and strict parameter validation — no raw samples, no peer identity,
// only percentile aggregates over the hourly rollup.
package leaderboard

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/opentela-ai/api/internal/httputil"
)

// Bounds on the public query surface.
const (
	defaultWindowHours = 168 // one week
	maxWindowHours     = 720 // matches the raw-sample retention rationale
	maxEntries         = 100
)

// Row is one aggregate line: GPU model × served model over the window.
type Row struct {
	GPUModel        string
	Model           string
	Requests        uint64
	ServerErrors    uint64
	ClientAborts    uint64
	Providers       uint64
	AvgTokensPerSec float64
	P50TokensPerSec float64
	TTFTP50Ms       float64
	TTFTP90Ms       float64
	TTFTP99Ms       float64
}

// Querier aggregates one leaderboard window. Implemented by ClickHouse.
type Querier interface {
	Leaderboard(ctx context.Context, hours int, service, model string) ([]Row, error)
}

// Handler serves GET /v1/leaderboard with in-process TTL caching. With a
// non-nil usage querier the response also carries the daily token-usage
// section (window derived from the requested hours).
type Handler struct {
	q   Querier
	u   UsageQuerier
	ttl time.Duration

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry

	now func() time.Time // tests
}

type cacheKey struct {
	hours       int
	successOnly bool
	service     string
	model       string
}

type cacheEntry struct {
	payload response
	expires time.Time
}

type response struct {
	GeneratedAt string       `json:"generated_at"`
	WindowHours int          `json:"window_hours"`
	WindowDays  int          `json:"window_days,omitempty"`
	SuccessOnly *bool        `json:"success_only,omitempty"`
	Entries     []entry      `json:"entries"`
	Usage       []usageEntry `json:"usage,omitempty"`
}

type entry struct {
	GPUModel        string  `json:"gpu_model"`
	Model           string  `json:"model"`
	Requests        uint64  `json:"requests"`
	Providers       uint64  `json:"providers"`
	SuccessRate     float64 `json:"success_rate"`
	AvgTokensPerSec float64 `json:"avg_output_tokens_per_sec"`
	P50TokensPerSec float64 `json:"p50_output_tokens_per_sec"`
	TTFTP50Ms       float64 `json:"ttft_p50_ms"`
	TTFTP90Ms       float64 `json:"ttft_p90_ms"`
	TTFTP99Ms       float64 `json:"ttft_p99_ms"`
}

// New builds a handler querying q and caching each distinct window for ttl.
func New(q Querier, ttl time.Duration) *Handler {
	return &Handler{q: q, ttl: ttl, entries: make(map[cacheKey]cacheEntry), now: time.Now}
}

// NewWithUsage is New plus a usage querier folded into the same response.
func NewWithUsage(q Querier, u UsageQuerier, ttl time.Duration) *Handler {
	h := New(q, ttl)
	h.u = u
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hours, err := parseHours(r.URL.Query().Get("hours"))
	if err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	successOnly, err := parseSuccessOnly(r.URL.Query().Get("success_only"))
	if err != nil {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	service, model := r.URL.Query().Get("service"), r.URL.Query().Get("model")
	if !validFilter(service) || !validFilter(model) {
		httputil.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid service or model filter"})
		return
	}

	key := cacheKey{hours: hours, successOnly: successOnly, service: service, model: model}
	h.mu.Lock()
	if ce, ok := h.entries[key]; ok && h.now().Before(ce.expires) {
		payload := ce.payload
		h.mu.Unlock()
		httputil.WriteJSON(w, http.StatusOK, payload)
		return
	}
	h.mu.Unlock()

	// Both aggregates come from the same store; run them in parallel. The
	// perf query is load-bearing (503 on failure), while a usage failure
	// only drops the usage section — the leaderboard must not depend on the
	// newer pipe being healthy.
	var (
		rows  []Row
		lerr  error
		urows []UsageRow
		uerr  error
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rows, lerr = h.q.Leaderboard(r.Context(), hours, service, model)
	}()
	if h.u != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			urows, uerr = h.u.TokenUsage(r.Context(), windowDays(hours), service, model, successOnly)
		}()
	}
	wg.Wait()
	if lerr != nil {
		// Aggregate store unavailable: generic 503, never blank success.
		httputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "leaderboard temporarily unavailable"})
		return
	}

	payload := response{
		GeneratedAt: h.now().UTC().Format(time.RFC3339),
		WindowHours: hours,
		Entries:     make([]entry, 0, len(rows)),
	}
	for _, row := range rows {
		success := float64(1)
		if row.Requests > 0 {
			success = float64(row.Requests-row.ServerErrors) / float64(row.Requests)
		}
		payload.Entries = append(payload.Entries, entry{
			GPUModel:        row.GPUModel,
			Model:           row.Model,
			Requests:        row.Requests,
			Providers:       row.Providers,
			SuccessRate:     round3(success),
			AvgTokensPerSec: row.AvgTokensPerSec,
			P50TokensPerSec: row.P50TokensPerSec,
			TTFTP50Ms:       row.TTFTP50Ms,
			TTFTP90Ms:       row.TTFTP90Ms,
			TTFTP99Ms:       row.TTFTP99Ms,
		})
	}
	if h.u != nil {
		payload.WindowDays = windowDays(hours)
		payload.SuccessOnly = &successOnly
		if uerr == nil {
			payload.Usage = make([]usageEntry, 0, len(urows))
			for _, row := range urows {
				payload.Usage = append(payload.Usage, usageEntry{
					Day:               row.Day,
					Model:             row.Model,
					Requests:          row.Requests,
					InputTokens:       row.InputTokens,
					CachedInputTokens: row.CachedInputTokens,
					OutputTokens:      row.OutputTokens,
				})
			}
		}
		// uerr != nil: the usage section is omitted and the degraded
		// response is cached for ttl — bounded staleness instead of
		// unthrottled re-querying while the pipe recovers.
	}

	h.mu.Lock()
	// Opportunistic sweep keeps the map bounded without a janitor goroutine.
	cutoff := h.now()
	for k, ce := range h.entries {
		if cutoff.After(ce.expires) {
			delete(h.entries, k)
		}
	}
	h.entries[key] = cacheEntry{payload: payload, expires: cutoff.Add(h.ttl)}
	h.mu.Unlock()

	httputil.WriteJSON(w, http.StatusOK, payload)
}

func parseHours(raw string) (int, error) {
	if raw == "" {
		return defaultWindowHours, nil
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours < 1 || hours > maxWindowHours {
		return 0, strconv.ErrSyntax
	}
	return hours, nil
}

func parseSuccessOnly(raw string) (bool, error) {
	switch raw {
	case "", "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	}
	return false, errors.New("invalid success_only (expected 0 or 1)")
}

// validFilter keeps query interpolation injection-free by construction: only
// characters seen in real service/model names pass.
func validFilter(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '/', c == ':':
		default:
			return false
		}
	}
	return true
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }
