//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// spawnConfiguredShell starts an interactive shell with the given
// environment, inheriting this console, and returns its exit code.
// The shell is the parent shell's kind unless overridden (ADR 0018).
func spawnConfiguredShell(env []string, override string) (int, error) {
	shellPath := override
	if shellPath == "" {
		shellPath = defaultShell(parentProcessExe(), os.Getenv("ComSpec"))
	}
	return runShell(shellPath, nil, env, "")
}

// runShell runs an interactive shell attached to this console and
// returns its exit code. dir "" keeps the current directory.
//
// Ctrl-C and Ctrl-Break are delivered to every process attached to the
// console, nakama included, and a Go program exits on them unless
// os.Interrupt is registered with signal.Notify (os/signal, "Windows").
// signal.Ignore does not prevent that on Windows: the runtime's
// sigignore is a no-op there, so the event falls through to the
// default handler, which ends the process. nakama would then return to
// its parent shell while the subshell still reads the same console.
// The events are received and dropped instead; the shell handles its
// own Ctrl-C.
func runShell(path string, args, env []string, dir string) (int, error) {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	c := exec.Command(path, args...) // #nosec G204 G702 -- the path is the parent shell's image, ComSpec, Windows PowerShell, or the explicit --shell value
	c.Env = env
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("start shell %s: %w", path, err)
	}
	return 0, nil
}

// windowsPowerShellPath returns Windows PowerShell's executable in the
// system directory, which every supported Windows has, or "" if it is
// missing. The system directory is asked from Windows rather than built
// from %SystemRoot%, and the shell is not looked up on PATH, so neither
// the environment nor the folder nakama.exe was started from can
// substitute another program.
func windowsPowerShellPath() string {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	p := filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// parentProcessExe returns the executable image path of the parent
// process ("" when it cannot be determined; defaultShell then falls
// back to ComSpec).
func parentProcessExe() string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getppid()))
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var buf [windows.MAX_PATH + 1]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}
