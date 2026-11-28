//go:build linux

package peeruid

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Supported reports whether Check looks the client's owner up.
const Supported = true

// procOpen opens /proc/net/tcp{,6}; tests substitute their own tables.
var procOpen = func(path string) (io.ReadCloser, error) { return os.Open(path) } // #nosec G304 -- fixed /proc paths

// currentUID is the user the request must come from.
var currentUID = os.Getuid

// Check returns nil when the client end of r's connection is owned by
// the user running this process, an *OtherUserError when it belongs to
// another user, and another error when the owner cannot be found (the
// caller refuses the request then: it fails closed).
func Check(r *http.Request) error {
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return fmt.Errorf("parse the client address %q: %w", r.RemoteAddr, err)
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || la == nil {
		return fmt.Errorf("the request carries no local address")
	}
	server, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return fmt.Errorf("parse the local address %q: %w", la.String(), err)
	}
	uid, err := socketOwner(client, server)
	if err != nil {
		return err
	}
	if uid != currentUID() {
		return &OtherUserError{UID: uid}
	}
	return nil
}

// socketOwner returns the uid of the socket whose local address is
// client and whose remote address is server: the client's end of the
// connection, which lives in the same network namespace.
func socketOwner(client, server netip.AddrPort) (int, error) {
	path := "/proc/net/tcp"
	if !client.Addr().Is4() {
		path = "/proc/net/tcp6"
	}
	f, err := procOpen(path)
	if err != nil {
		return -1, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	local, remote := procAddr(client), procAddr(server)
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first { // the header line
			first = false
			continue
		}
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid ...
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[1] != local || fields[2] != remote {
			continue
		}
		uid, err := strconv.Atoi(fields[7])
		if err != nil {
			return -1, fmt.Errorf("parse the uid in %s: %w", path, err)
		}
		return uid, nil
	}
	if err := sc.Err(); err != nil {
		return -1, fmt.Errorf("read %s: %w", path, err)
	}
	return -1, fmt.Errorf("no socket from %s to %s in %s", client, server, path)
}

// procAddr renders an address as /proc/net/tcp{,6} prints it: each
// 32-bit word of the address read in the machine's byte order (little
// endian on every architecture nakama ships for: amd64, arm64) and
// printed in hex, a colon, and the port in upper-case hex. An IPv4-mapped IPv6 address keeps the IPv6 form, as
// in /proc/net/tcp6.
func procAddr(ap netip.AddrPort) string {
	var b strings.Builder
	a := ap.Addr()
	var raw []byte
	if a.Is4() {
		x := a.As4()
		raw = x[:]
	} else {
		x := a.As16()
		raw = x[:]
	}
	for len(raw) >= 4 {
		fmt.Fprintf(&b, "%08X", binary.LittleEndian.Uint32(raw))
		raw = raw[4:]
	}
	fmt.Fprintf(&b, ":%04X", ap.Port())
	return b.String()
}
