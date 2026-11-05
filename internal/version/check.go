package version

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/proxy"
)

// CheckTimeout bounds the whole latest-release request, so that a
// network that silently drops api.github.com costs a few seconds, not
// a hang.
const CheckTimeout = 5 * time.Second

// maxReleaseBody caps the release JSON that is read. The answer carries
// the release notes and the asset list, which stay far below this.
const maxReleaseBody = 1 << 20

// Status is how the running build compares with the latest release.
type Status string

const (
	// StatusUpToDate means the running version is the latest release.
	StatusUpToDate Status = "up-to-date"
	// StatusOutdated means a newer release exists.
	StatusOutdated Status = "outdated"
	// StatusAhead means the running version is newer than the latest
	// release (for example a prerelease of the next version).
	StatusAhead Status = "ahead"
	// StatusDevelopment means the running build is not a release
	// version (dev, a commit hash, a build between tags), so it is not
	// compared.
	StatusDevelopment Status = "development"
	// StatusUnknown means the latest release could not be determined;
	// CheckResult.Err says why.
	StatusUnknown Status = "unknown"
)

// CheckResult is the outcome of Checker.Check.
type CheckResult struct {
	// Current is the running version, as given to Check.
	Current string
	// Latest is the latest release's tag (e.g. v0.25.0). Empty with
	// StatusUnknown.
	Latest string
	// URL is the latest release's page. Empty with StatusUnknown.
	URL string
	// Status is the comparison.
	Status Status
	// Err is why the check failed (StatusUnknown only).
	Err error
}

// Checker asks GitHub for the latest nakama release. The zero value is
// the production checker.
type Checker struct {
	// URL is the latest-release endpoint (config.LatestReleaseURL when
	// empty).
	URL string
	// Client sends the request. When nil, Check uses a client with
	// CheckTimeout and proxy.NewTransport(), the proxy resolution every
	// nakama connection uses.
	Client *http.Client
}

// Check compares current with the latest release. It never fails: a
// failed request comes back as StatusUnknown with Err set, so that the
// caller can warn and carry on.
func (c Checker) Check(ctx context.Context, current string) CheckResult {
	res := CheckResult{Current: current, Status: StatusUnknown}
	tag, latest, err := c.latestRelease(ctx)
	if err != nil {
		res.Err = err
		return res
	}
	res.Latest = tag
	res.URL = config.ReleasesURL + "/tag/" + tag

	cur, ok := parseRelease(current)
	if !ok {
		res.Status = StatusDevelopment
		return res
	}
	switch c := compareSemver(cur, latest); {
	case c < 0:
		res.Status = StatusOutdated
	case c > 0:
		res.Status = StatusAhead
	default:
		res.Status = StatusUpToDate
	}
	return res
}

// latestRelease GETs the latest release and returns its tag, parsed.
func (c Checker) latestRelease(ctx context.Context) (string, semver, error) {
	target := c.URL
	if target == "" {
		target = config.LatestReleaseURL
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: CheckTimeout, Transport: proxy.NewTransport()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", semver{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "nakama/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return "", semver{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return "", semver{}, fmt.Errorf("GitHub answered with status %d: the API rate limit for this address is used up; try again later", resp.StatusCode)
		}
		return "", semver{}, fmt.Errorf("GitHub answered with status %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&rel); err != nil {
		return "", semver{}, fmt.Errorf("could not read GitHub's answer: %w", err)
	}
	v, ok := parseSemver(rel.TagName)
	if !ok {
		return "", semver{}, fmt.Errorf("the latest release's tag %q is not a version", rel.TagName)
	}
	return rel.TagName, v, nil
}

// semver is a parsed MAJOR.MINOR.PATCH[-PRERELEASE] (semver.org 2.0.0).
// Build metadata is dropped: it takes no part in precedence.
type semver struct {
	core [3]int
	pre  []string
}

// parseSemver parses a semantic version with or without a leading "v":
// tags are v0.24.0, while release binaries report 0.24.0 (GoReleaser's
// {{.Version}}).
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, ok := numericIdent(p)
		if !ok {
			return semver{}, false
		}
		v.core[i] = n
	}
	if !hasPre {
		return v, true
	}
	v.pre = strings.Split(pre, ".")
	for _, id := range v.pre {
		if !validPreIdent(id) {
			return semver{}, false
		}
	}
	return v, true
}

// describeSuffix matches what `git describe --tags --dirty` (make
// build) appends to a tag: the commits since it and the hash, and/or
// -dirty. Such a build is past its tag or has local changes.
var describeSuffix = regexp.MustCompile(`(?:-[0-9]+-g[0-9a-f]+(?:-dirty)?|-dirty)$`)

// parseRelease parses the running version, refusing the forms that are
// not a release: anything that is not a semantic version (dev, a bare
// commit hash) and git describe output, which would otherwise read as a
// prerelease of the tag it was built after.
func parseRelease(s string) (semver, bool) {
	if describeSuffix.MatchString(s) {
		return semver{}, false
	}
	return parseSemver(s)
}

// numericIdent parses a numeric identifier: digits only, no leading
// zero, within int.
func numericIdent(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// validPreIdent reports whether s is a prerelease identifier: [0-9A-Za-z-]+,
// and a valid numeric identifier when it is all digits.
func validPreIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' {
			return false
		}
	}
	if strings.Trim(s, "0123456789") == "" {
		_, ok := numericIdent(s)
		return ok
	}
	return true
}

// compareSemver returns -1, 0 or +1 by semver precedence: the numbers,
// then a version without a prerelease above one with, then the
// prerelease identifiers in order (numeric ones by value and below
// alphanumeric ones, which compare in ASCII order; more identifiers
// win when the shared ones are equal).
func compareSemver(a, b semver) int {
	for i := range a.core {
		if c := cmp.Compare(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	return slices.CompareFunc(a.pre, b.pre, comparePreIdent)
}

// comparePreIdent compares two valid prerelease identifiers.
func comparePreIdent(a, b string) int {
	an, aNum := numericIdent(a)
	bn, bNum := numericIdent(b)
	switch {
	case aNum && bNum:
		return cmp.Compare(an, bn)
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
