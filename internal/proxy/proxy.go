// Package proxy resolves the outbound HTTP(S) proxy for nakama,
// combining the conventional environment variables with OS-level proxy
// settings (Windows WinHTTP/IE, including PAC scripts) via
// github.com/mattn/go-ieproxy.
//
// Every outbound HTTP path in nakama (AWS SDK clients, the Session
// Manager websocket dialer, the checkip discoverer) routes through
// Func so that corporate-proxy environments work without manual
// HTTP(S)_PROXY exports on Windows.
package proxy

import (
	"net/http"
	"net/url"

	"github.com/mattn/go-ieproxy"
	"golang.org/x/net/http/httpproxy"
)

// Func returns a proxy resolution function suitable for
// http.Transport.Proxy.
//
// Resolution is deliberately two-tiered:
//
//  1. Environment variables win. httpproxy.FromEnvironment (the same
//     parser net/http uses underneath) is re-evaluated on EVERY call of
//     the returned function; when HTTP_PROXY or HTTPS_PROXY (either
//     case) is set, its ProxyFunc decides, honoring NO_PROXY. This is
//     intentionally NOT http.ProxyFromEnvironment, which caches the
//     environment in a sync.Once on first use and therefore cannot be
//     exercised deterministically in tests using t.Setenv.
//  2. With no proxy environment variables set, resolution is delegated
//     to ieproxy.GetProxyFunc(), which reads OS-level settings on
//     Windows (WinHTTP / IE static configuration and PAC scripts,
//     no cgo required). On Linux, and on macOS binaries built with
//     CGO_ENABLED=0 (as nakama release binaries are — the macOS
//     system-settings reader in go-ieproxy needs cgo), ieproxy falls
//     back to the environment, i.e. resolves to no proxy here.
func Func() func(*http.Request) (*url.URL, error) {
	osProxy := ieproxy.GetProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		cfg := httpproxy.FromEnvironment()
		if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
			return cfg.ProxyFunc()(req.URL)
		}
		return osProxy(req)
	}
}

// NewTransport returns a clone of http.DefaultTransport with its Proxy
// function replaced by Func. Use it for plain http.Client construction;
// AWS SDK clients get the same Proxy via their BuildableClient
// transport options instead (see internal/awsx).
func NewTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = Func()
	return tr
}
