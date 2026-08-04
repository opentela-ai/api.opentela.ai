package perf

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// tinybirdDatasource matches tinybird/datasources/perf_samples.datasource.
const tinybirdDatasource = "perf_samples"

// TinybirdSink batches samples and posts them to the Tinybird Forward Events
// API ({host}/v0/events?name=perf_samples) as NDJSON — same row encoding as
// the ClickHouse sink, stdlib only. Like ClickHouseSink it never blocks the
// request path: proxy traffic outranks telemetry, so a saturated queue drops
// samples with logged counters.
type TinybirdSink struct {
	host   *url.URL
	token  string // static token with APPEND scope on the datasource
	client *http.Client

	queue      chan Sample
	flushEvery time.Duration
	batchSize  int

	dropped uint64
}

// NewTinybirdSink builds a sink. Call Run to start flushing.
func NewTinybirdSink(host *url.URL, token string, flushEvery time.Duration, batchSize, queueSize int) *TinybirdSink {
	return &TinybirdSink{
		host:       host,
		token:      token,
		client:     &http.Client{Timeout: 30 * time.Second},
		queue:      make(chan Sample, queueSize),
		flushEvery: flushEvery,
		batchSize:  batchSize,
	}
}

// Observe implements Recorder. It never blocks the streaming request.
func (s *TinybirdSink) Observe(sm Sample) {
	select {
	case s.queue <- sm:
	default:
		s.dropped++
		if s.dropped%1000 == 1 {
			log.Printf("perf: Tinybird sample queue full, dropping samples (total dropped: %d)", s.dropped)
		}
	}
}

// Run flushes queued samples to Tinybird until ctx is canceled, then makes
// one bounded final flush attempt before returning.
func (s *TinybirdSink) Run(ctx context.Context) {
	t := time.NewTicker(s.flushEvery)
	defer t.Stop()
	var pending []byte // encoded rows retained after a failed post
	flush := func(reason string) {
		if len(pending) == 0 {
			n := min(len(s.queue), s.batchSize)
			if n == 0 {
				return
			}
			pending = encodeSamples(s.drain(n))
		}
		if err := s.post(ctx, pending); err != nil {
			log.Printf("perf: Tinybird insert failed (%s), retaining batch: %v", reason, err)
			return
		}
		pending = nil
	}
	for {
		select {
		case <-t.C:
			flush("tick")
		case <-ctx.Done():
			for batch := s.drain(s.batchSize); len(batch) > 0; batch = s.drain(s.batchSize) {
				pending = append(pending, encodeSamples(batch)...)
			}
			if len(pending) > 0 {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := s.post(shutdownCtx, pending); err != nil {
					log.Printf("perf: final Tinybird flush dropped %d samples: %v", bytes.Count(pending, []byte("\n")), err)
				}
				cancel()
			}
			return
		}
	}
}

func (s *TinybirdSink) drain(n int) []Sample {
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

func (s *TinybirdSink) post(ctx context.Context, rows []byte) error {
	u := *s.host
	u.Path = "/v0/events"
	q := u.Query()
	q.Set("name", tinybirdDatasource)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(rows))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	// Partially rejected payloads (quarantined rows) deserve visibility.
	if rest := bytes.TrimSpace(body); len(rest) > 0 && !bytes.Contains(rest, []byte(`"quarantined_rows":0`)) {
		log.Printf("perf: Tinybird events response: %s", rest)
	}
	return nil
}
