//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

// ignoreTerminalSignals makes the Connect supervisor ignore ^C
// (os.Interrupt, delivered as CTRL_C_EVENT / CTRL_BREAK_EVENT on
// Windows) while the session-runner child owns the terminal. SIGTSTP
// and SIGQUIT do not exist on Windows, so only Interrupt is handled.
// The returned restore function resets the disposition to its default.
func ignoreTerminalSignals() (restore func()) {
	signal.Ignore(os.Interrupt)
	return func() {
		signal.Reset(os.Interrupt)
	}
}

// terminateSessionRunner stops the session-runner child. Windows has
// no SIGTERM; Kill (TerminateProcess) is the only portable way to end
// the child from the supervisor.
func terminateSessionRunner(p *os.Process) error {
	return p.Kill()
}

// attachSessionPayload hands the payload pipe's read end to the child.
//
// os/exec's ExtraFiles is not supported on Windows, so instead the
// pipe's read handle is marked inheritable (HANDLE_FLAG_INHERIT) and
// listed in SysProcAttr.AdditionalInheritedHandles; inherited handles
// keep their numeric value in the child. The child learns WHICH handle
// to read from through the SessionPayloadHandleEnv environment
// variable. Only that numeric handle value — a non-secret integer —
// travels via the environment; the payload itself (which contains the
// secret session TokenValue) stays inside the pipe, exactly as in the
// unix fd 3 scheme.
func attachSessionPayload(cmd *exec.Cmd, r *os.File) error {
	h := windows.Handle(r.Fd())
	if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return fmt.Errorf("mark session payload handle inheritable: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(
		cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(h))
	if cmd.Env == nil {
		// Not reached from spawnSessionRunner (which always sets Env),
		// but appending to a nil Env would otherwise erase the whole
		// child environment.
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", SessionPayloadHandleEnv, uintptr(h)))
	return nil
}
