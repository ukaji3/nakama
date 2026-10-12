//go:build windows

package cli

import (
	"os"
	"os/signal"

	"github.com/ukaji3/nakama/internal/cliproxy"
)

// HandleDoubleClick opens the double-click window's shell when
// nakama.exe was started from Explorer without arguments (ADR 0022).
// It reports whether it did, with the exit code to end nakama with;
// otherwise the caller runs the command line as usual.
func HandleDoubleClick() (code int, handled bool) {
	if !isDoubleClick(os.Args, parentProcessExe()) {
		return 0, false
	}
	// Registered for the rest of the process, which ends with the
	// shell: Ctrl-C must neither close the window during the detection
	// (it skips the detection instead) nor between it and the shell.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)

	exeDir, err := executableDir()
	if err != nil {
		exeDir = ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	shell, args := launcherShell(windowsPowerShellPath(), os.Getenv("ComSpec"))
	l := &doubleClickLauncher{
		out:        os.Stdout,
		in:         os.Stdin,
		environ:    os.Environ(),
		exeDir:     exeDir,
		homeDir:    home,
		shell:      shell,
		shellArgs:  args,
		detect:     cliproxy.NewDetector().Detect,
		interrupts: interrupts,
		run:        runShell,
	}
	return l.launch(), true
}
