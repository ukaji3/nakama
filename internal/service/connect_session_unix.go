//go:build !windows

package service

import (
	"fmt"
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

// attachSessionStatus hands the status pipe's write end to the child as
// the next ExtraFiles descriptor (fd 4 after the payload's fd 3) and
// announces its number through SessionStatusHandleEnv.
func attachSessionStatus(cmd *exec.Cmd, w *os.File) error {
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", SessionStatusHandleEnv, fd))
	return nil
}

// sessionPayloadFD is the file descriptor on which the Connect
// supervisor hands over the session payload: ExtraFiles[0] becomes fd 3
// in the child. The payload contains the secret session TokenValue and
// therefore must not travel via argv or the environment; fd 3 keeps
// stdin free for the interactive session itself.
const sessionPayloadFD = 3

// OpenSessionPayload opens, in the session-runner child, the payload
// pipe inherited from the Connect supervisor on fd 3.
func OpenSessionPayload() (*os.File, error) {
	f := os.NewFile(sessionPayloadFD, "nakama-session-payload")
	if f == nil {
		return nil, fmt.Errorf("session payload fd %d is not open; "+
			"`nakama aws session-runner` is an internal command spawned by `nakama aws connect`",
			sessionPayloadFD)
	}
	return f, nil
}

// closeOnExec keeps an inherited descriptor from leaking into the
// processes the child starts (the plugin runs stty, for example):
// descriptors received through ExtraFiles arrive without FD_CLOEXEC.
func closeOnExec(f *os.File) {
	syscall.CloseOnExec(int(f.Fd()))
}
