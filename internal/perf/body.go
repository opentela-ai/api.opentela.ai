package perf

import (
	"io"
	"time"
)

// measureBody wraps a proxied response body, measuring what the client
// actually receives: first body byte (TTFT), first generated-content token,
// end of stream, and byte count. It finalizes exactly once, on EOF or on the
// first Close, and hands the observation to done. A body closed before EOF
// (client disconnect, upstream reset) is finalized as a client abort.
type measureBody struct {
	rc    io.ReadCloser
	start time.Time
	now   func() time.Time
	done  func(*measureBody)

	parser *streamParser

	firstByte  bool
	firstToken bool
	eof        bool
	finished   bool
	bytes      int64
	ttft       time.Duration
	firstTok   time.Duration
}

func wrapBody(rc io.ReadCloser, contentType string, start time.Time, done func(*measureBody)) *measureBody {
	return &measureBody{
		rc:     rc,
		start:  start,
		now:    time.Now,
		done:   done,
		parser: newStreamParser(contentType),
	}
}

func (m *measureBody) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	if n > 0 {
		now := m.now()
		m.bytes += int64(n)
		if !m.firstByte {
			m.firstByte = true
			m.ttft = now.Sub(m.start)
		}
		m.parser.feed(p[:n])
		if !m.firstToken && m.parser.sawToken {
			m.firstToken = true
			m.firstTok = now.Sub(m.start)
		}
	}
	if err == io.EOF {
		m.eof = true
		m.parser.finish()
		m.finalize()
	}
	return n, err
}

func (m *measureBody) Close() error {
	m.finalize()
	return m.rc.Close()
}

func (m *measureBody) finalize() {
	if m.finished {
		return
	}
	m.parser.finish() // harmless when already run at EOF
	m.finished = true
	m.done(m)
}

// total is measured from upstream response headers (ModifyResponse time) to
// body end — the full client-visible response latency.
func (m *measureBody) total() time.Duration { return m.now().Sub(m.start) }
