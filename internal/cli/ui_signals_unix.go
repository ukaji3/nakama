//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// panelStopSignals end `nakama ui`: Ctrl-C, SIGTERM, and SIGHUP when its
// terminal window is closed (the panel then removes its state file).
func panelStopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
