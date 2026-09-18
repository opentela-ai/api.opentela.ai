package perf

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTinybirdSinkPostsBatchedSamples(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"successful_rows":2,"quarantined_rows":0}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	sink := NewTinybirdSink(host, "test-token", time.Hour, 1024, 16)
	ts := time.Date(2026, 8, 4, 22, 15, 0, 0, time.UTC)
	samples := []Sample{
		{TS: ts, Service: "llm", Route: "chat/completions", Model: "gpt-4o", PeerFP: "aaaa0000aaaa0001", GPUModel: "NVIDIA GeForce RTX 4090", GPUCount: 1, Status: 200, TTFTMs: 220, FirstTokenMs: 225, TotalMs: 1475, InputTokens: 50, CachedInputTokens: 12, OutputTokens: 100, ResponseBytes: 900, GPUMs: 1400},
		{TS: ts, Service: "llm", Route: "chat/completions", Model: "gpt-4o", PeerFP: "cccc0000cccc0003", GPUModel: "Tesla T4", GPUCount: 1, Status: 500, TTFTMs: 900, FirstTokenMs: 905, TotalMs: 5005, InputTokens: 50, OutputTokens: 100, ResponseBytes: 900, GPUMs: 4800},
	}
	if err := sink.post(context.Background(), encodeSamples(samples)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotPath != "/v0/events" {
		t.Fatalf("path = %q, want /v0/events", gotPath)
	}
	if gotQuery != "name=perf_samples" {
		t.Fatalf("query = %q, want name=perf_samples", gotQuery)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	// Same JSONEachRow encoding as the ClickHouse path: identical schema keys.
	if lines := strings.Split(strings.TrimSuffix(gotBody, "\n"), "\n"); len(lines) != 2 {
		t.Fatalf("body lines = %d, want 2", len(lines))
	}
	if !strings.Contains(gotBody, `"peer_fp":"aaaa0000aaaa0001"`) || !strings.Contains(gotBody, `"ttft_ms":900`) {
		t.Fatalf("body missing expected fields:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, `"cached_input_tokens":12`) {
		t.Fatalf("body missing cached_input_tokens:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, `"ts":"2026-08-04 22:15:00.000"`) {
		t.Fatalf("ts format mismatch:\n%s", gotBody)
	}
}

func TestTinybirdSinkSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)

	sink := NewTinybirdSink(host, "bad-token", time.Hour, 1024, 16)
	if err := sink.post(context.Background(), []byte("{}\n")); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403 surfaced", err)
	}
}
