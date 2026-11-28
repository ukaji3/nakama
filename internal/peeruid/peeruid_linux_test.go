//go:build linux

package peeruid

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// Not parallel: the tests replace procOpen and currentUID.

func TestProcAddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The forms /proc/net/tcp{,6} print on little-endian machines.
		{"127.0.0.1:38369", "0100007F:95E1"},
		{"10.20.30.40:80", "281E140A:0050"},
		{"[::1]:18888", "00000000000000000000000001000000:49C8"},
		{"[::ffff:127.0.0.1]:80", "0000000000000000FFFF00000100007F:0050"},
	} {
		if got := procAddr(netip.MustParseAddrPort(tc.in)); got != tc.want {
			t.Errorf("procAddr(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// checkServer answers each request with Check's verdict.
func checkServer(t *testing.T, network, addr string) string {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Check(r); err != nil {
			var other *OtherUserError
			if errors.As(err, &other) {
				_, _ = io.WriteString(w, "other "+err.Error())
				return
			}
			_, _ = io.WriteString(w, "error "+err.Error())
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestCheckAcceptsTheCurrentUser(t *testing.T) {
	if got := checkServer(t, "tcp4", "127.0.0.1:0"); got != "ok" {
		t.Errorf("IPv4: %s", got)
	}
	if got := checkServer(t, "tcp6", "[::1]:0"); got != "ok" {
		t.Errorf("IPv6: %s", got)
	}
}

func TestCheckRefusesAnotherUser(t *testing.T) {
	prev := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = prev })
	got := checkServer(t, "tcp4", "127.0.0.1:0")
	if !strings.HasPrefix(got, "other ") || !strings.Contains(got, "another OS user") {
		t.Errorf("a socket of uid %d must be refused for uid %d: %s", os.Getuid(), os.Getuid()+1, got)
	}
}

func TestCheckFailsClosed(t *testing.T) {
	prev := procOpen
	t.Cleanup(func() { procOpen = prev })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:40000"
	r = r.WithContext(contextWithLocalAddr(r, "127.0.0.1:18888"))

	procOpen = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid\n" +
			"   0: 0100007F:49C8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000\n")), nil
	}
	if err := Check(r); err == nil {
		t.Error("a connection that is not in /proc/net/tcp must be refused")
	}
	procOpen = func(string) (io.ReadCloser, error) { return nil, os.ErrPermission }
	if err := Check(r); err == nil {
		t.Error("an unreadable /proc/net/tcp must refuse the request")
	}
	// The entry of the client's end decides, not the server's end.
	procOpen = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("header\n" +
			"   0: 0100007F:49C8 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  4242\n" +
			"   1: 0100007F:9C40 0100007F:49C8 01 00000000:00000000 00:00000000 00000000  4343\n")), nil
	}
	var other *OtherUserError
	if err := Check(r); !errors.As(err, &other) || other.UID != 4343 {
		t.Errorf("want the uid of the client's end (4343), got %v", err)
	}
	r.RemoteAddr = "not-an-address"
	if err := Check(r); err == nil {
		t.Error("an unparsable client address must be refused")
	}
}

func contextWithLocalAddr(r *http.Request, addr string) context.Context {
	return context.WithValue(r.Context(), http.LocalAddrContextKey, net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr)))
}
