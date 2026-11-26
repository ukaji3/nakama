package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// Tests of the result `code` and `kirocrew` print (Issue #137): both
// commands print the same "key : value" lines, in every path, and
// -o json is unchanged.

// fetchingBuffer is stdout for a --print-url run: it fetches the
// login_url once it appears, as the operator would paste it into a
// browser, so the one-shot page is delivered.
type fetchingBuffer struct {
	t       *testing.T
	mu      sync.Mutex
	buf     bytes.Buffer
	fetched bool
	done    chan struct{}
}

var loginURLLine = regexp.MustCompile(`login_url  : (http://127\.0\.0\.1:\d+/\S+)  \(`)

func newFetchingBuffer(t *testing.T) *fetchingBuffer {
	return &fetchingBuffer{t: t, done: make(chan struct{})}
}

func (b *fetchingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if m := loginURLLine.FindStringSubmatch(b.buf.String()); m != nil && !b.fetched {
		b.fetched = true
		go func() {
			defer close(b.done)
			if _, _, err := httpGet(b.t, m[1]); err != nil {
				b.t.Errorf("fetch %s: %v", m[1], err)
			}
		}()
	}
	return n, err
}

func (b *fetchingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runOpenCommand runs cmd with args and returns stdout and stderr.
// stdout is a fetchingBuffer when fetch is set.
func runOpenCommand(t *testing.T, cmd *cobra.Command, fetch bool, args ...string) (string, string, error) {
	t.Helper()
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if !fetch {
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}
	out := newFetchingBuffer(t)
	cmd.SetOut(out)
	err := cmd.Execute()
	if out.fetched {
		<-out.done
	} else {
		t.Error("the run printed no login_url to fetch")
	}
	return out.String(), errOut.String(), err
}

// fetchingLauncher is a browser that loads whatever it is told to open.
func fetchingLauncher(t *testing.T) *fakeLauncher {
	l := &fakeLauncher{}
	l.onOpen = func(u string) {
		if _, _, err := httpGet(t, u); err != nil {
			t.Errorf("fetch %s: %v", u, err)
		}
	}
	return l
}

// The URL of the local one-shot page changes every run.
var oneShotURL = regexp.MustCompile(`http://127\.0\.0\.1:\d+/\S+`)

func TestCodeAndKirocrewPrintTheSameResult(t *testing.T) {
	// Not parallel: other tests switch global.Output.
	codeCmd := func(l *fakeLauncher) *cobra.Command {
		return newAWSCodeCommandWithDeps(newTestOpenDeps(runningBackend(), l))
	}
	kirocrewCmd := func(l *fakeLauncher) *cobra.Command {
		return newAWSKirocrewCommandWithDeps(kirocrewDeps{
			connect: func(context.Context) (kirocrewBackend, error) { return enabledKirocrewBackend(), nil },
			open:    newTestOpenDeps(runningBackend(), l),
		})
	}
	const (
		codeSSM  = "http://code-server.dev01.nakama.localhost:18888"
		codeCF   = "https://d111abcdef.cloudfront.net"
		crewSSM  = "http://kirocrew.dev01.nakama.localhost:18888"
		notFetch = "auto-login skipped: the browser did not load the one-shot login page in time (waited 100ms)"
	)
	for _, tc := range []struct {
		desc     string
		cmd      func(*fakeLauncher) *cobra.Command
		launcher func(*testing.T) *fakeLauncher
		fetch    bool
		args     []string
		want     string
	}{
		{
			desc: "code over SSM, signed in", cmd: codeCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--timeout", "5s"},
			want: "url        : " + codeSSM + "\nvia        : ssm\nmethod     : auto-login\nauto_login : completed\n",
		},
		{
			desc: "kirocrew over SSM, signed in", cmd: kirocrewCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--timeout", "5s"},
			want: "url        : " + crewSSM + "\nvia        : ssm\nmethod     : auto-login\nauto_login : completed\n",
		},
		{
			desc: "code --no-auth", cmd: codeCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--no-auth"},
			want: "url        : " + codeSSM + "\nvia        : ssm\nmethod     : plain\nauto_login : skipped\n",
		},
		{
			desc: "kirocrew --no-auth", cmd: kirocrewCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--no-auth"},
			want: "url        : " + crewSSM + "\nvia        : ssm\nmethod     : plain\nauto_login : skipped\n",
		},
		{
			desc: "code --no-auth --print-url: the URL first, on its own", cmd: codeCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--no-auth", "--print-url"},
			want: codeSSM + "\nvia        : ssm\nmethod     : plain\nauto_login : skipped\n",
		},
		{
			desc: "kirocrew --no-auth --print-url: the URL first, on its own", cmd: kirocrewCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--no-auth", "--print-url"},
			want: crewSSM + "/\nvia        : ssm\nmethod     : plain\nauto_login : skipped\n",
		},
		{
			desc: "code --print-url, page fetched: url once, in the width of the result", cmd: codeCmd,
			launcher: func(*testing.T) *fakeLauncher { return &fakeLauncher{} }, fetch: true,
			args: []string{"dev01", "--print-url", "--timeout", "5s"},
			want: "url        : " + codeSSM + "\nlogin_url  : <one-shot>  (open on this machine within 5s to auto-login)\n" +
				"via        : ssm\nmethod     : auto-login\nauto_login : completed\n",
		},
		{
			desc: "kirocrew --print-url, page fetched: url once, in the width of the result", cmd: kirocrewCmd,
			launcher: func(*testing.T) *fakeLauncher { return &fakeLauncher{} }, fetch: true,
			args: []string{"dev01", "--print-url", "--timeout", "5s"},
			want: "url        : " + crewSSM + "\nlogin_url  : <one-shot>  (open on this machine within 5s; the token inside expires in 5 minutes)\n" +
				"via        : ssm\nmethod     : auto-login\nauto_login : completed\n",
		},
		{
			desc: "code, page not fetched in time: the plain URL is opened", cmd: codeCmd,
			launcher: func(*testing.T) *fakeLauncher { return &fakeLauncher{} },
			args:     []string{"dev01", "--timeout", "100ms"},
			want:     "url        : " + codeSSM + "\nvia        : ssm\nmethod     : plain\nauto_login : timeout\nnote       : " + notFetch + "\n",
		},
		{
			desc: "code --internet, signed in", cmd: codeCmd, launcher: fetchingLauncher,
			args: []string{"dev01", "--internet", "--timeout", "5s"},
			want: "url        : " + codeCF + "\nvia        : internet\nmethod     : auto-login\nauto_login : completed\n",
		},
		{
			desc: "code --internet --no-auth, no browser: the URL first, on its own", cmd: codeCmd,
			launcher: func(*testing.T) *fakeLauncher { return &fakeLauncher{err: errors.New("no launcher")} },
			args:     []string{"dev01", "--internet", "--no-auth"},
			want:     codeCF + "\nvia        : internet\nmethod     : plain\nauto_login : skipped\nnote       : browser launch failed: no launcher\n",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			out, errOut, err := runOpenCommand(t, tc.cmd(tc.launcher(t)), tc.fetch, tc.args...)
			if err != nil {
				t.Fatalf("%v\nstderr:\n%s", err, errOut)
			}
			if got := oneShotURL.ReplaceAllString(out, "<one-shot>"); got != tc.want {
				t.Errorf("stdout\n%q\nwant\n%q", got, tc.want)
			}
			if strings.Contains(out, "s3cret-pw") || strings.Contains(out, testKCToken) {
				t.Error("the password or the token is on stdout")
			}
		})
	}
}

// TestCodeSaysWhatItSignsInTo: the progress line names the site, as
// kirocrew's does.
func TestCodeSaysWhatItSignsInTo(t *testing.T) {
	_, errOut, err := runCodeCommand(t, newTestOpenDeps(runningBackend(), fetchingLauncher(t)), "dev01", "--timeout", "5s")
	if err != nil {
		t.Fatal(err)
	}
	if want := "auto-login page delivered to the browser; signing in to code-server at http://code-server.dev01.nakama.localhost:18888\n"; !strings.Contains(errOut, want) {
		t.Errorf("stderr lacks %q:\n%s", want, errOut)
	}
}

// TestCodeNoAuthPrintURLJSONIsUnchanged: with -o json the URL stays in
// the result only, so stdout is one JSON document, as before.
func TestCodeNoAuthPrintURLJSONIsUnchanged(t *testing.T) {
	prev := global.Output
	global.Output = "json"
	t.Cleanup(func() { global.Output = prev })
	out, _, err := runCodeCommand(t, newTestOpenDeps(runningBackend(), &fakeLauncher{}), "dev01", "--no-auth", "--print-url")
	if err != nil {
		t.Fatal(err)
	}
	var res openResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if res.URL != "http://code-server.dev01.nakama.localhost:18888" || res.Method != "plain" || res.Opened || res.AutoLogin != "skipped" {
		t.Errorf("result = %+v", res)
	}
}
