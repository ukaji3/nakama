# Vendored session-manager-plugin

- Upstream: https://github.com/aws/session-manager-plugin.git
- Tag: `1.2.814.0`
- Commit: `dcff8da8cdecd53789bd06a193bd2032665dccd8`
- Imported by: `scripts/vendor-ssm-plugin.sh`
- License: Apache-2.0 (see LICENSE / NOTICE)

Driven as a library by `internal/service/connect.go`. Resolved as a Go
module via a hand-maintained `go.mod` + `replace` directives in the repo
root `go.mod`. `twinj/uuid` is vendored under `third_party/twinj-uuid`
because its published versions are not API-compatible with this snapshot.

To update: run `scripts/vendor-ssm-plugin.sh [tag]`, then
`go mod tidy && go build ./...`, and adjust go.mod requires if deps changed.
