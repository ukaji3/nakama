package version

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v0.9.0", "v0.10.0", -1}, // numeric, not string, order
		{"v0.10.0", "v0.9.0", 1},
		{"v0.24.0", "0.24.0", 0}, // tag vs GoReleaser's {{.Version}}
		{"v0.24.0", "v0.24.1", -1},
		{"v0.99.99", "v1.0.0", -1},
		{"v2.0.0", "v10.0.0", -1},
		{"v0.25.0-rc.1", "v0.25.0", -1}, // a prerelease is below its release
		{"v0.25.0-rc.1", "v0.24.0", 1},  // and above the previous one
		{"v0.25.0-rc.2", "v0.25.0-rc.10", -1},
		{"v0.25.0-rc.1", "v0.25.0-rc.1", 0},
		{"1.0.0+build.5", "1.0.0", 0}, // build metadata takes no part
		{"0.24.1-snapshot+9010bd5", "v0.24.0", 1},
		// semver.org 2.0.0 §11's example chain.
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
	}
	for _, tt := range tests {
		a, okA := parseSemver(tt.a)
		b, okB := parseSemver(tt.b)
		if !okA || !okB {
			t.Fatalf("parseSemver(%q) ok=%v, parseSemver(%q) ok=%v; want both valid", tt.a, okA, tt.b, okB)
		}
		if got := compareSemver(a, b); got != tt.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := compareSemver(b, a); got != -tt.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

func TestParseRelease(t *testing.T) {
	for _, s := range []string{"0.24.0", "v0.24.0", "v0.25.0-rc.1", "1.0.0-x-y.7", "0.24.1-snapshot+9010bd5"} {
		if _, ok := parseRelease(s); !ok {
			t.Errorf("parseRelease(%q) = not a release, want a release", s)
		}
	}
	for _, s := range []string{
		"dev", "", "none", "9010bd5", // the defaults and `git describe --always` without tags
		"v0.24", "v0.24.0.1", "v01.2.3", "v1.2.x", "V1.2.3",
		"v1.2.3-", "v1.2.3-rc..1", "v1.2.3-rc.01", "v1.2.3-rc_1",
		"v0.24.0-3-g9010bd5", "v0.24.0-3-g9010bd5-dirty", "v0.24.0-dirty", // make build between tags
		"v99999999999999999999.0.0",
	} {
		if _, ok := parseRelease(s); ok {
			t.Errorf("parseRelease(%q) = a release, want a development build", s)
		}
	}
}

// releaseServer answers every request with status, header and body.
func releaseServer(t *testing.T, status int, body string, header http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck(t *testing.T) {
	const release = `{"tag_name":"%s","html_url":"https://example.com/elsewhere","name":"nakama %[1]s","body":"notes"}`
	tests := []struct {
		name, current, tag string
		want               Status
	}{
		{"latest", "0.24.0", "v0.24.0", StatusUpToDate},
		{"latest with v", "v0.24.0", "v0.24.0", StatusUpToDate},
		{"outdated", "0.9.0", "v0.10.0", StatusOutdated},
		{"outdated prerelease", "0.25.0-rc.1", "v0.25.0", StatusOutdated},
		{"newer than latest", "0.25.0", "v0.24.0", StatusAhead},
		{"newer prerelease", "0.25.0-rc.1", "v0.24.0", StatusAhead},
		{"dev", "dev", "v0.24.0", StatusDevelopment},
		{"git describe", "v0.24.0-3-g9010bd5-dirty", "v0.24.0", StatusDevelopment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := releaseServer(t, http.StatusOK, fmt.Sprintf(release, tt.tag), nil)
			res := Checker{URL: srv.URL}.Check(context.Background(), tt.current)
			if res.Status != tt.want || res.Err != nil {
				t.Fatalf("Check(%q) against %s = %+v, want %s", tt.current, tt.tag, res, tt.want)
			}
			if res.Current != tt.current || res.Latest != tt.tag {
				t.Errorf("Current, Latest = %q, %q; want %q, %q", res.Current, res.Latest, tt.current, tt.tag)
			}
			// The page is built from the validated tag, never taken from
			// the answer's html_url.
			if want := "https://github.com/ukaji3/nakama/releases/tag/" + tt.tag; res.URL != want {
				t.Errorf("URL = %q, want %q", res.URL, want)
			}
		})
	}
}

func TestCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		header  http.Header
		wantErr string
	}{
		{"server error", http.StatusInternalServerError, "oops", nil, "status 500"},
		{"no release yet", http.StatusNotFound, `{"message":"Not Found"}`, nil, "status 404"},
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`,
			http.Header{"X-Ratelimit-Remaining": {"0"}}, "rate limit"},
		{"rate limited 429", http.StatusTooManyRequests, "", http.Header{"X-Ratelimit-Remaining": {"0"}}, "rate limit"},
		{"broken JSON", http.StatusOK, `{"tag_name": "v0.2`, nil, "could not read GitHub's answer"},
		{"not JSON", http.StatusOK, `<html>proxy login</html>`, nil, "could not read GitHub's answer"},
		{"empty body", http.StatusOK, "", nil, "could not read GitHub's answer"},
		{"no tag", http.StatusOK, `{"name":"nakama"}`, nil, `tag "" is not a version`},
		{"tag not a version", http.StatusOK, `{"tag_name":"nightly"}`, nil, `tag "nightly" is not a version`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := releaseServer(t, tt.status, tt.body, tt.header)
			res := Checker{URL: srv.URL}.Check(context.Background(), "0.24.0")
			if res.Status != StatusUnknown || res.Err == nil || !strings.Contains(res.Err.Error(), tt.wantErr) {
				t.Fatalf("Check = %+v, want unknown with an error containing %q", res, tt.wantErr)
			}
			if res.Latest != "" || res.URL != "" || res.Current != "0.24.0" {
				t.Errorf("Check = %+v, want only Current set", res)
			}
		})
	}
}

func TestCheckTimeout(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // never answers in time
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })

	start := time.Now()
	res := Checker{URL: srv.URL, Client: &http.Client{Timeout: 100 * time.Millisecond}}.Check(context.Background(), "0.24.0")
	if res.Status != StatusUnknown || res.Err == nil {
		t.Fatalf("Check = %+v, want unknown", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Check took %v; the client timeout must bound it", elapsed)
	}
	if CheckTimeout < time.Second || CheckTimeout > 10*time.Second {
		t.Errorf("CheckTimeout = %v, want a few seconds", CheckTimeout)
	}
}

func TestCheckRequest(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		_, _ = fmt.Fprint(w, `{"tag_name":"v0.24.0"}`)
	}))
	t.Cleanup(srv.Close)

	Checker{URL: srv.URL + "/repos/ukaji3/nakama/releases/latest"}.Check(context.Background(), "0.24.0")
	r := <-got
	if r.Method != http.MethodGet || r.URL.Path != "/repos/ukaji3/nakama/releases/latest" {
		t.Errorf("request = %s %s", r.Method, r.URL.Path)
	}
	if a := r.Header.Get("Accept"); a != "application/vnd.github+json" {
		t.Errorf("Accept = %q", a)
	}
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "nakama/") {
		t.Errorf("User-Agent = %q, want nakama/<version>", ua)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none (no token is sent)", auth)
	}
}

func TestCheckDefaultClientUsesProxySettings(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	got := make(chan string, 1)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.String() // a proxy receives the absolute URL
		_, _ = fmt.Fprint(w, `{"tag_name":"v0.24.0"}`)
	}))
	t.Cleanup(proxySrv.Close)
	t.Setenv("HTTP_PROXY", proxySrv.URL)

	// The .invalid host never resolves: the answer can only come from
	// the proxy that the zero-value client picked up.
	const target = "http://releases.nakama.invalid/latest"
	res := Checker{URL: target}.Check(context.Background(), "0.24.0")
	if res.Status != StatusUpToDate {
		t.Fatalf("Check = %+v, want up to date through the proxy", res)
	}
	if u := <-got; u != target {
		t.Errorf("proxy received %q, want %q", u, target)
	}
}
