# Vendored session-manager-plugin

- Upstream: https://github.com/aws/session-manager-plugin.git
- Tag: `1.2.835.0`
- Commit: `930a08e65d3a378eeeebb7f1bcf67eae7d860ae0`
- Imported by: `scripts/vendor-ssm-plugin.sh`
- License: Apache-2.0 (see LICENSE / NOTICE)

Driven as a library by `internal/service/connect.go`. Resolved as a Go
module via a hand-maintained `go.mod` + `replace` directives in the repo
root `go.mod`. `twinj/uuid` is vendored under `third_party/twinj-uuid`
because its published versions are not API-compatible with this snapshot.

To update: run `scripts/vendor-ssm-plugin.sh [tag]`, then
`go mod tidy && go build ./...`, and adjust go.mod requires if deps changed.
