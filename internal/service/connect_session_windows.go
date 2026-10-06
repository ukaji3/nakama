//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
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
	return inheritHandle(cmd, r, SessionPayloadHandleEnv, "session payload")
}

// attachSessionStatus hands the status pipe's write end to the child
// the same way, announced through SessionStatusHandleEnv.
func attachSessionStatus(cmd *exec.Cmd, w *os.File) error {
	return inheritHandle(cmd, w, SessionStatusHandleEnv, "session status")
}

// inheritHandle marks f's handle inheritable, lists it in
// AdditionalInheritedHandles (os/exec then passes exactly the listed
// handles), and announces its numeric value in envName.
func inheritHandle(cmd *exec.Cmd, f *os.File, envName, what string) error {
	h := windows.Handle(f.Fd())
	if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return fmt.Errorf("mark %s handle inheritable: %w", what, err)
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
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envName, uintptr(h)))
	return nil
}

// OpenSessionPayload opens, in the session-runner child, the payload
// pipe inherited from the Connect supervisor. On Windows ExtraFiles is
// not supported, so the supervisor marks the pipe's read handle
// inheritable and announces its numeric value (a non-secret integer)
// through SessionPayloadHandleEnv; the payload itself, which contains
// the secret session TokenValue, stays inside the pipe and never
// travels via argv or the environment.
func OpenSessionPayload() (*os.File, error) {
	v := os.Getenv(SessionPayloadHandleEnv)
	if v == "" {
		return nil, fmt.Errorf("%s is not set; "+
			"`nakama aws session-runner` is an internal command spawned by `nakama aws connect`",
			SessionPayloadHandleEnv)
	}
	h, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", SessionPayloadHandleEnv, err)
	}
	f := os.NewFile(uintptr(h), "nakama-session-payload")
	if f == nil {
		return nil, fmt.Errorf("session payload handle %s is not a valid handle", v)
	}
	return f, nil
}

// closeOnExec is a no-op on Windows: os/exec passes a child only the
// handles listed in AdditionalInheritedHandles (plus stdio), so an
// inherited handle cannot leak further.
func closeOnExec(*os.File) {}
