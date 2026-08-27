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
)

// ErrACLEntryNotFound is returned by DeleteACL when the supplied
// CIDR is not present in the WAFv2 IP set.
var ErrACLEntryNotFound = errors.New("acl entry not found")

// ACLEntry pairs an allowlist CIDR with the timestamp at which it
// was added (recorded in /nakama/_meta/acl/<safe-cidr>).
type ACLEntry struct {
	CIDR    string `json:"cidr"`
	AddedAt string `json:"added_at,omitempty"`
}

// AddACL inserts cidr (or the caller's public IP if cidr is empty)
// into the shared WAFv2 IP set. The function is idempotent: if the
// entry already exists, no WAF update is performed and the SSM
// timestamp is preserved.
func (s *Service) AddACL(ctx context.Context, cidr string) (string, error) {
	if cidr == "" {
		ip, err := s.clients.IPDiscoverer.PublicIPv4(ctx)
		if err != nil {
			return "", fmt.Errorf("auto-detect public IP: %w", err)
		}
		cidr = ip
		slog.Info("acl: auto-detected caller IP", "ip", ip)
	}

	canonical, err := canonicalCIDR(cidr)
	if err != nil {
		return "", err
	}

	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return "", err
	}

	addrs, lockToken, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return "", err
	}

	if containsCIDR(addrs, canonical) {
		slog.Info("acl: entry already present", "cidr", canonical)
	} else {
		addrs = append(addrs, canonical)
		sort.Strings(addrs)
		if _, err := s.clients.WAF.UpdateIPSet(ctx, &wafv2.UpdateIPSetInput{
			Name:      aws.String(ipset.name),
			Id:        aws.String(ipset.id),
			Scope:     scopeCloudFront,
			Addresses: addrs,
			LockToken: aws.String(lockToken),
		}); err != nil {
			return "", fmt.Errorf("UpdateIPSet: %w", err)
		}
		slog.Info("acl: added", "cidr", canonical, "total_entries", len(addrs))
	}

	if err := s.writeACLTimestamp(ctx, canonical); err != nil {
		// non-fatal; the IP set itself is the source of truth
		slog.Warn("acl: failed to record timestamp", "cidr", canonical, "error", err)
	}
	return canonical, nil
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
	sort.Strings(addrs)

	timestamps := s.fetchAllACLTimestamps(ctx)
	entries := make([]ACLEntry, 0, len(addrs))
	for _, a := range addrs {
		entries = append(entries, ACLEntry{
			CIDR:    a,
			AddedAt: timestamps[a],
		})
	}
	return entries, nil
}

// DeleteACL removes cidr from the WAFv2 IP set. Returns
// ErrACLEntryNotFound if the entry is not present.
func (s *Service) DeleteACL(ctx context.Context, cidr string) error {
	canonical, err := canonicalCIDR(cidr)
	if err != nil {
		return err
	}

	ipset, err := s.findIPSetSummary(ctx)
	if err != nil {
		return err
	}
	addrs, lockToken, err := s.fetchIPSetAddresses(ctx, ipset.id, ipset.name)
	if err != nil {
		return err
	}

	filtered := make([]string, 0, len(addrs))
	removed := false
	for _, a := range addrs {
		if a == canonical {
			removed = true
			continue
		}
		filtered = append(filtered, a)
	}
	if !removed {
		return ErrACLEntryNotFound
	}

	if _, err := s.clients.WAF.UpdateIPSet(ctx, &wafv2.UpdateIPSetInput{
		Name:      aws.String(ipset.name),
		Id:        aws.String(ipset.id),
		Scope:     scopeCloudFront,
		Addresses: filtered,
		LockToken: aws.String(lockToken),
	}); err != nil {
		return fmt.Errorf("UpdateIPSet: %w", err)
	}
	slog.Info("acl: deleted", "cidr", canonical, "remaining_entries", len(filtered))

	if _, err := s.clients.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(aclTimestampPath(canonical)),
	}); err != nil {
		// Tolerate ParameterNotFound; anything else is logged but non-fatal.
		if !awshelper.IsAPIError(err, "ParameterNotFound") {
			slog.Warn("acl: failed to delete timestamp parameter", "cidr", canonical, "error", err)
		}
	}
	return nil
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
