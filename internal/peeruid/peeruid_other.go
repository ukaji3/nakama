//go:build !linux

package peeruid

import "net/http"

// Supported reports whether Check looks the client's owner up.
const Supported = false

// Check accepts every request: this system offers no way to look up the
// owner of a TCP connection (see the package documentation).
func Check(*http.Request) error { return nil }
