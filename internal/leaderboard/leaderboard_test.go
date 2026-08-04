package leaderboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeQuerier struct {
	rows []Row
	err  error

	calls    int
	gotHours int
	gotSvc   string
	gotModel string
}

func (f *fakeQuerier) Leaderboard(_ context.Context, hours int, service, model string) ([]Row, error) {
	f.calls++
	f.gotHours, f.gotSvc, f.gotModel = hours, service, model
	return f.rows, f.err
}

func do(h http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func leaderboardFixture() []Row {
	return []Row{{
		GPUModel: "NVIDIA GeForce RTX 4090", Model: "gpt-4o",
		Requests: 100, ServerErrors: 5, Providers: 7,
		AvgTokensPerSec: 63.2, P50TokensPerSec: 61.0,
		TTFTP50Ms: 240.5, TTFTP90Ms: 610.0, TTFTP99Ms: 1500.0,
	}}
}

func TestHandlerAggregatesAndShape(t *testing.T) {
	q := &fakeQuerier{rows: leaderboardFixture()}
	h := New(q, time.Minute)

	rec := do(h, "/v1/leaderboard?hours=48&service=chat&model=gpt-4o")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if q.gotHours != 48 || q.gotSvc != "chat" || q.gotModel != "gpt-4o" {
		t.Errorf("querier args = %d/%q/%q", q.gotHours, q.gotSvc, q.gotModel)
	}
	var payload struct {
		GeneratedAt string `json:"generated_at"`
		WindowHours int    `json:"window_hours"`
		Entries     []struct {
			GPUModel        string  `json:"gpu_model"`
			Model           string  `json:"model"`
			Requests        uint64  `json:"requests"`
			Providers       uint64  `json:"providers"`
			SuccessRate     float64 `json:"success_rate"`
			AvgTokensPerSec float64 `json:"avg_output_tokens_per_sec"`
			TTFTP50Ms       float64 `json:"ttft_p50_ms"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.WindowHours != 48 || payload.GeneratedAt == "" || len(payload.Entries) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	e := payload.Entries[0]
	if e.GPUModel != "NVIDIA GeForce RTX 4090" || e.Providers != 7 || e.Requests != 100 {
		t.Errorf("entry = %+v", e)
	}
	if e.SuccessRate != 0.95 {
		t.Errorf("success_rate = %v, want 0.95", e.SuccessRate)
	}
}

func TestHandlerDefaultsAndBounds(t *testing.T) {
	q := &fakeQuerier{}
	h := New(q, time.Minute)

	if rec := do(h, "/v1/leaderboard"); rec.Code != http.StatusOK || q.gotHours != defaultWindowHours {
		t.Fatalf("default hours: code=%d hours=%d", rec.Code, q.gotHours)
	}
	for _, target := range []string{
		"/v1/leaderboard?hours=0",
		"/v1/leaderboard?hours=721",
		"/v1/leaderboard?hours=abc",
		"/v1/leaderboard?model=%27%20OR%20%271%27%3D%271",
		"/v1/leaderboard?service=a%20b",
	} {
		if rec := do(h, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", target, rec.Code)
		}
	}
}

func TestHandlerCaches(t *testing.T) {
	q := &fakeQuerier{rows: leaderboardFixture()}
	h := New(q, time.Minute)

	do(h, "/v1/leaderboard?hours=24")
	do(h, "/v1/leaderboard?hours=24")
	if q.calls != 1 {
		t.Fatalf("querier calls = %d, want 1 (cached)", q.calls)
	}
	// A distinct window is a distinct cache entry.
	do(h, "/v1/leaderboard?hours=48")
	if q.calls != 2 {
		t.Fatalf("querier calls = %d, want 2", q.calls)
	}
}

func TestHandlerQuerierFailureIs503(t *testing.T) {
	h := New(&fakeQuerier{err: errors.New("clickhouse down")}, time.Minute)
	if rec := do(h, "/v1/leaderboard"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}
