package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/cliproxy"
)

// shellCall records what the launcher started.
type shellCall struct {
	path string
	args []string
	env  []string
	dir  string
}

func newTestLauncher(detect func(context.Context) (*cliproxy.Detection, error)) (*doubleClickLauncher, *bytes.Buffer, *[]shellCall) {
	var out bytes.Buffer
	var calls []shellCall
	return &doubleClickLauncher{
		out:       &out,
		in:        strings.NewReader(""),
		environ:   []string{`Path=C:\Windows`, "USERNAME=you"},
		exeDir:    `C:\Tools`,
		homeDir:   `C:\Users\you`,
		shell:     `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		shellArgs: []string{"-NoLogo"},
		detect:    detect,
		run: func(path string, args, env []string, dir string) (int, error) {
			calls = append(calls, shellCall{path, slices.Clone(args), slices.Clone(env), dir})
			return 0, nil
		},
	}, &out, &calls
}

func TestLauncherOpensConfiguredShell(t *testing.T) {
	det := &cliproxy.Detection{ProxyURL: "http://proxy.example:8080", Intercepted: true,
		RootSubject: "CN=Example Corp CA", BundlePath: `C:\Users\you\.config\nakama\aws-ca-bundle.pem`}
	l, out, calls := newTestLauncher(func(context.Context) (*cliproxy.Detection, error) { return det, nil })
	l.run = func(path string, args, env []string, dir string) (int, error) {
		*calls = append(*calls, shellCall{path, args, env, dir})
		return 3, nil
	}

	if code := l.launch(); code != 3 {
		t.Errorf("exit code = %d, want the shell's 3", code)
	}
	if len(*calls) != 1 {
		t.Fatalf("shell started %d times, want once", len(*calls))
	}
	c := (*calls)[0]
	if c.path != l.shell || !slices.Equal(c.args, []string{"-NoLogo"}) || c.dir != `C:\Users\you` {
		t.Errorf("started %q %q in %q", c.path, c.args, c.dir)
	}
	for key, want := range map[string]string{
		"Path":                 `C:\Tools;C:\Windows`,
		"HTTPS_PROXY":          det.ProxyURL,
		"AWS_CA_BUNDLE":        det.BundlePath,
		"NAKAMA_AWS_CLI_SHELL": "1",
		"USERNAME":             "you",
	} {
		if got, _ := envValue(c.env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, want := range []string{
		`path:   C:\Tools added in front of PATH (this window only)`,
		"proxy:  http://proxy.example:8080",
		"re-signed by CN=Example Corp CA",
		"nakama --help",
		"`exit` to close this window",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLauncherOpensShellWhenDetectionFails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"offline", errors.New("probe https://sts.ap-northeast-1.amazonaws.com/: dial tcp: no such host"), "no such host"},
		{"NTLM proxy", &cliproxy.ProxyAuthError{Proxy: "http://proxy.example:8080", Schemes: []string{"NTLM", "Negotiate"}}, "px or Cntlm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, out, calls := newTestLauncher(func(context.Context) (*cliproxy.Detection, error) { return nil, tt.err })
			l.environ = append(l.environ, "HTTPS_PROXY=http://set-by-user:3128")

			if code := l.launch(); code != 0 {
				t.Errorf("exit code = %d", code)
			}
			if len(*calls) != 1 {
				t.Fatalf("a failed detection must still open the shell; started %d times", len(*calls))
			}
			env := (*calls)[0].env
			if got, _ := envValue(env, "Path"); got != `C:\Tools;C:\Windows` {
				t.Errorf("PATH = %q, nakama's folder must still come first", got)
			}
			// No marker: `nakama aws cli shell` must remain available in
			// the window to retry the detection.
			if _, ok := envValue(env, "NAKAMA_AWS_CLI_SHELL"); ok {
				t.Error("the nesting marker must not be set when nothing was configured")
			}
			if got, _ := envValue(env, "HTTPS_PROXY"); got != "http://set-by-user:3128" {
				t.Errorf("the user's own HTTPS_PROXY must be kept, got %q", got)
			}
			for _, want := range []string{"detection failed", tt.want, "nakama aws cli shell"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestLauncherCtrlCSkipsDetection(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	started := make(chan struct{})
	l, out, calls := newTestLauncher(func(ctx context.Context) (*cliproxy.Detection, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, errors.New("the detection was not cancelled")
		}
	})
	l.interrupts = interrupts
	go func() {
		<-started
		interrupts <- os.Interrupt
	}()

	done := make(chan int, 1)
	go func() { done <- l.launch() }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not end the detection")
	}
	if len(*calls) != 1 {
		t.Fatalf("the shell must open after Ctrl-C; started %d times", len(*calls))
	}
	if !strings.Contains(out.String(), "detection skipped (Ctrl-C)") {
		t.Errorf("output:\n%s", out.String())
	}
}

// enterReader reports that the launcher waited for input.
type enterReader struct{ read bool }

func (r *enterReader) Read(p []byte) (int, error) {
	r.read = true
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = '\n'
	return 1, io.EOF
}

func TestLauncherKeepsShellErrorOnScreen(t *testing.T) {
	l, out, _ := newTestLauncher(func(context.Context) (*cliproxy.Detection, error) { return &cliproxy.Detection{}, nil })
	in := &enterReader{}
	l.in = in
	l.run = func(string, []string, []string, string) (int, error) {
		return -1, errors.New("start shell powershell.exe: file does not exist")
	}

	if code := l.launch(); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !in.read {
		t.Error("the window must wait for Enter before closing, or the error vanishes")
	}
	for _, want := range []string{"file does not exist", "Press Enter"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLauncherWithoutExecutableDir(t *testing.T) {
	l, out, calls := newTestLauncher(func(context.Context) (*cliproxy.Detection, error) { return &cliproxy.Detection{}, nil })
	l.exeDir = ""
	l.launch()
	if got, _ := envValue((*calls)[0].env, "Path"); got != `C:\Windows` {
		t.Errorf("PATH = %q, want it unchanged", got)
	}
	if !strings.Contains(out.String(), "PATH is unchanged") {
		t.Errorf("output:\n%s", out.String())
	}
}
