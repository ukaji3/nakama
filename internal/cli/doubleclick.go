package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ukaji3/nakama/internal/cliproxy"
	"github.com/ukaji3/nakama/internal/version"
)

// Double-clicking nakama.exe in Explorer (ADR 0022). Windows has no
// per-user folder that is on PATH by default, so instead of cobra's
// "This is a command line tool" splash, nakama keeps the window Windows
// opened and starts Windows PowerShell in it, with nakama's folder in
// front of PATH and, like `nakama aws cli shell`, the proxy and CA
// settings for the aws CLI. Everything is scoped to that window: the
// registry and the user's environment variables are not written.

// isDoubleClick reports whether nakama was started by double-clicking
// it in Explorer: no arguments, and Explorer as the parent process (the
// test cobra's mousetrap uses). Started from Explorer with arguments (a
// shortcut, say), nakama keeps cobra's behavior: the mousetrap splash.
func isDoubleClick(args []string, parentExe string) bool {
	return len(args) == 1 && shellBaseName(parentExe) == "explorer.exe"
}

// launcherShell picks the double-click window's shell: Windows
// PowerShell (powershellPath; "" when it is missing) without its
// copyright banner, so nakama's summary stays on screen, falling back
// to ComSpec and then cmd.exe.
func launcherShell(powershellPath, comspec string) (string, []string) {
	if powershellPath != "" {
		return powershellPath, []string{"-NoLogo"}
	}
	return defaultShell("", comspec), nil
}

// doubleClickLauncher runs the double-click window. Its dependencies
// are fields so the flow is tested on every OS.
type doubleClickLauncher struct {
	out io.Writer
	// in is read only to keep an error on screen until Enter.
	in      io.Reader
	environ []string
	// exeDir is nakama's folder ("" when unknown: PATH is unchanged).
	exeDir string
	// homeDir is where the shell starts ("" keeps the current folder,
	// which Explorer sets to nakama's).
	homeDir   string
	shell     string
	shellArgs []string
	detect    func(context.Context) (*cliproxy.Detection, error)
	// interrupts delivers Ctrl-C; one during the detection skips it.
	interrupts <-chan os.Signal
	run        func(path string, args, env []string, dir string) (int, error)
}

// launch prints what the window is set up with, starts the shell and
// returns the exit code for nakama. A failed detection still opens the
// shell, with PATH only: closing a window the user just opened with
// nothing usable in it would be worse than a shell without the aws CLI
// settings, which nakama itself does not need (ADR 0015).
func (l *doubleClickLauncher) launch() int {
	_, _ = fmt.Fprintf(l.out, "nakama %s\n", version.String())
	if l.exeDir != "" {
		_, _ = fmt.Fprintf(l.out, "path:   %s added in front of PATH (this window only)\n", l.exeDir)
	} else {
		_, _ = fmt.Fprintf(l.out, "path:   WARNING: nakama's folder is unknown; PATH is unchanged\n")
	}
	_, _ = fmt.Fprintf(l.out, "detecting the proxy settings for the aws CLI (Ctrl-C skips this)...\n")

	det, interrupted, err := l.detectInterruptibly()
	var env []string
	if err != nil {
		if interrupted {
			_, _ = fmt.Fprintf(l.out, "proxy:  detection skipped (Ctrl-C)\n")
		} else {
			_, _ = fmt.Fprintf(l.out, "proxy:  WARNING: detection failed: %v\n", explainDetectionError(err))
		}
		_, _ = fmt.Fprintf(l.out, "        This window has no proxy settings for the aws CLI. nakama itself finds the proxy on its own.\n")
		_, _ = fmt.Fprintf(l.out, "        To retry, run `nakama aws cli shell` in this window.\n")
		env = l.environ
		if l.exeDir != "" {
			env = prependWindowsPath(env, l.exeDir)
		}
	} else {
		printDetection(l.out, det)
		env = cliShellEnv(l.environ, det, l.exeDir)
	}
	_, _ = fmt.Fprintf(l.out, "\nType `nakama --help` to see the commands, and `exit` to close this window.\n\n")

	code, err := l.run(l.shell, l.shellArgs, env, l.homeDir)
	if err != nil {
		_, _ = fmt.Fprintf(l.out, "error: %v\n", err)
		_, _ = fmt.Fprintf(l.out, "Press Enter to close this window.")
		_, _ = bufio.NewReader(l.in).ReadString('\n')
		return 1
	}
	return code
}

// detectInterruptibly runs the detection, cancelling it on Ctrl-C;
// interrupted reports that the error is that cancellation.
func (l *doubleClickLauncher) detectInterruptibly() (det *cliproxy.Detection, interrupted bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-l.interrupts:
			cancel()
		case <-done:
		}
	}()
	det, err = l.detect(ctx)
	return det, err != nil && ctx.Err() != nil, err
}
