//go:build !windows

package service

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ignoreTerminalSignals makes the Connect supervisor ignore
// terminal-generated signals (SIGINT via ^C, SIGTSTP via ^Z, SIGQUIT
// via ^\) while the session-runner child owns the terminal. The
// returned restore function resets the dispositions to their defaults.
func ignoreTerminalSignals() (restore func()) {
	signal.Ignore(os.Interrupt, syscall.SIGTSTP, syscall.SIGQUIT)
	return func() {
		signal.Reset(os.Interrupt, syscall.SIGTSTP, syscall.SIGQUIT)
	}
}

// terminateSessionRunner asks the session-runner child to terminate.
// SIGTERM gives the child a chance to run its cleanup paths.
func terminateSessionRunner(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// attachSessionPayload hands the payload pipe's read end to the child
// as fd 3 via ExtraFiles (ExtraFiles[0] becomes fd 3). fd 3 keeps
// stdin free for the interactive session itself, and the payload
// (which contains the secret session TokenValue) never travels via
// argv, the environment, or disk.
func attachSessionPayload(cmd *exec.Cmd, r *os.File) error {
	cmd.ExtraFiles = []*os.File{r}
	return nil
}
