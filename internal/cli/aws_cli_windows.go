//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

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
	c := exec.Command(shellPath) // #nosec G204 -- the path is the parent shell's image, ComSpec, or the explicit --shell value
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("start shell %s: %w", shellPath, err)
	}
	return 0, nil
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
