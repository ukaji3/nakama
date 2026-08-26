//go:build !windows

package service

import (
	"fmt"
	"log/syslog"
	"os"
	"os/signal"
	"syscall"
)

// openTunnelLogger connects to the local syslog daemon (facility
// LOG_USER, tag nakama-tunnel). journald / rsyslog / macOS unified
// logging handle rotation and retention (ADR 0017). When syslog is
// unavailable (e.g. a minimal container), fall back to stderr so a
// foreground debug run still produces output.
func openTunnelLogger() tunnelLogger {
	w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_USER, "nakama-tunnel")
	if err != nil {
		return stderrTunnelLogger{}
	}
	return &syslogTunnelLogger{w: w}
}

type syslogTunnelLogger struct{ w *syslog.Writer }

// Infof logs at severity info.
func (l *syslogTunnelLogger) Infof(format string, args ...any) {
	_ = l.w.Info(fmt.Sprintf(format, args...))
}

// Errorf logs at severity err.
func (l *syslogTunnelLogger) Errorf(format string, args ...any) {
	_ = l.w.Err(fmt.Sprintf(format, args...))
}

// Close closes the syslog connection.
func (l *syslogTunnelLogger) Close() error { return l.w.Close() }

// tunnelDaemonSysProcAttr detaches the daemon from the CLI's session
// so it survives the terminal: a new session (Setsid) has no
// controlling terminal and receives no terminal-generated signals.
func tunnelDaemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// notifyTunnelStopSignals routes SIGTERM/SIGINT to the daemon's
// graceful stop path, so `kill <pid>` behaves like `tunnel stop`
// (minus the client-side state-file wait). The returned cleanup stops
// signal delivery.
func notifyTunnelStopSignals(stop func()) (cleanup func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		if _, ok := <-ch; ok {
			stop()
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}
