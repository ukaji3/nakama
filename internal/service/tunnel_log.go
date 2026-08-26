package service

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// tunnelLogger is the daemon's log sink. The production
// implementations write to the OS-standard logging facility so
// rotation and retention are the OS's problem (ADR 0017): syslog on
// unix (tunnel_log_unix.go), the Windows Application event log
// (tunnel_log_windows.go).
type tunnelLogger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
	Close() error
}

// stderrTunnelLogger is the fallback when the OS log is unavailable
// (e.g. a container without /dev/log). For a detached daemon stderr is
// discarded, but a daemon run in the foreground for debugging still
// gets readable output.
type stderrTunnelLogger struct{}

// Infof writes an info line to stderr.
func (stderrTunnelLogger) Infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nakama-tunnel: "+format+"\n", args...)
}

// Errorf writes an error line to stderr.
func (stderrTunnelLogger) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nakama-tunnel: error: "+format+"\n", args...)
}

// Close is a no-op for the stderr fallback.
func (stderrTunnelLogger) Close() error { return nil }

// lineWriter adapts an io.Writer sink to per-line callbacks, buffering
// partial writes until a newline arrives. The daemon uses it to funnel
// session-runner child stdout/stderr into the OS log.
type lineWriter struct {
	mu   sync.Mutex
	buf  strings.Builder
	emit func(line string)
}

func newLineWriter(emit func(line string)) *lineWriter {
	return &lineWriter{emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			line := strings.TrimRight(w.buf.String(), "\r")
			w.buf.Reset()
			if line != "" {
				w.emit(line)
			}
			continue
		}
		w.buf.WriteByte(b)
	}
	return len(p), nil
}

// Flush emits any buffered partial line.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		line := strings.TrimRight(w.buf.String(), "\r")
		w.buf.Reset()
		if line != "" {
			w.emit(line)
		}
	}
}
