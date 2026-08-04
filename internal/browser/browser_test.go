package browser

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// newTestLauncher returns an ExecLauncher whose OS hooks are fully
// faked: nothing is ever looked up in the real PATH and no process is
// spawned. started captures the argv passed to the start hook.
func newTestLauncher(goos string, env map[string]string, started *[]string) *ExecLauncher {
	return &ExecLauncher{
		goos:   goos,
		getenv: func(key string) string { return env[key] },
		lookPath: func(file string) (string, error) {
			return "/fake/bin/" + file, nil
		},
		start: func(_ context.Context, path string, args []string) error {
			*started = append([]string{path}, args...)
			return nil
		},
	}
}

func TestExecLauncher_CommandSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		goos     string
		env      map[string]string
		override string
		wantArgv []string // path + args, URL appended last
		wantErr  bool
	}{
		{
			name:     "linux default is xdg-open",
			goos:     "linux",
			wantArgv: []string{"/fake/bin/xdg-open", "https://example.test"},
		},
		{
			name:     "darwin default is open",
			goos:     "darwin",
			wantArgv: []string{"/fake/bin/open", "https://example.test"},
		},
		{
			name:     "windows best-effort rundll32",
			goos:     "windows",
			wantArgv: []string{"/fake/bin/rundll32", "url.dll,FileProtocolHandler", "https://example.test"},
		},
		{
			name:    "unknown OS yields not-found error",
			goos:    "plan9",
			wantErr: true,
		},
		{
			name:     "$BROWSER beats the per-OS default",
			goos:     "linux",
			env:      map[string]string{"BROWSER": "firefox --new-window"},
			wantArgv: []string{"/fake/bin/firefox", "--new-window", "https://example.test"},
		},
		{
			name:     "override beats $BROWSER",
			goos:     "linux",
			env:      map[string]string{"BROWSER": "firefox"},
			override: "chromium --incognito",
			wantArgv: []string{"/fake/bin/chromium", "--incognito", "https://example.test"},
		},
		{
			name:     "whitespace-only $BROWSER is ignored",
			goos:     "linux",
			env:      map[string]string{"BROWSER": "   "},
			wantArgv: []string{"/fake/bin/xdg-open", "https://example.test"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var started []string
			l := newTestLauncher(tc.goos, tc.env, &started)
			l.OverrideCommand = tc.override
			err := l.Open(context.Background(), "https://example.test")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, exec.ErrNotFound) {
					t.Errorf("error must wrap exec.ErrNotFound, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if len(started) != len(tc.wantArgv) {
				t.Fatalf("argv=%v, want %v", started, tc.wantArgv)
			}
			for i := range started {
				if started[i] != tc.wantArgv[i] {
					t.Errorf("argv[%d]=%q, want %q (full: %v)", i, started[i], tc.wantArgv[i], started)
				}
			}
		})
	}
}

func TestExecLauncher_LookPathFailureIsDistinguishable(t *testing.T) {
	t.Parallel()
	var started []string
	l := newTestLauncher("linux", nil, &started)
	l.lookPath = func(file string) (string, error) {
		// Mirror the real exec.LookPath failure shape.
		return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
	}
	err := l.Open(context.Background(), "https://example.test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error must wrap exec.ErrNotFound so callers can fall back, got: %v", err)
	}
	if len(started) != 0 {
		t.Errorf("nothing must be started when the launcher is missing, got %v", started)
	}
}

func TestExecLauncher_StartFailureIsReported(t *testing.T) {
	t.Parallel()
	var started []string
	l := newTestLauncher("linux", nil, &started)
	boom := errors.New("fork/exec failed")
	l.start = func(context.Context, string, []string) error { return boom }
	err := l.Open(context.Background(), "https://example.test")
	if !errors.Is(err, boom) {
		t.Errorf("start failure must propagate, got: %v", err)
	}
}

func TestNew_PopulatesRealHooks(t *testing.T) {
	t.Parallel()
	l := New("my-browser")
	if l.OverrideCommand != "my-browser" {
		t.Errorf("OverrideCommand=%q", l.OverrideCommand)
	}
	if l.goos == "" || l.getenv == nil || l.lookPath == nil || l.start == nil {
		t.Error("New must wire all hooks")
	}
}
