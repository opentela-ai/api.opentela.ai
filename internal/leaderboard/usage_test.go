package leaderboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeUsageQuerier struct {
	rows []UsageRow
	err  error

	calls      int
	gotDays    int
	gotSvc     string
	gotModel   string
	gotSuccess bool
}

func (f *fakeUsageQuerier) TokenUsage(_ context.Context, days int, service, model string, successOnly bool) ([]UsageRow, error) {
	f.calls++
	f.gotDays, f.gotSvc, f.gotModel, f.gotSuccess = days, service, model, successOnly
	return f.rows, f.err
}

func usageFixture() []UsageRow {
	return []UsageRow{
		{Day: "2026-09-18", Model: "glm-5.3-flash", Requests: 91, InputTokens: 9143051, CachedInputTokens: 40, OutputTokens: 81945},
		{Day: "2026-09-17", Model: "gpt-4o", Requests: 12, InputTokens: 5300, CachedInputTokens: 0, OutputTokens: 910},
	}
}

func TestWindowDays(t *testing.T) {
	for hours, want := range map[int]int{1: 1, 24: 1, 25: 2, 168: 7, 720: 30} {
		if got := windowDays(hours); got != want {
			t.Errorf("windowDays(%d) = %d, want %d", hours, got, want)
		}
	}
}

func TestLeaderboardWithUsage(t *testing.T) {
	q := &fakeQuerier{rows: leaderboardFixture()}
	u := &fakeUsageQuerier{rows: usageFixture()}
	h := NewWithUsage(q, u, time.Minute)

	rec := do(h, "/v1/leaderboard?hours=48&success_only=0&service=chat&model=gpt-4o")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if u.gotDays != 2 || u.gotSuccess != false || u.gotSvc != "chat" || u.gotModel != "gpt-4o" {
		t.Errorf("usage querier args = %d/%q/%q/%v", u.gotDays, u.gotSvc, u.gotModel, u.gotSuccess)
	}
	var payload struct {
		GeneratedAt string `json:"generated_at"`
		WindowHours int    `json:"window_hours"`
		WindowDays  int    `json:"window_days"`
		SuccessOnly bool   `json:"success_only"`
		Entries     []struct {
			GPUModel string `json:"gpu_model"`
		} `json:"entries"`
		Usage []struct {
			Day               string `json:"day"`
			Model             string `json:"model"`
			Requests          uint64 `json:"requests"`
			InputTokens       uint64 `json:"input_tokens"`
			CachedInputTokens uint64 `json:"cached_input_tokens"`
			OutputTokens      uint64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if payload.WindowHours != 48 || payload.WindowDays != 2 || payload.SuccessOnly != false {
		t.Errorf("windows/success = %d/%d/%v", payload.WindowHours, payload.WindowDays, payload.SuccessOnly)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].GPUModel != "NVIDIA GeForce RTX 4090" {
		t.Errorf("entries = %+v", payload.Entries)
	}
	if len(payload.Usage) != 2 ||
		payload.Usage[0].Day != "2026-09-18" || payload.Usage[0].Requests != 91 ||
		payload.Usage[0].InputTokens != 9143051 || payload.Usage[0].CachedInputTokens != 40 ||
		payload.Usage[0].OutputTokens != 81945 {
		t.Errorf("usage = %+v", payload.Usage)
	}
}

func TestLeaderboardWithoutUsageOmitsSection(t *testing.T) {
	h := New(&fakeQuerier{rows: leaderboardFixture()}, time.Minute)
	rec := do(h, "/v1/leaderboard?hours=168")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, key := range []string{`"usage"`, `"window_days"`, `"success_only"`} {
		if strings.Contains(body, key) {
			t.Errorf("perf-only response carries %s: %s", key, body)
		}
	}
}

func TestLeaderboardUsageDegradesGracefully(t *testing.T) {
	h := NewWithUsage(&fakeQuerier{rows: leaderboardFixture()}, &fakeUsageQuerier{err: errors.New("pipe down")}, time.Minute)
	rec := do(h, "/v1/leaderboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage failure must not fail the leaderboard, code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"usage": [`) || strings.Contains(rec.Body.String(), `"usage":[`) {
		t.Fatalf("degraded response must omit usage: %s", rec.Body.String())
	}
	var payload struct {
		Entries []json.RawMessage `json:"entries"`
		Usage   []json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(payload.Entries) != 1 || payload.Usage != nil {
		t.Errorf("entries = %d, usage = %v", len(payload.Entries), payload.Usage)
	}
}

func TestLeaderboardUsageCacheKeys(t *testing.T) {
	q := &fakeQuerier{rows: leaderboardFixture()}
	u := &fakeUsageQuerier{rows: usageFixture()}
	h := NewWithUsage(q, u, time.Minute)

	do(h, "/v1/leaderboard?hours=48")
	do(h, "/v1/leaderboard?hours=48")
	if u.calls != 1 || q.calls != 1 {
		t.Fatalf("calls = %d/%d, want 1/1 (cache)", q.calls, u.calls)
	}
	do(h, "/v1/leaderboard?hours=48&success_only=0")
	if u.calls != 2 {
		t.Fatalf("usage calls = %d, want 2 (success_only is a distinct key)", u.calls)
	}
}

func TestLeaderboardBadSuccessOnly(t *testing.T) {
	h := NewWithUsage(&fakeQuerier{}, &fakeUsageQuerier{}, time.Minute)
	if rec := do(h, "/v1/leaderboard?success_only=yes"); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestUsageTinybird(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"meta": [],
			"data": [
				{"day":"2026-09-18","model":"glm-5.3-flash","requests":91,"input_tokens":9143051,"cached_input_tokens":0,"output_tokens":81945}
			],
			"rows": 1
		}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	q := NewUsageTinybird(host, "usage-read-token")
	rows, err := q.TokenUsage(context.Background(), 30, "", "glm-5.3-flash", true)
	if err != nil {
		t.Fatalf("TokenUsage: %v", err)
	}
	if gotPath != "/v0/pipes/"+usagePipe+".json" {
		t.Fatalf("path = %q", gotPath)
	}
	for _, want := range []string{"token=usage-read-token", "days=30", "model=glm-5.3-flash"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
	if strings.Contains(gotQuery, "success_only") {
		t.Fatalf("default success_only should not be sent, got %q", gotQuery)
	}
	if len(rows) != 1 || rows[0].Day != "2026-09-18" || rows[0].Requests != 91 ||
		rows[0].InputTokens != 9143051 || rows[0].CachedInputTokens != 0 || rows[0].OutputTokens != 81945 {
		t.Fatalf("rows = %+v", rows)
	}

	// success_only=0 must be forwarded so raw totals match the pipe default off.
	if _, err := q.TokenUsage(context.Background(), 7, "chat", "", false); err != nil {
		t.Fatalf("TokenUsage: %v", err)
	}
	if !strings.Contains(gotQuery, "success_only=0") || !strings.Contains(gotQuery, "days=7") || !strings.Contains(gotQuery, "service=chat") {
		t.Fatalf("query %q missing explicit params", gotQuery)
	}
}

func TestUsageClickHouseQuery(t *testing.T) {
	var gotBody, gotDB string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotDB = r.URL.Query().Get("database")
		_, _ = w.Write([]byte(`{"day":"2026-09-18","model":"glm-5.3-flash","requests":91,"input_tokens":9143051,"cached_input_tokens":40,"output_tokens":81945}
`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	q := NewClickHouse(host, "default", "user", "pass")
	rows, err := q.TokenUsage(context.Background(), 30, "", "glm-5.3-flash", true)
	if err != nil {
		t.Fatalf("TokenUsage: %v", err)
	}
	if gotDB != "default" {
		t.Errorf("database = %q", gotDB)
	}
	// Settlement predicate must be row-level against perf_samples, not the rollup.
	for _, want := range []string{
		"FROM perf_samples",
		"ts >= now() - INTERVAL 30 DAY",
		"status >= 200 AND status < 300",
		"NOT client_abort",
		"input_tokens > 0 OR cached_input_tokens > 0 OR output_tokens > 0",
		"model = 'glm-5.3-flash'",
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("query %q missing %q", gotBody, want)
		}
	}
	if len(rows) != 1 || rows[0].Requests != 91 || rows[0].CachedInputTokens != 40 {
		t.Fatalf("rows = %+v", rows)
	}

	// success_only=false drops the settlement predicate and keeps the filters.
	if _, err := q.TokenUsage(context.Background(), 7, "chat", "", false); err != nil {
		t.Fatalf("TokenUsage: %v", err)
	}
	if strings.Contains(gotBody, "status >= 200") {
		t.Fatalf("success_only=false must not filter status, got %q", gotBody)
	}
	if !strings.Contains(gotBody, "INTERVAL 7 DAY") || !strings.Contains(gotBody, "service = 'chat'") {
		t.Fatalf("query %q missing window/service", gotBody)
	}
}
