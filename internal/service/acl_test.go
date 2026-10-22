package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

func TestCanonicalCIDR(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"203.0.113.5", "203.0.113.5/32", false},
		{"203.0.113.5/32", "203.0.113.5/32", false},
		{"203.0.113.0/24", "203.0.113.0/24", false},
		{" 203.0.113.5 ", "203.0.113.5/32", false},
		{"203.0.113.5/24", "203.0.113.0/24", false}, // ParseCIDR truncates host bits
		{"203.0.113.999", "", true},
		{"not-an-ip", "", true},
		{"", "", true},
		{"::1", "", true}, // IPv6 explicitly rejected in v1
		{"2001:db8::/32", "", true},
	}
	for _, c := range cases {
		got, err := canonicalCIDR(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("canonicalCIDR(%q) expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("canonicalCIDR(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("canonicalCIDR(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestACLPathRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{
		"203.0.113.5/32",
		"203.0.113.0/24",
		"10.0.0.0/8",
	}
	for _, c := range cases {
		path := aclTimestampPath(c)
		if !strings.HasPrefix(path, "/nakama/_meta/acl/") {
			t.Errorf("path=%q must be under /nakama/_meta/acl/", path)
		}
		if strings.Contains(path[len("/nakama/_meta/acl/"):], "/") {
			t.Errorf("path=%q must not contain raw / after the prefix", path)
		}
		if got := aclCIDRFromPath(path); got != c {
			t.Errorf("aclCIDRFromPath(%q)=%q, want %q", path, got, c)
		}
	}
}

func TestContainsCIDR(t *testing.T) {
	t.Parallel()
	addrs := []string{"a", "b", "c"}
	if !containsCIDR(addrs, "b") {
		t.Error("expected b to be present")
	}
	if containsCIDR(addrs, "z") {
		t.Error("expected z to be absent")
	}
}

// --- fakes for the allowlist ------------------------------------------------

// fakeACLWAF holds the nakama IP set and records every UpdateIPSet.
type fakeACLWAF struct {
	awsx.WAFAPI
	addrs   []string
	updates [][]string
}

func (f *fakeACLWAF) ListIPSets(context.Context, *wafv2.ListIPSetsInput, ...func(*wafv2.Options)) (*wafv2.ListIPSetsOutput, error) {
	return &wafv2.ListIPSetsOutput{IPSets: []wafv2types.IPSetSummary{{
		Name: aws.String("nakama-allowed-ips"), Id: aws.String("ipset-1"), ARN: aws.String("arn:aws:wafv2:us-east-1:1:global/ipset/nakama-allowed-ips/ipset-1"),
	}}}, nil
}

func (f *fakeACLWAF) GetIPSet(context.Context, *wafv2.GetIPSetInput, ...func(*wafv2.Options)) (*wafv2.GetIPSetOutput, error) {
	return &wafv2.GetIPSetOutput{
		IPSet:     &wafv2types.IPSet{Addresses: append([]string(nil), f.addrs...)},
		LockToken: aws.String("lock-1"),
	}, nil
}

func (f *fakeACLWAF) UpdateIPSet(_ context.Context, in *wafv2.UpdateIPSetInput, _ ...func(*wafv2.Options)) (*wafv2.UpdateIPSetOutput, error) {
	if in.Addresses == nil {
		// The real SDK rejects this before sending (validateOpUpdateIPSetInput).
		return nil, errors.New("Addresses is required")
	}
	f.updates = append(f.updates, append([]string{}, in.Addresses...))
	f.addrs = append([]string(nil), in.Addresses...)
	return &wafv2.UpdateIPSetOutput{}, nil
}

// fakeACLSSM holds the added-at records under /nakama/_meta/acl/.
type fakeACLSSM struct {
	awsx.SSMAPI
	params  map[string]string
	deleted []string
}

func (f *fakeACLSSM) GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	out := &ssm.GetParametersByPathOutput{}
	for k, v := range f.params {
		out.Parameters = append(out.Parameters, ssmtypes.Parameter{Name: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeACLSSM) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	name := aws.ToString(in.Name)
	if _, ok := f.params[name]; !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	delete(f.params, name)
	f.deleted = append(f.deleted, aclCIDRFromPath(name))
	return &ssm.DeleteParameterOutput{}, nil
}

type fakeIP struct {
	ip  string
	ips []string // DetectPublicIPv4s; nil means {ip}
	err error
	// failures are failed checks DetectPublicIPv4s reports beside ips.
	failures []awsx.IPCheckFailure
	calls    int
}

func (f *fakeIP) PublicIPv4(context.Context) (string, error) {
	f.calls++
	return f.ip, f.err
}

func (f *fakeIP) DetectPublicIPv4s(context.Context) (*awsx.DetectedIPs, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	ips := f.ips
	if ips == nil {
		ips = []string{f.ip}
	}
	return &awsx.DetectedIPs{IPs: ips, Checks: 6, Failures: f.failures}, nil
}

// aclService: the IP set holds addrs, and every CIDR in stamped has an
// added-at record.
func aclService(addrs []string, stamped ...string) (*Service, *fakeACLWAF, *fakeACLSSM, *fakeIP) {
	waf := &fakeACLWAF{addrs: addrs}
	ssmf := &fakeACLSSM{params: map[string]string{}}
	for _, c := range stamped {
		ssmf.params[aclTimestampPath(c)] = "2026-09-01T00:00:00Z"
	}
	ip := &fakeIP{ip: "153.242.166.137"}
	return &Service{clients: &awsx.Clients{WAF: waf, SSM: ssmf, IPDiscoverer: ip}}, waf, ssmf, ip
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

// --- DeleteACLs -----------------------------------------------------------

func TestDeleteACLs_RemovesSeveralInOneUpdate(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := aclService(
		[]string{"203.0.113.5/32", "198.51.100.0/24", "192.0.2.7/32"},
		"203.0.113.5/32", "198.51.100.0/24", "192.0.2.7/32")
	got, err := svc.DeleteACLs(context.Background(), []string{"203.0.113.5", "198.51.100.0/24", "203.0.113.5/32"})
	if err != nil {
		t.Fatalf("DeleteACLs: %v", err)
	}
	if strings.Join(got, ",") != "203.0.113.5/32,198.51.100.0/24" {
		t.Errorf("deleted = %v, want the two canonical CIDRs once each", got)
	}
	if len(waf.updates) != 1 || strings.Join(waf.updates[0], ",") != "192.0.2.7/32" {
		t.Errorf("updates = %v, want one update leaving 192.0.2.7/32", waf.updates)
	}
	if strings.Join(sortedCopy(ssmf.deleted), ",") != "198.51.100.0/24,203.0.113.5/32" {
		t.Errorf("added-at records deleted = %v", ssmf.deleted)
	}
}

func TestDeleteACLs_IsAllOrNothing(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := aclService([]string{"203.0.113.5/32"}, "203.0.113.5/32")
	_, err := svc.DeleteACLs(context.Background(), []string{"203.0.113.5", "198.51.100.9", "192.0.2.0/24"})
	if !errors.Is(err, ErrACLEntryNotFound) {
		t.Fatalf("error = %v, want ErrACLEntryNotFound", err)
	}
	// The missing ones are named as given, as the single-entry error was.
	if !strings.Contains(err.Error(), "198.51.100.9, 192.0.2.0/24") || strings.Contains(err.Error(), "203.0.113.5") {
		t.Errorf("error = %q, want only the missing entries as given", err)
	}
	if len(waf.updates) != 0 || len(ssmf.deleted) != 0 {
		t.Errorf("nothing may change: updates %v, ssm %v", waf.updates, ssmf.deleted)
	}
	if _, err := svc.DeleteACLs(context.Background(), []string{"203.0.113.5", "not-an-ip"}); err == nil || len(waf.updates) != 0 {
		t.Errorf("an invalid address must fail before any change: err %v, updates %v", err, waf.updates)
	}
}

func TestDeleteACLs_LastEntryLeavesAnEmptyNonNilList(t *testing.T) {
	t.Parallel()
	svc, waf, _, _ := aclService([]string{"203.0.113.5/32"})
	if _, err := svc.DeleteACLs(context.Background(), []string{"203.0.113.5"}); err != nil {
		t.Fatalf("DeleteACLs: %v", err)
	}
	if len(waf.updates) != 1 || waf.updates[0] == nil || len(waf.updates[0]) != 0 {
		t.Errorf("updates = %#v, want one update with an empty list", waf.updates)
	}
}

// --- delete all -----------------------------------------------------------

func TestPlanACLDeleteAll_EverythingAndTheStaleRecords(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, ip := aclService(
		[]string{"203.0.113.5/32", "153.242.166.137/32", "10.0.0.0/8"},
		"203.0.113.5/32", "153.242.166.137/32", "192.0.2.1/32") // 192.0.2.1 has a record but no entry
	p, err := svc.PlanACLDeleteAll(context.Background(), false)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	var cidrs []string
	for _, e := range p.Delete {
		cidrs = append(cidrs, e.CIDR)
	}
	if strings.Join(cidrs, ",") != "10.0.0.0/8,153.242.166.137/32,203.0.113.5/32" || len(p.Keep) != 0 {
		t.Errorf("plan delete = %v keep = %v, want every entry sorted", cidrs, p.Keep)
	}
	if p.Delete[0].AddedAt != "" || p.Delete[2].AddedAt == "" {
		t.Errorf("added-at not paired: %+v", p.Delete)
	}
	if strings.Join(p.Stale, ",") != "192.0.2.1/32" {
		t.Errorf("stale = %v", p.Stale)
	}
	if ip.calls != 0 || p.MyIP != "" {
		t.Errorf("without --keep-mine the IP is not detected (calls %d)", ip.calls)
	}
	if len(waf.updates) != 0 || len(ssmf.deleted) != 0 {
		t.Errorf("planning must not change anything")
	}
}

func TestPlanACLDeleteAll_KeepMine(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := aclService([]string{"203.0.113.5/32", "153.242.166.137/32", "153.242.0.0/16", "153.243.0.0/16"})
	p, err := svc.PlanACLDeleteAll(context.Background(), true)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	var keep, del []string
	for _, e := range p.Keep {
		keep = append(keep, e.CIDR)
	}
	for _, e := range p.Delete {
		del = append(del, e.CIDR)
	}
	// Every entry that contains the IP is kept, not only the /32.
	if strings.Join(keep, ",") != "153.242.0.0/16,153.242.166.137/32" || strings.Join(del, ",") != "153.243.0.0/16,203.0.113.5/32" {
		t.Errorf("keep = %v, delete = %v", keep, del)
	}
	if p.MyIP != "153.242.166.137" || !p.KeepMine {
		t.Errorf("plan = %+v", p)
	}
}

func TestPlanACLDeleteAll_KeepMineFailsClosed(t *testing.T) {
	t.Parallel()
	for ip, cause := range map[*fakeIP]string{{err: errors.New("dial tcp: timeout")}: "timeout", {ip: "2001:db8::1"}: "not an IPv4"} {
		svc, waf, _, _ := aclService([]string{"203.0.113.5/32"})
		svc.clients.IPDiscoverer = ip
		if _, err := svc.PlanACLDeleteAll(context.Background(), true); err == nil ||
			!strings.Contains(err.Error(), "--keep-mine") || !strings.Contains(err.Error(), cause) {
			t.Errorf("error = %v, want a --keep-mine detection error saying %q", err, cause)
		}
		if len(waf.updates) != 0 {
			t.Errorf("nothing may change")
		}
	}
}

func TestDeleteACLAll_RemovesOnlyWhatWasPlanned(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := aclService(
		[]string{"203.0.113.5/32", "198.51.100.0/24", "153.242.166.137/32"},
		"203.0.113.5/32", "198.51.100.0/24", "153.242.166.137/32", "192.0.2.1/32", "192.0.2.2/32")
	p, err := svc.PlanACLDeleteAll(context.Background(), true)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	// Between the question and the answer: someone adds 192.0.2.2 (it had
	// a stale record), and someone else removes 198.51.100.0/24.
	waf.addrs = []string{"203.0.113.5/32", "153.242.166.137/32", "192.0.2.2/32"}

	res, err := svc.DeleteACLAll(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteACLAll: %v", err)
	}
	if len(waf.updates) != 1 || strings.Join(sortedCopy(waf.updates[0]), ",") != "153.242.166.137/32,192.0.2.2/32" {
		t.Errorf("updates = %v, want one update keeping my IP and the new entry", waf.updates)
	}
	if strings.Join(res.Deleted, ",") != "203.0.113.5/32" || strings.Join(res.Gone, ",") != "198.51.100.0/24" {
		t.Errorf("result = %+v", res)
	}
	if strings.Join(res.Remaining, ",") != "153.242.166.137/32,192.0.2.2/32" {
		t.Errorf("remaining = %v", res.Remaining)
	}
	// Records: the deleted, the gone, and the stale one still without an
	// entry; not my IP's, and not 192.0.2.2's (it has an entry now).
	if got := strings.Join(sortedCopy(ssmf.deleted), ","); got != "192.0.2.1/32,198.51.100.0/24,203.0.113.5/32" || res.TimestampsRemoved != 3 {
		t.Errorf("records deleted = %s (count %d)", got, res.TimestampsRemoved)
	}
}

func TestDeleteACLAll_EmptiesTheIPSet(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := aclService([]string{"203.0.113.5/32", "10.0.0.0/8"}, "203.0.113.5/32")
	p, err := svc.PlanACLDeleteAll(context.Background(), false)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	res, err := svc.DeleteACLAll(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteACLAll: %v", err)
	}
	if len(waf.updates) != 1 || waf.updates[0] == nil || len(waf.updates[0]) != 0 {
		t.Errorf("updates = %#v, want a single update to an empty list", waf.updates)
	}
	if len(res.Deleted) != 2 || len(res.Remaining) != 0 || res.Remaining == nil || len(ssmf.params) != 0 {
		t.Errorf("result = %+v, records left %v", res, ssmf.params)
	}
}

func TestDeleteACLAll_AlreadyEmptyChangesNothing(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := aclService(nil, "192.0.2.1/32")
	p, err := svc.PlanACLDeleteAll(context.Background(), false)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	if len(p.Delete) != 0 || len(p.Stale) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	res, err := svc.DeleteACLAll(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteACLAll: %v", err)
	}
	if len(waf.updates) != 0 {
		t.Errorf("an empty IP set must not be updated: %v", waf.updates)
	}
	if res.TimestampsRemoved != 1 || len(ssmf.params) != 0 || res.Deleted == nil {
		t.Errorf("the stale record should go: %+v", res)
	}
}
