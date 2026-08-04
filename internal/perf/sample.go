// Package perf measures how fast the mesh's GPU workers answer requests and
// records one anonymized sample per proxied inference response.
//
// Attribution: the mesh head stamps every routed response with
// X-Computing-Node (<peer id>); the peer's GPU inventory is public node-table
// data (hardware.gpus). The recorder never persists raw peer ids — only a
// truncated sha256 fingerprint, so the leaderboard can count distinct
// providers without deanonymizing operators. No API keys, prompts, or
// response payloads are stored: only service/model/hardware identity, timing,
// and token counts.
//
// The samples feed ClickHouse (perf_samples; rolled up hourly into
// perf_hourly by clickhouse/schema.sql) and the public leaderboard served by
// internal/leaderboard.
package perf

import (
	"time"
)

// Sample is one completed (or aborted) proxied response.
type Sample struct {
	TS            time.Time
	Service       string // mesh service name, e.g. "llm"
	Route         string // path tail routed to the worker, e.g. "chat/completions"
	Model         string // model reported by the worker's response ("" if unseen)
	PeerFP        string // hex sha256(peer id), truncated — never the raw id
	GPUModel      string // normalized GPU name, "" when unresolvable
	GPUCount      int
	Status        int
	ClientAbort   bool // client disconnected before the upstream body ended
	TTFTMs        float64
	FirstTokenMs  float64 // streaming only; 0 when no content token was observed
	TotalMs       float64
	InputTokens   int
	OutputTokens  int
	ResponseBytes int64
	GPUMs         int64 // worker-reported GPU time (X-Usage-Gpu-Ms); 0 if absent
}

// Recorder accepts finished samples. Implementations must be safe for
// concurrent use: a sample is recorded from the proxy goroutine streaming the
// response, i.e. one per in-flight request.
type Recorder interface {
	Observe(Sample)
}

// nullRecorder drops every sample; used when ClickHouse is not configured.
type nullRecorder struct{}

func (nullRecorder) Observe(Sample) {}

// Null returns a Recorder that discards everything.
func Null() Recorder { return nullRecorder{} }
