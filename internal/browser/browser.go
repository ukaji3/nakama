// Package browser launches the user's web browser in a detached,
// non-blocking way. It exists so that `nakama aws open` (ADR 0012) can
// hand a URL to whatever browser the desktop environment provides
// without tying the child process to the CLI's lifetime, and so that
// callers can distinguish "no browser launcher available on this
// machine" (fall back to printing the URL) from a genuine launch
// failure.
//
// The launcher is an interface with an exec-hook-injectable default
// implementation, keeping unit tests free of real process spawning.
package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Launcher opens a URL in the user's browser. Implementations must be
// non-blocking: Open returns once the launcher process has started
// (or immediately failed), not when the browser exits.
type Launcher interface {
	Open(ctx context.Context, url string) error
}

// ExecLauncher is the default Launcher. It resolves the launcher
// command in this order of precedence:
//
//  1. OverrideCommand (the `--browser` flag of `nakama aws open`),
//  2. the $BROWSER environment variable,
//  3. a per-OS default: `open` (darwin), `xdg-open` (linux),
//     `rundll32 url.dll,FileProtocolHandler` (windows, best effort).
//
// For 1 and 2 the value is split on whitespace; the first token is the
// executable and the rest become leading arguments. The URL is always
// appended as the last argument.
//
// When no launcher can be resolved (unknown OS, or the command is not
// in PATH), the returned error wraps exec.ErrNotFound so callers can
// detect the condition with errors.Is and fall back to printing the
// URL instead of failing the whole command.
type ExecLauncher struct {
	// OverrideCommand, when non-empty, replaces both $BROWSER and the
	// per-OS default (highest precedence).
	OverrideCommand string

	// Test hooks. Zero values mean "use the real implementation";
	// New() populates them.
	goos     string
	getenv   func(key string) string
	lookPath func(file string) (string, error)
	start    func(ctx context.Context, path string, args []string) error
}

// New returns an ExecLauncher wired to the real OS: runtime.GOOS,
// os.Getenv, exec.LookPath and a detached exec.Cmd start.
func New(overrideCommand string) *ExecLauncher {
	return &ExecLauncher{
		OverrideCommand: overrideCommand,
		goos:            runtime.GOOS,
		getenv:          os.Getenv,
		lookPath:        exec.LookPath,
		start:           detachedStart,
	}
}

// Open resolves the launcher command and starts it with url appended
// as the last argument. The error wraps exec.ErrNotFound when no
// launcher is available so the caller can degrade gracefully.
func (l *ExecLauncher) Open(ctx context.Context, url string) error {
	argv, err := l.commandLine()
	if err != nil {
		return err
	}
	// exec.LookPath wraps exec.ErrNotFound in its *exec.Error, so
	// errors.Is(err, exec.ErrNotFound) keeps working through our wrap.
	path, err := l.lookPath(argv[0])
	if err != nil {
		return fmt.Errorf("browser launcher %q: %w", argv[0], err)
	}
	if err := l.start(ctx, path, append(argv[1:], url)); err != nil {
		return fmt.Errorf("launch browser via %q: %w", argv[0], err)
	}
	return nil
}

// commandLine resolves the launcher argv (without the URL) using the
// precedence documented on ExecLauncher.
func (l *ExecLauncher) commandLine() ([]string, error) {
	if s := strings.TrimSpace(l.OverrideCommand); s != "" {
		return strings.Fields(s), nil
	}
	if s := strings.TrimSpace(l.getenv("BROWSER")); s != "" {
		return strings.Fields(s), nil
	}
	switch l.goos {
	case "darwin":
		return []string{"open"}, nil
	case "linux":
		return []string{"xdg-open"}, nil
	case "windows":
		// Best effort: nakama targets linux/darwin, but do not crash
		// elsewhere (see design of `nakama aws open`, ADR 0012).
		return []string{"rundll32", "url.dll,FileProtocolHandler"}, nil
	default:
		return nil, fmt.Errorf(
			"no default browser launcher known for %s (set $BROWSER or use --browser): %w",
			l.goos, exec.ErrNotFound)
	}
}

// detachedStart starts the launcher without waiting for it and without
// tying its lifetime to the CLI process: exec.Command (not
// CommandContext) is used on purpose so that cancelling the CLI's
// context never kills the user's browser. Start (not Run/Output)
// still surfaces immediate launch failures (fork/exec errors), and a
// background Wait reaps the child to avoid a zombie while the CLI is
// still running.
func detachedStart(_ context.Context, path string, args []string) error {
	// #nosec G204 -- path comes from exec.LookPath over an
	// operator-chosen launcher (--browser / $BROWSER) or a hard-coded
	// per-OS default; args are static tokens plus the URL nakama
	// itself built from the CloudFront domain or the local one-shot
	// login server. Nothing here is attacker-controlled input.
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap; exit status is irrelevant
	return nil
}
