package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ukaji3/nakama/internal/version"
)

// runVersion runs `nakama version args...` as the build reporting
// running, against a release API answering with status and body.
func runVersion(t *testing.T, running string, status int, body string, args ...string) (stdout, stderr string, calls int64, err error) {
	t.Helper()
	old := version.Version
	version.Version = running
	t.Cleanup(func() { version.Version = old })

	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	cmd := newVersionCommandWith(version.Checker{URL: srv.URL})
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err = cmd.Execute()
	srv.Close() // the handler has run for every request once Close returns
	return out.String(), errOut.String(), n.Load(), err
}

func TestVersionWithoutCheckStaysOffline(t *testing.T) {
	stdout, stderr, calls, err := runVersion(t, "0.24.0", http.StatusOK, `{"tag_name":"v0.25.0"}`)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if want := version.String() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want the unchanged banner %q", stdout, want)
	}
	if stderr != "" || calls != 0 {
		t.Errorf("stderr = %q, requests = %d; want neither", stderr, calls)
	}
}

func TestVersionCheck(t *testing.T) {
	const page = "https://github.com/ukaji3/nakama/releases/tag/"
	tests := []struct {
		name, running, tag string
		wantStatus         string
	}{
		{"latest", "0.24.0", "v0.24.0", "status  : up to date\n"},
		{"outdated", "0.9.0", "v0.10.0", "status  : out of date; download v0.10.0 from the release page\n"},
		{"newer than latest", "0.25.0-rc.1", "v0.24.0", "status  : newer than the latest release\n"},
		{"development build", "dev", "v0.24.0", "status  : development build; not compared with the latest release\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, calls, err := runVersion(t, tt.running, http.StatusOK,
				fmt.Sprintf(`{"tag_name":%q}`, tt.tag), "--check")
			if err != nil {
				t.Fatalf("version --check: %v (the exit code must stay 0)", err)
			}
			want := "current : " + tt.running + "\n" +
				"latest  : " + tt.tag + "\n" +
				"release : " + page + tt.tag + "\n" +
				tt.wantStatus
			if stdout != want {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
			}
			if stderr != "" || calls != 1 {
				t.Errorf("stderr = %q, requests = %d; want none and 1", stderr, calls)
			}
		})
	}
}

func TestVersionCheckFailureOnlyWarns(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"HTTP error", http.StatusBadGateway, "", "status 502"},
		{"broken JSON", http.StatusOK, `{"tag_name":`, "could not read GitHub's answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, _, err := runVersion(t, "0.24.0", tt.status, tt.body, "--check")
			if err != nil {
				t.Fatalf("version --check: %v (a failed check must not change the exit code)", err)
			}
			if stdout != "current : 0.24.0\n" {
				t.Errorf("stdout = %q, want the current version only", stdout)
			}
			if !strings.HasPrefix(stderr, "warning: could not check for a newer version: ") || !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want a warning containing %q", stderr, tt.wantErr)
			}
		})
	}
}

func TestVersionCheckTimeoutOnlyWarns(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // never answers in time
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })

	var out, errOut bytes.Buffer
	cmd := newVersionCommandWith(version.Checker{URL: srv.URL, Client: &http.Client{Timeout: 100 * time.Millisecond}})
	cmd.SetArgs([]string{"--check"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version --check: %v (a timeout must not change the exit code)", err)
	}
	if !strings.HasPrefix(out.String(), "current : ") {
		t.Errorf("stdout = %q, want the current version", out.String())
	}
	if !strings.Contains(errOut.String(), "warning: could not check for a newer version") {
		t.Errorf("stderr = %q, want the warning", errOut.String())
	}
}
