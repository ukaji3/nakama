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
	"sync"

	"github.com/mattn/go-ieproxy"
	"golang.org/x/net/http/httpproxy"
)

// osProxy holds the OS-level proxy resolution shared by every Func.
// go-ieproxy reads the OS settings once per process (a sync.Once on
// Windows and macOS) and each GetProxyFunc captures them, so a
// long-running process (the tunnel daemon) keeps the settings of the
// network it started on. Reload replaces it.
var osProxy struct {
	mu sync.RWMutex
	fn func(*http.Request) (*url.URL, error)
}

// currentOSProxy returns the OS-level resolution, reading the settings
// on first use.
func currentOSProxy() func(*http.Request) (*url.URL, error) {
	osProxy.mu.RLock()
	fn := osProxy.fn
	osProxy.mu.RUnlock()
	if fn != nil {
		return fn
	}
	osProxy.mu.Lock()
	defer osProxy.mu.Unlock()
	if osProxy.fn == nil {
		osProxy.fn = ieproxy.GetProxyFunc()
	}
	return osProxy.fn
}

// Reload re-reads the OS proxy settings (Windows WinHTTP/IE including
// PAC; a no-op elsewhere, where only the environment counts). Every
// function returned by Func, including those inside already-built HTTP
// transports, resolves through the new settings from then on. The
// tunnel daemon calls it before it reconnects, so a laptop that moved
// between a proxied office network and a direct one does not keep
// dialling the old proxy (Issue #118).
func Reload() {
	ieproxy.ReloadConf()
	fn := ieproxy.GetProxyFunc()
	osProxy.mu.Lock()
	osProxy.fn = fn
	osProxy.mu.Unlock()
}

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
	return func(req *http.Request) (*url.URL, error) {
		cfg := httpproxy.FromEnvironment()
		if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
			return cfg.ProxyFunc()(req.URL)
		}
		return currentOSProxy()(req)
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
