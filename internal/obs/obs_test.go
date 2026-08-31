package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordingHandler captures the records it is asked to handle. Its Enabled is
// gated by the configured level so multiHandler's per-handler level check is
// exercised.
type recordingHandler struct {
	min     slog.Level
	records []slog.Record
}

func (r *recordingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= r.min }
func (r *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	r.records = append(r.records, rec)
	return nil
}
func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingHandler) WithGroup(string) slog.Handler      { return r }

// multiHandler fans every handled record out to every handler, and a level
// accepted by any handler must reach all that accept it.
func TestMultiHandlerFansOut(t *testing.T) {
	lo, hi := &recordingHandler{min: slog.LevelDebug}, &recordingHandler{min: slog.LevelWarn}
	m := multiHandler{handlers: []slog.Handler{lo, hi}}

	if !m.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatalf("Enabled(Debug) = false; the debug handler accepts it")
	}

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0)
	if err := m.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(lo.records) != 1 || lo.records[0].Message != "hello" {
		t.Fatalf("low handler = %v, want one \"hello\"", lo.records)
	}
	if len(hi.records) != 0 {
		t.Fatalf("warn handler received %d records, want 0 (level gated)", len(hi.records))
	}

	// A WARN record reaches both.
	r2 := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	if err := m.Handle(context.Background(), r2); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(lo.records) != 2 || len(hi.records) != 1 {
		t.Fatalf("after error: low=%d high=%d, want 2/1", len(lo.records), len(hi.records))
	}
}

// WithAttrs/WithGroup preserve fan-out: every wrapped handler receives
// subsequently handled records.
func TestMultiHandlerWithAttrsAndGroupPreserveFanout(t *testing.T) {
	a, b := &recordingHandler{min: slog.LevelDebug}, &recordingHandler{min: slog.LevelDebug}
	m := multiHandler{handlers: []slog.Handler{a, b}}.WithAttrs([]slog.Attr{slog.String("k", "v")}).WithGroup("g")
	if err := m.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(a.records) != 1 || len(b.records) != 1 {
		t.Fatalf("wrapped fan-out: a=%d b=%d, want 1/1", len(a.records), len(b.records))
	}
}

// TestStdlibLogBridgesThroughNew is the central integration contract:
// after main calls slog.SetDefault(New(...)), the standard library's log
// package (log.Printf and friends across the codebase) is routed through the
// new logger as INFO records — so existing call sites need no change. The
// global state slog.SetDefault mutates is saved and restored.
func TestStdlibLogBridgesThroughNew(t *testing.T) {
	prevSlog := slog.Default()
	prevLogOut := log.Writer()
	prevLogFlags := log.Flags()
	defer func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	}()

	var out bytes.Buffer
	slog.SetDefault(New(Options{Level: slog.LevelInfo, stdout: &out}))

	log.Printf("listening on %s", ":8080")

	line := out.String()
	if !strings.Contains(line, `"msg":"listening on :8080"`) {
		t.Fatalf("stdlib log.Printf did not reach the logger as JSON: %q", line)
	}
	if !strings.Contains(line, `"level":"INFO"`) {
		t.Fatalf("bridged record not at INFO: %q", line)
	}
}

// With no token, New writes structured JSON to stdout and makes no network
// call. (The Better Stack handler is not constructed at all when the token is
// empty, so there is nothing to dial.)
func TestNewStdoutOnlyWhenNoToken(t *testing.T) {
	var out bytes.Buffer
	logger := New(Options{Token: "", Level: slog.LevelInfo, stdout: &out})
	logger.Info("listening", slog.String("addr", ":8080"))

	line := out.String()
	if !strings.Contains(line, `"msg":"listening"`) {
		t.Fatalf("stdout = %q, want a JSON record with msg \"listening\"", line)
	}
	if !strings.Contains(line, `"level":"INFO"`) {
		t.Fatalf("stdout = %q, want level INFO", line)
	}
	if !strings.Contains(line, `"addr":"\u003a8080"`) && !strings.Contains(line, `"addr":":8080"`) {
		t.Fatalf("stdout = %q, want the addr attribute", line)
	}

	// A record below the configured level is not written anywhere.
	out.Reset()
	logger.Debug("hidden")
	if out.Len() != 0 {
		t.Fatalf("debug record reached stdout: %q", out.String())
	}
}

// A text stdout format is honored for local ergonomics; Better Stack still
// receives a structured payload (covered by TestNewShipsToBetterStack).
func TestNewTextFormatOnStdout(t *testing.T) {
	var out bytes.Buffer
	logger := New(Options{Format: "text", Level: slog.LevelInfo, stdout: &out})
	logger.Info("hi")
	if !strings.Contains(out.String(), "level=INFO") || !strings.Contains(out.String(), "msg=hi") {
		t.Fatalf("text stdout = %q, want level=INFO ... msg=hi", out.String())
	}
}

// With a token, the Better Stack leg ships each emitted record as a POST to
// its ingest endpoint (overridden here to a local server) with Bearer auth and
// a JSON-array body, in addition to stdout. This is the end-to-end proof that
// fan-out reaches Better Stack; the handler is fire-and-forget so the request
// is awaited with a short poll.
func TestNewShipsToBetterStack(t *testing.T) {
	var posts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-source-token" {
			t.Errorf("Authorization = %q, want Bearer test-source-token", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var payload []map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("body not a JSON array: %v (body=%q)", err, body)
		} else if len(payload) != 1 || payload[0]["message"] != "shipped" {
			t.Errorf("payload = %v, want one entry with message \"shipped\"", payload)
		}
		atomic.AddInt64(&posts, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out bytes.Buffer
	logger := New(Options{
		Token:               "test-source-token",
		Level:               slog.LevelInfo,
		stdout:              &out,
		betterstackEndpoint: srv.URL,
	})
	logger.Info("shipped", slog.String("svc", "api"))

	if !strings.Contains(out.String(), `"msg":"shipped"`) {
		t.Fatalf("stdout missing the record: %q", out.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&posts) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&posts); got != 1 {
		t.Fatalf("betterstack POSTs = %d after 2s, want 1", got)
	}
}
