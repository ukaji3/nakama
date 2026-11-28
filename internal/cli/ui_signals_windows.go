//go:build windows

package cli

import (
	"os"
	"syscall"
)

// panelStopSignals end `nakama ui`: Ctrl-C, and SIGTERM, which Go
// delivers when the console window is closed (CTRL_CLOSE_EVENT), at
// logoff and at shutdown.
func panelStopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
