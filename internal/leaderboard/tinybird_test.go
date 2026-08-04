package leaderboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTinybirdLeaderboard(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"meta": [],
			"data": [
				{"gpu_model":"NVIDIA GeForce RTX 4090","model":"gpt-4o","requests":101,"server_errors":1,"client_aborts":0,"providers":8,"avg_tps":55.94,"p50_tps":56,"ttft_p50_ms":224,"ttft_p90_ms":244,"ttft_p99_ms":248},
				{"gpu_model":"Tesla T4","model":"gpt-4o-mini","requests":21,"server_errors":0,"client_aborts":0,"providers":3,"avg_tps":17.95,"p50_tps":18.1,"ttft_p50_ms":225,"ttft_p90_ms":245,"ttft_p99_ms":249}
			],
			"rows": 2,
			"statistics": {"elapsed": 0.01}
		}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	q := NewTinybird(host, "read-token")
	rows, err := q.Leaderboard(context.Background(), 336, "llm", "gpt-4o")
	if err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if gotPath != "/v0/pipes/"+tinybirdPipe+".json" {
		t.Fatalf("path = %q", gotPath)
	}
	for _, want := range []string{"token=read-token", "hours=336", "service=llm", "model=gpt-4o"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].GPUModel != "NVIDIA GeForce RTX 4090" || rows[0].Requests != 101 || rows[0].Providers != 8 ||
		rows[0].ServerErrors != 1 || rows[0].AvgTokensPerSec != 55.94 || rows[0].P50TokensPerSec != 56 ||
		rows[0].TTFTP50Ms != 224 || rows[0].TTFTP90Ms != 244 || rows[0].TTFTP99Ms != 248 {
		t.Fatalf("row[0] = %+v", rows[0])
	}
	if rows[1].GPUModel != "Tesla T4" || rows[1].Requests != 21 || rows[1].AvgTokensPerSec != 17.95 {
		t.Fatalf("row[1] = %+v", rows[1])
	}
}

func TestTinybirdLeaderboardOmitsEmptyFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":[],"rows":0}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	q := NewTinybird(host, "tok")
	if _, err := q.Leaderboard(context.Background(), 168, "", ""); err != nil {
		t.Fatalf("Leaderboard: %v", err)
	}
	if strings.Contains(gotQuery, "service=") || strings.Contains(gotQuery, "model=") {
		t.Fatalf("empty filters leaked into query %q", gotQuery)
	}
}

func TestTinybirdLeaderboardServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	q := NewTinybird(host, "tok")
	if _, err := q.Leaderboard(context.Background(), 168, "", ""); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want 500 surfaced", err)
	}
}
