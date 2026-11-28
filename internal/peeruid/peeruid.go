// Package peeruid tells whether the client of a loopback HTTP request
// runs as the same OS user as this process (Issue #139, ADR 0031).
//
// Anything on the machine can connect to a loopback port, including the
// processes of other OS users. The nakama control panel hands the
// browser a one-time sign-in code on the browser's command line, which
// other users can read on Linux (/proc/<pid>/cmdline), so the panel
// refuses requests whose client socket belongs to another user. Linux
// exposes the owner of every TCP socket in /proc/net/tcp{,6}; other
// systems have no such interface, and Check accepts every request there
// (their command lines are not readable by other users in the first
// place on Windows; macOS was not examined).
package peeruid

import "fmt"

// OtherUserError reports a client socket owned by another OS user.
type OtherUserError struct {
	// UID is the owner of the client socket.
	UID int
}

func (e *OtherUserError) Error() string {
	return fmt.Sprintf("the connection comes from another OS user (uid %d)", e.UID)
}
