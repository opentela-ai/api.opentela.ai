// Package obs wires structured logging (log/slog) to stdout and, when a
// Better Stack source token is configured, to Better Stack Logs
// (https://betterstack.com/logs) via github.com/samber/slog-betterstack — the
// Better Stack Go integration.
//
// The logger returned by New is the single sink for the process. After main
// installs it with slog.SetDefault, the standard library's log package is
// automatically routed through it (see slog.SetDefault), so existing
// log.Printf call sites across the codebase need no change: they become INFO
// records on stdout and, when a token is set, in Better Stack.
//
// When the token is unset the logger writes to stdout only and makes no
// network calls. When set, every emitted record is also shipped to Better
// Stack asynchronously — the handler is fire-and-forget (one POST per record,
// see the upstream TODO around batching), so delivery is best-effort at
// shutdown. That matches the established posture of the perf sink and is
// acceptable for logs.
package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	slogbetterstack "github.com/samber/slog-betterstack"
)

// Options selects the logging sinks and the minimum emitted level.
type Options struct {
	// Token is the Better Stack source token ("$SOURCE_TOKEN"). When empty,
	// Better Stack shipping is disabled and the logger writes to stdout only.
	Token string
	// Level is the minimum record level emitted to every sink. The zero value
	// (slog.LevelInfo) is the default.
	Level slog.Level
	// Format selects the stdout record format: "json" (the default) or "text".
	// Better Stack always receives a structured JSON payload regardless.
	Format string

	// stdout is overridable in tests; nil falls back to os.Stdout.
	stdout io.Writer
	// betterstackEndpoint, when set, overrides the Better Stack ingest URL
	// (default https://in.logs.betterstack.com/). Test-only; tests in this
	// package point it at a local server to assert shipping hermetically.
	betterstackEndpoint string
}

// New returns the process logger: stdout always, Better Stack when a token is
// configured. The returned logger is safe to pass to slog.SetDefault.
//
// Both sinks gate on the same Level, so the configured level controls total
// volume — raising it above INFO silences startup banners emitted through the
// stdlib log bridge as well as direct slog calls.
func New(o Options) *slog.Logger {
	w := o.stdout
	if w == nil {
		w = os.Stdout
	}

	handlerOpts := &slog.HandlerOptions{Level: o.Level}
	var stdoutHandler slog.Handler
	if strings.EqualFold(o.Format, "text") {
		stdoutHandler = slog.NewTextHandler(w, handlerOpts)
	} else {
		stdoutHandler = slog.NewJSONHandler(w, handlerOpts)
	}

	handlers := []slog.Handler{stdoutHandler}
	if o.Token != "" {
		// NewBetterstackHandler panics on an empty token, so it is built only
		// here. It does not dial on construction — the single HTTP POST per
		// record happens asynchronously in Handle — so wiring it is cheap and
		// test-safe. Endpoint/Timeout default to Better Stack's ingest URL
		// and 10s (see the slog-betterstack Option defaults).
		opt := slogbetterstack.Option{
			Level: o.Level,
			Token: o.Token,
		}
		if o.betterstackEndpoint != "" {
			opt.Endpoint = o.betterstackEndpoint
		}
		handlers = append(handlers, opt.NewBetterstackHandler())
	}

	return slog.New(multiHandler{handlers: handlers})
}

// multiHandler fans a single record out to every handler. It is the small
// stdlib gap that slog.HandlerOptions leaves: Go has no built-in multi-sink
// handler, so fan-out (stdout + Better Stack) is implemented here.
type multiHandler struct {
	handlers []slog.Handler
}

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	// Clone per handler: a record may be read after Handle returns (e.g. the
	// Better Stack handler's background POST reads its payload synchronously,
	// but cloning is the documented-safe approach for multi-sink dispatch).
	var first error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return multiHandler{handlers: hs}
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return multiHandler{handlers: hs}
}
