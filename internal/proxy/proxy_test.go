package proxy

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// clearProxyEnv blanks every proxy-related environment variable (both
// cases) for the duration of the test. httpproxy.FromEnvironment
// treats empty values as unset. t.Setenv also disallows t.Parallel,
// which keeps these tests from racing each other on the process
// environment.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy",
	} {
		t.Setenv(k, "")
	}
}

// resolve runs Func against a synthetic request for rawURL. No network
// traffic is involved: proxy resolution only inspects the URL and the
// environment.
func resolve(t *testing.T, rawURL string) (string, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	u, err := Func()(req)
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", nil
	}
	return u.String(), nil
}

func TestFunc_HTTPSProxyFromEnv(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")

	got, err := resolve(t, "https://ssm.ap-northeast-1.amazonaws.com/")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if want := "http://proxy.internal:3128"; got != want {
		t.Errorf("proxy = %q, want %q", got, want)
	}
}

func TestFunc_NoProxyExclusion(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	t.Setenv("NO_PROXY", "amazonaws.com")

	got, err := resolve(t, "https://ssm.ap-northeast-1.amazonaws.com/")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if got != "" {
		t.Errorf("proxy = %q, want direct (NO_PROXY exclusion)", got)
	}

	// A host outside NO_PROXY still goes through the proxy.
	got, err = resolve(t, "https://example.com/")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if want := "http://proxy.internal:3128"; got != want {
		t.Errorf("proxy = %q, want %q", got, want)
	}
}

func TestFunc_ReevaluatesEnvironmentPerCall(t *testing.T) {
	clearProxyEnv(t)
	f := Func()
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)

	// Setting the environment AFTER Func() was built must still take
	// effect: the env tier is re-evaluated on every call (this is the
	// reason Func avoids http.ProxyFromEnvironment's sync.Once cache).
	t.Setenv("HTTPS_PROXY", "http://late.internal:8080")
	u, err := f(req)
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if u == nil || u.String() != "http://late.internal:8080" {
		t.Errorf("proxy = %v, want http://late.internal:8080", u)
	}
}

func TestFunc_NoEnvMeansDirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("with no env vars set, resolution falls through to the OS " +
			"(WinHTTP/IE) settings of the machine running the tests")
	}
	clearProxyEnv(t)

	got, err := resolve(t, "https://example.com/")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if got != "" {
		t.Errorf("proxy = %q, want direct (no proxy configured)", got)
	}
}

func TestNewTransport(t *testing.T) {
	tr := NewTransport()
	if tr.Proxy == nil {
		t.Fatal("NewTransport().Proxy is nil")
	}
	if def, ok := http.DefaultTransport.(*http.Transport); ok && tr == def {
		t.Fatal("NewTransport returned http.DefaultTransport itself instead of a clone")
	}
}
