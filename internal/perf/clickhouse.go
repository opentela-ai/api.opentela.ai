package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// samplesTable matches clickhouse/schema.sql.
const samplesTable = "perf_samples"

// ClickHouseSink batches samples and inserts them over the ClickHouse HTTP
// interface (JSONEachRow) — no native driver, stdlib only. It never blocks
// the request path: Observe is a non-blocking channel send; under sustained
// sink failure samples are dropped (with logged counters), because proxying
// traffic outranks telemetry.
type ClickHouseSink struct {
	url      *url.URL
	database string
	username string
	password string
	client   *http.Client

	queue      chan Sample
	flushEvery time.Duration
	batchSize  int

	dropped uint64 // monotonically increasing, for log visibility
}

// NewClickHouseSink builds a sink. Call Run to start flushing; Observe is a
// no-op-safe (sample-dropping) until then.
func NewClickHouseSink(chURL *url.URL, database, username, password string, flushEvery time.Duration, batchSize, queueSize int) *ClickHouseSink {
	return &ClickHouseSink{
		url:        chURL,
		database:   database,
		username:   username,
		password:   password,
		client:     &http.Client{Timeout: 30 * time.Second},
		queue:      make(chan Sample, queueSize),
		flushEvery: flushEvery,
		batchSize:  batchSize,
	}
}

// Observe implements Recorder. It never blocks the streaming request.
func (s *ClickHouseSink) Observe(sm Sample) {
	select {
	case s.queue <- sm:
	default:
		s.dropped++
		if s.dropped%1000 == 1 {
			log.Printf("perf: sample queue full, dropping samples (total dropped: %d)", s.dropped)
		}
	}
}

// Run flushes queued samples to ClickHouse until ctx is canceled, then makes
// one bounded final flush attempt before returning.
func (s *ClickHouseSink) Run(ctx context.Context) {
	t := time.NewTicker(s.flushEvery)
	defer t.Stop()
	var pending []byte // encoded rows retained after a failed insert
	flush := func(reason string) {
		if len(pending) == 0 {
			n := min(len(s.queue), s.batchSize)
			if n == 0 {
				return
			}
			pending = encodeSamples(s.drain(n))
		}
		if err := s.insert(ctx, pending); err != nil {
			log.Printf("perf: ClickHouse insert failed (%s), retaining batch: %v", reason, err)
			return
		}
		pending = nil
	}
	for {
		select {
		case <-t.C:
			flush("tick")
		case <-ctx.Done():
			if len(pending) == 0 {
				// Drain everything still queued, batching into one final payload.
				for batch := s.drain(s.batchSize); len(batch) > 0; batch = s.drain(s.batchSize) {
					pending = append(pending, encodeSamples(batch)...)
				}
			}
			if len(pending) > 0 {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := s.insert(shutdownCtx, pending); err != nil {
					log.Printf("perf: final ClickHouse flush dropped %d samples: %v", bytes.Count(pending, []byte("\n")), err)
				}
				cancel()
			}
			return
		}
	}
}

// drain pulls up to n queued samples without blocking.
func (s *ClickHouseSink) drain(n int) []Sample {
	out := make([]Sample, 0, n)
	for len(out) < n {
		select {
		case sm := <-s.queue:
			out = append(out, sm)
		default:
			return out
		}
	}
	return out
}

// sampleRow mirrors the perf_samples columns for JSONEachRow inserts.
type sampleRow struct {
	TS                string  `json:"ts"`
	Service           string  `json:"service"`
	Route             string  `json:"route"`
	Model             string  `json:"model"`
	PeerFP            string  `json:"peer_fp"`
	GPUModel          string  `json:"gpu_model"`
	GPUCount          int     `json:"gpu_count"`
	Status            int     `json:"status"`
	ClientAbort       bool    `json:"client_abort"`
	TTFTMs            float64 `json:"ttft_ms"`
	FirstTokenMs      float64 `json:"first_token_ms"`
	TotalMs           float64 `json:"total_ms"`
	InputTokens       int     `json:"input_tokens"`
	CachedInputTokens int     `json:"cached_input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	ResponseBytes     int64   `json:"response_bytes"`
	GPUMs             int64   `json:"gpu_ms"`
}

func encodeSamples(samples []Sample) []byte {
	if len(samples) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, sm := range samples {
		row := sampleRow{
			TS:                sm.TS.UTC().Format("2006-01-02 15:04:05.000"),
			Service:           sm.Service,
			Route:             sm.Route,
			Model:             sm.Model,
			PeerFP:            sm.PeerFP,
			GPUModel:          sm.GPUModel,
			GPUCount:          sm.GPUCount,
			Status:            sm.Status,
			ClientAbort:       sm.ClientAbort,
			TTFTMs:            sm.TTFTMs,
			FirstTokenMs:      sm.FirstTokenMs,
			TotalMs:           sm.TotalMs,
			InputTokens:       sm.InputTokens,
			CachedInputTokens: sm.CachedInputTokens,
			OutputTokens:      sm.OutputTokens,
			ResponseBytes:     sm.ResponseBytes,
			GPUMs:             sm.GPUMs,
		}
		if err := enc.Encode(row); err != nil {
			log.Printf("perf: encoding sample: %v", err)
		}
	}
	return buf.Bytes()
}

func (s *ClickHouseSink) insert(ctx context.Context, rows []byte) error {
	u := *s.url
	q := u.Query()
	q.Set("database", s.database)
	q.Set("query", "INSERT INTO "+samplesTable+" FORMAT JSONEachRow")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(rows))
	if err != nil {
		return err
	}
	if s.username != "" {
		req.Header.Set("X-ClickHouse-User", s.username)
	}
	if s.password != "" {
		req.Header.Set("X-ClickHouse-Key", s.password)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}
