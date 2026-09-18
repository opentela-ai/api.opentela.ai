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

func TestUsageHandlerDefaultsAndShape(t *testing.T) {
	q := &fakeUsageQuerier{rows: usageFixture()}
	h := NewUsage(q, time.Minute)

	rec := do(h, "/v1/token-usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if q.gotDays != defaultWindowDays || q.gotSuccess != true || q.gotSvc != "" || q.gotModel != "" {
		t.Errorf("querier args = %d/%q/%q/%v", q.gotDays, q.gotSvc, q.gotModel, q.gotSuccess)
	}
	var payload struct {
		GeneratedAt string `json:"generated_at"`
		WindowDays  int    `json:"window_days"`
		SuccessOnly bool   `json:"success_only"`
		Entries     []struct {
			Day               string `json:"day"`
			Model             string `json:"model"`
			Requests          uint64 `json:"requests"`
			InputTokens       uint64 `json:"input_tokens"`
			CachedInputTokens uint64 `json:"cached_input_tokens"`
			OutputTokens      uint64 `json:"output_tokens"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if payload.WindowDays != 30 || payload.SuccessOnly != true {
		t.Errorf("window/success = %d/%v", payload.WindowDays, payload.SuccessOnly)
	}
	if len(payload.Entries) != 2 ||
		payload.Entries[0].Day != "2026-09-18" || payload.Entries[0].Model != "glm-5.3-flash" ||
		payload.Entries[0].Requests != 91 || payload.Entries[0].InputTokens != 9143051 ||
		payload.Entries[0].CachedInputTokens != 40 || payload.Entries[0].OutputTokens != 81945 {
		t.Errorf("entries = %+v", payload.Entries)
	}
	if payload.GeneratedAt == "" {
		t.Error("generated_at empty")
	}
}

func TestUsageHandlerParams(t *testing.T) {
	q := &fakeUsageQuerier{rows: usageFixture()}
	h := NewUsage(q, time.Minute)

	rec := do(h, "/v1/token-usage?days=7&success_only=0&service=chat&model=gpt-4o")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if q.gotDays != 7 || !q.gotSuccess == false || q.gotSvc != "chat" || q.gotModel != "gpt-4o" {
		t.Errorf("querier args = %d/%q/%q/%v", q.gotDays, q.gotSvc, q.gotModel, q.gotSuccess)
	}

	for _, target := range []string{
		"/v1/token-usage?days=0",
		"/v1/token-usage?days=31",
		"/v1/token-usage?days=abc",
		"/v1/token-usage?success_only=yes",
		"/v1/token-usage?model=bad%20name",
		"/v1/token-usage?service=%3Cscript%3E",
	} {
		if rec := do(h, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", target, rec.Code)
		}
	}
}

func TestUsageHandlerCachesPerWindow(t *testing.T) {
	q := &fakeUsageQuerier{rows: usageFixture()}
	h := NewUsage(q, time.Minute)
	do(h, "/v1/token-usage?days=7")
	do(h, "/v1/token-usage?days=7")
	if q.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cache)", q.calls)
	}
	do(h, "/v1/token-usage?days=7&success_only=0")
	if q.calls != 2 {
		t.Fatalf("calls = %d, want 2 (success_only is a distinct key)", q.calls)
	}
}

func TestUsageHandlerQuerierErrorIs503(t *testing.T) {
	h := NewUsage(&fakeUsageQuerier{err: errors.New("down")}, time.Minute)
	if rec := do(h, "/v1/token-usage"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
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
