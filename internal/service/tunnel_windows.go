//go:build windows

package service

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/eventlog"
)

// tunnelEventID is the generic event ID used for all daemon messages.
// The "nakama" event source is not pre-registered (registration needs
// administrator rights), so Event Viewer prefixes entries with a
// boilerplate "description not found" note; the message text itself is
// fully readable (ADR 0017).
const tunnelEventID = 1

// openTunnelLogger opens the Windows Application event log. Rotation
// and retention are governed by the OS's event log policy. Falls back
// to stderr when the event log is unavailable.
func openTunnelLogger() tunnelLogger {
	l, err := eventlog.Open("nakama")
	if err != nil {
		return stderrTunnelLogger{}
	}
	return &eventTunnelLogger{l: l}
}

type eventTunnelLogger struct{ l *eventlog.Log }

// Infof logs an informational event.
func (e *eventTunnelLogger) Infof(format string, args ...any) {
	_ = e.l.Info(tunnelEventID, fmt.Sprintf(format, args...))
}

// Errorf logs an error event.
func (e *eventTunnelLogger) Errorf(format string, args ...any) {
	_ = e.l.Error(tunnelEventID, fmt.Sprintf(format, args...))
}

// Close closes the event log handle.
func (e *eventTunnelLogger) Close() error { return e.l.Close() }

// tunnelDaemonSysProcAttr detaches the daemon from the CLI's console:
// DETACHED_PROCESS starts it without a console at all, and
// CREATE_NEW_PROCESS_GROUP keeps console control events (^C) from ever
// being routed to it.
func tunnelDaemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// notifyTunnelStopSignals is a no-op on Windows: a detached daemon has
// no console to deliver control events, so graceful stop happens
// exclusively over the control channel (ADR 0017).
func notifyTunnelStopSignals(func()) (cleanup func()) {
	return func() {}
}
