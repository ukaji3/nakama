package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/awsx"
)

// ErrACLEntryNotFound is returned by DeleteACLs when a supplied CIDR
// is not present in the WAFv2 IP set.
var ErrACLEntryNotFound = errors.New("acl entry not found")

// ACLEntry pairs an allowlist CIDR with the timestamp at which it
// was added (recorded in /nakama/_meta/acl/<safe-cidr>).
type ACLEntry struct {
	CIDR    string `json:"cidr"`
	AddedAt string `json:"added_at,omitempty"`
}

// ACLAddResult is what AddACL put in the allowlist.
type ACLAddResult struct {
	// Added are the canonical CIDRs, now all in the IP set: the given
	// one, or every detected address.
	Added []string
	// Detection is the auto-detection (nil when a CIDR was given).
	Detection *awsx.DetectedIPs
}

// AddACL inserts cidr into the shared WAFv2 IP set. With an empty
// cidr it detects the caller's public IPv4 addresses (every check site,
// IPCheckRounds times each; Issue #104) and inserts each distinct one,
// so that every exit of a network with redundant exits is allowed.
// The IP set is changed with at most one update. It is idempotent:
// entries already present are left alone (their added-at record is
// refreshed).
func (s *Service) AddACL(ctx context.Context, cidr string) (*ACLAddResult, error) {
	res := &ACLAddResult{}
	inputs := []string{cidr}
	if cidr == "" {
		det, err := s.clients.IPDiscoverer.DetectPublicIPv4s(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-detect public IP: %w", err)
		}
		res.Detection = det
		inputs = det.IPs
		slog.Info("acl: auto-detected caller IP", "ips", det.IPs, "checks", det.Checks, "failed_checks", len(det.Failures))
	}
	for _, in := range inputs {
		canonical, err := canonicalCIDR(in)
		if err != nil {
			return nil, err
		}
		if !containsCIDR(res.Added, canonical) {
			res.Added = append(res.Added, canonical)
		}
	}

	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return nil, err
	}
	addrs, lockToken, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, c := range res.Added {
		if containsCIDR(addrs, c) {
			slog.Info("acl: entry already present", "cidr", c)
		} else {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		addrs = append(append([]string(nil), addrs...), missing...)
		sort.Strings(addrs)
		if err := s.updateIPSetAddresses(ctx, ipset, addrs, lockToken); err != nil {
			return nil, err
		}
		slog.Info("acl: added", "cidrs", missing, "total_entries", len(addrs))
	}

	for _, c := range res.Added {
		if err := s.writeACLTimestamp(ctx, c); err != nil {
			// non-fatal; the IP set itself is the source of truth
			slog.Warn("acl: failed to record timestamp", "cidr", c, "error", err)
		}
	}
	return res, nil
}

// ListACL returns every CIDR currently in the WAFv2 IP set, paired
// with its added_at timestamp (best-effort, may be empty for
// entries created outside nakama).
func (s *Service) ListACL(ctx context.Context) ([]ACLEntry, error) {
	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return nil, err
	}
	addrs, _, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return nil, err
	}
	return aclEntries(addrs, s.fetchAllACLTimestamps(ctx)), nil
}

// aclEntries pairs the IP set addresses (sorted) with their timestamps.
func aclEntries(addrs []string, timestamps map[string]string) []ACLEntry {
	sorted := append([]string(nil), addrs...)
	sort.Strings(sorted)
	entries := make([]ACLEntry, 0, len(sorted))
	for _, a := range sorted {
		entries = append(entries, ACLEntry{CIDR: a, AddedAt: timestamps[a]})
	}
	return entries
}

// DeleteACLs removes every given IP or CIDR from the WAFv2 IP set in a
// single update and returns their canonical forms. It is all or
// nothing: when any of them is not in the IP set, nothing changes and
// the error wraps ErrACLEntryNotFound naming them as given. Duplicates
// (e.g. "203.0.113.5" and "203.0.113.5/32") count once.
func (s *Service) DeleteACLs(ctx context.Context, cidrs []string) ([]string, error) {
	if len(cidrs) == 0 {
		return nil, errors.New("no IP address or CIDR given")
	}
	var targets []string
	given := map[string]string{} // canonical -> as given, for the error
	for _, c := range cidrs {
		canonical, err := canonicalCIDR(c)
		if err != nil {
			return nil, err
		}
		if _, dup := given[canonical]; !dup {
			targets = append(targets, canonical)
			given[canonical] = strings.TrimSpace(c)
		}
	}

	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return nil, err
	}
	addrs, lockToken, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, t := range targets {
		if !containsCIDR(addrs, t) {
			missing = append(missing, given[t])
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrACLEntryNotFound, strings.Join(missing, ", "))
	}

	filtered := withoutCIDRs(addrs, targets)
	if err := s.updateIPSetAddresses(ctx, ipset, filtered, lockToken); err != nil {
		return nil, err
	}
	slog.Info("acl: deleted", "cidrs", targets, "remaining_entries", len(filtered))
	for _, t := range targets {
		s.deleteACLTimestamp(ctx, t)
	}
	return targets, nil
}

// ACLDeleteAllPlan is what `nakama aws acl delete all` will do,
// resolved with read-only calls so that the CLI can show it before
// asking.
type ACLDeleteAllPlan struct {
	// Delete are the entries to remove; Keep the ones --keep-mine keeps.
	Delete []ACLEntry
	Keep   []ACLEntry
	// MyIPs are the caller's public IPv4 addresses (--keep-mine only),
	// detected as `acl add` does; Detection is that detection.
	MyIPs     []string
	Detection *awsx.DetectedIPs
	KeepMine  bool
	// Stale are CIDRs that have an added-at record in SSM but are not
	// in the IP set (left behind by edits outside nakama). They are
	// removed too; that changes nobody's access.
	Stale []string
}

// ACLDeleteAllResult reports what `acl delete all` changed.
type ACLDeleteAllResult struct {
	Deleted []string `json:"deleted"`
	// Remaining is the IP set after the update: the kept entries plus
	// any added after the plan was made.
	Remaining []string `json:"remaining"`
	// MyIP is the first of MyIPs, kept for the output before Issue #104.
	MyIP  string   `json:"my_ip,omitempty"`
	MyIPs []string `json:"my_ips,omitempty"`
	// Gone are planned entries someone else removed in the meantime.
	Gone              []string `json:"gone,omitempty"`
	TimestampsRemoved int      `json:"timestamps_removed"`
}

// PlanACLDeleteAll lists the entries `acl delete all` removes. With
// keepMine, the caller's public IPs are detected (as `acl add` does)
// and every entry containing one of them is kept; detecting none is an
// error, so that the caller is never locked out by surprise.
func (s *Service) PlanACLDeleteAll(ctx context.Context, keepMine bool) (*ACLDeleteAllPlan, error) {
	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return nil, err
	}
	addrs, _, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return nil, err
	}
	timestamps := s.fetchAllACLTimestamps(ctx)
	p := &ACLDeleteAllPlan{KeepMine: keepMine}

	var mine []net.IP
	if keepMine {
		det, err := s.clients.IPDiscoverer.DetectPublicIPv4s(ctx)
		if err != nil {
			return nil, fmt.Errorf("detect your public IP for --keep-mine: %w", err)
		}
		for _, ip := range det.IPs {
			v4 := net.ParseIP(strings.TrimSpace(ip)).To4()
			if v4 == nil {
				return nil, fmt.Errorf("detect your public IP for --keep-mine: %q is not an IPv4 address", ip)
			}
			mine = append(mine, v4)
			p.MyIPs = append(p.MyIPs, v4.String())
		}
		if len(mine) == 0 {
			return nil, errors.New("detect your public IP for --keep-mine: no address found")
		}
		p.Detection = det
	}
	for _, e := range aclEntries(addrs, timestamps) {
		if cidrContainsAny(e.CIDR, mine) {
			p.Keep = append(p.Keep, e)
			continue
		}
		p.Delete = append(p.Delete, e)
	}
	for cidr := range timestamps {
		if !containsCIDR(addrs, cidr) {
			p.Stale = append(p.Stale, cidr)
		}
	}
	sort.Strings(p.Stale)
	return p, nil
}

// DeleteACLAll carries out a plan from PlanACLDeleteAll. It removes
// only the planned entries: one added after the plan (that the user
// did not see) stays. The IP set is changed with a single update.
func (s *Service) DeleteACLAll(ctx context.Context, p *ACLDeleteAllPlan) (*ACLDeleteAllResult, error) {
	if p == nil {
		return nil, errors.New("acl delete all: no plan")
	}
	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return nil, err
	}
	addrs, lockToken, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return nil, err
	}
	res := &ACLDeleteAllResult{MyIPs: p.MyIPs, Deleted: []string{}}
	if len(p.MyIPs) > 0 {
		res.MyIP = p.MyIPs[0]
	}
	var planned []string
	for _, e := range p.Delete {
		if containsCIDR(addrs, e.CIDR) {
			planned = append(planned, e.CIDR)
		} else {
			res.Gone = append(res.Gone, e.CIDR)
		}
	}
	remaining := withoutCIDRs(addrs, planned)
	if len(planned) > 0 {
		if err := s.updateIPSetAddresses(ctx, ipset, remaining, lockToken); err != nil {
			return nil, err
		}
		slog.Info("acl: deleted all", "deleted", len(planned), "remaining_entries", len(remaining))
	}
	res.Deleted = append(res.Deleted, planned...)
	res.Remaining = append([]string{}, remaining...)
	sort.Strings(res.Remaining)

	// The added-at records of the deleted entries, and the stale ones
	// that still have no entry.
	cleanup := append(append([]string(nil), planned...), res.Gone...)
	for _, c := range p.Stale {
		if !containsCIDR(remaining, c) {
			cleanup = append(cleanup, c)
		}
	}
	for _, c := range cleanup {
		if s.deleteACLTimestamp(ctx, c) {
			res.TimestampsRemoved++
		}
	}
	return res, nil
}

// --- helpers --------------------------------------------------------------

type ipSetSummary struct {
	id   string
	name string
	arn  string
}

func (s *Service) findIPSetSummary(ctx context.Context) (ipSetSummary, error) {
	all, err := s.listAllIPSets(ctx)
	if err != nil {
		return ipSetSummary{}, err
	}
	want := "nakama-allowed-ips"
	for _, ip := range all {
		if ip.Name != nil && *ip.Name == want && ip.Id != nil && ip.ARN != nil {
			return ipSetSummary{id: *ip.Id, name: *ip.Name, arn: *ip.ARN}, nil
		}
	}
	return ipSetSummary{}, fmt.Errorf("WAFv2 IP set %q not found; run \"nakama aws init\" first", want)
}

func (s *Service) fetchIPSetAddresses(ctx context.Context, id, name string) ([]string, string, error) {
	out, err := s.clients.WAF.GetIPSet(ctx, &wafv2.GetIPSetInput{
		Id:    aws.String(id),
		Name:  aws.String(name),
		Scope: scopeCloudFront,
	})
	if err != nil {
		return nil, "", fmt.Errorf("GetIPSet: %w", err)
	}
	if out.IPSet == nil || out.LockToken == nil {
		return nil, "", errors.New("GetIPSet returned nil")
	}
	return out.IPSet.Addresses, *out.LockToken, nil
}

// withoutCIDRs returns addrs minus remove. The result is never nil:
// UpdateIPSet requires the Addresses field even when it is empty.
func withoutCIDRs(addrs, remove []string) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if !containsCIDR(remove, a) {
			out = append(out, a)
		}
	}
	return out
}

// cidrContainsAny reports whether the CIDR includes one of ips. An
// unparsable CIDR (WAF does not accept one) contains nothing.
func cidrContainsAny(cidr string, ips []net.IP) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// updateIPSetAddresses replaces the IP set's addresses. The lock token
// from the read makes WAF reject the update if the IP set changed in
// between (WAFOptimisticLockException).
func (s *Service) updateIPSetAddresses(ctx context.Context, ipset ipSetSummary, addrs []string, lockToken string) error {
	if addrs == nil {
		addrs = []string{}
	}
	if _, err := s.clients.WAF.UpdateIPSet(ctx, &wafv2.UpdateIPSetInput{
		Name:      aws.String(ipset.name),
		Id:        aws.String(ipset.id),
		Scope:     scopeCloudFront,
		Addresses: addrs,
		LockToken: aws.String(lockToken),
	}); err != nil {
		return fmt.Errorf("UpdateIPSet: %w", err)
	}
	return nil
}

// deleteACLTimestamp removes the added-at record of a CIDR and reports
// whether one was removed. The IP set is the source of truth, so a
// failure only warns.
func (s *Service) deleteACLTimestamp(ctx context.Context, canonical string) bool {
	if _, err := s.clients.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(aclTimestampPath(canonical)),
	}); err != nil {
		if !awshelper.IsAPIError(err, "ParameterNotFound") {
			slog.Warn("acl: failed to delete timestamp parameter", "cidr", canonical, "error", err)
		}
		return false
	}
	return true
}

func containsCIDR(addrs []string, target string) bool {
	for _, a := range addrs {
		if a == target {
			return true
		}
	}
	return false
}

// canonicalCIDR normalizes a single IPv4 address or CIDR into its
// /32-completed canonical form ("203.0.113.5" -> "203.0.113.5/32").
// IPv6 is rejected in v1.
func canonicalCIDR(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty CIDR")
	}
	if !strings.Contains(s, "/") {
		// bare IP; require IPv4 and append /32
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("%q is not a valid IPv4 address", s)
		}
		return ip.To4().String() + "/32", nil
	}
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", s, err)
	}
	if ip.To4() == nil {
		return "", fmt.Errorf("%q is IPv6; nakama v1 supports IPv4 only", s)
	}
	return ipnet.String(), nil
}

// aclTimestampPath returns the SSM path used to record when a CIDR
// was added. Slashes in the CIDR are encoded as underscores so the
// resulting path has no extra hierarchy.
func aclTimestampPath(canonical string) string {
	safe := strings.ReplaceAll(canonical, "/", "_")
	return "/nakama/_meta/acl/" + safe
}

// aclCIDRFromPath reverses aclTimestampPath: given an SSM parameter
// name under /nakama/_meta/acl/, return the original CIDR.
func aclCIDRFromPath(name string) string {
	const prefix = "/nakama/_meta/acl/"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	return strings.ReplaceAll(name[len(prefix):], "_", "/")
}

func (s *Service) writeACLTimestamp(ctx context.Context, canonical string) error {
	_, err := s.clients.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(aclTimestampPath(canonical)),
		Value:     aws.String(time.Now().UTC().Format(time.RFC3339)),
		Type:      ssmtypes.ParameterTypeString,
		Overwrite: aws.Bool(true),
	})
	return err
}

// fetchAllACLTimestamps returns a CIDR -> timestamp map populated
// from SSM. Errors are logged and an empty map is returned.
func (s *Service) fetchAllACLTimestamps(ctx context.Context) map[string]string {
	out := map[string]string{}
	var nextToken *string
	for {
		page, err := s.clients.SSM.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:      aws.String("/nakama/_meta/acl/"),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		})
		if err != nil {
			slog.Warn("acl: GetParametersByPath failed", "error", err)
			return out
		}
		for _, p := range page.Parameters {
			if p.Name == nil || p.Value == nil {
				continue
			}
			cidr := aclCIDRFromPath(*p.Name)
			if cidr != "" {
				out[cidr] = *p.Value
			}
		}
		if page.NextToken == nil || *page.NextToken == "" {
			break
		}
		nextToken = page.NextToken
	}
	return out
}
