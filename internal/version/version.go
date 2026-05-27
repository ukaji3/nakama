// Package version exposes build-time version metadata.
//
// Values are injected via -ldflags at build time (see Makefile).
package version

// Version is the semantic version (e.g. v0.1.0) or `dev`.
var Version = "dev"

// Commit is the short git commit hash.
var Commit = "none"

// BuildDate is the UTC build timestamp (RFC3339).
var BuildDate = "unknown"

// String returns a single-line, human-readable version banner.
func String() string {
	return Version + " (" + Commit + ", " + BuildDate + ")"
}
