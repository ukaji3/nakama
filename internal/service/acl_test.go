package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

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
	pathErr error // GetParametersByPath fails with it
}

func (f *fakeACLSSM) GetParametersByPath(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	if f.pathErr != nil {
		return nil, f.pathErr
	}
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

func (f *fakeACLSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if !aws.ToBool(in.Overwrite) {
		return nil, errors.New("the added-at record is overwritten")
	}
	f.params[aws.ToString(in.Name)] = aws.ToString(in.Value)
	return &ssm.PutParameterOutput{}, nil
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
	if ip.calls != 0 || p.MyIPs != nil || p.Detection != nil {
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
	if strings.Join(p.MyIPs, ",") != "153.242.166.137" || !p.KeepMine || p.Detection == nil {
		t.Errorf("plan = %+v", p)
	}
}

// TestPlanACLDeleteAll_KeepMineKeepsEveryExit: with redundant exits,
// the entries of every detected exit are kept, as acl add added them.
func TestPlanACLDeleteAll_KeepMineKeepsEveryExit(t *testing.T) {
	t.Parallel()
	svc, _, _, ip := aclService([]string{"203.0.113.5/32", "198.51.100.1/32", "198.51.100.2/32", "192.0.2.0/24"})
	ip.ips = []string{"198.51.100.1", "192.0.2.77"}
	p, err := svc.PlanACLDeleteAll(context.Background(), true)
	if err != nil {
		t.Fatalf("PlanACLDeleteAll: %v", err)
	}
	var keep []string
	for _, e := range p.Keep {
		keep = append(keep, e.CIDR)
	}
	if strings.Join(keep, ",") != "192.0.2.0/24,198.51.100.1/32" || len(p.Delete) != 2 || strings.Join(p.MyIPs, ",") != "198.51.100.1,192.0.2.77" {
		t.Errorf("keep = %v, delete = %v, my IPs = %v", keep, p.Delete, p.MyIPs)
	}
	res, err := svc.DeleteACLAll(context.Background(), p)
	if err != nil {
		t.Fatalf("DeleteACLAll: %v", err)
	}
	// my_ip stays for the JSON output from before Issue #104.
	if res.MyIP != "198.51.100.1" || strings.Join(res.MyIPs, ",") != "198.51.100.1,192.0.2.77" {
		t.Errorf("result my IPs = %q, %v", res.MyIP, res.MyIPs)
	}
}

func TestAddACL_AddsEveryDetectedExitInOneUpdate(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, ip := aclService([]string{"203.0.113.5/32", "198.51.100.2/32"}, "198.51.100.2/32")
	ip.ips = []string{"198.51.100.1", "198.51.100.2", "203.0.113.9"}
	ip.failures = []awsx.IPCheckFailure{{Site: "ifconfig.io", Err: errors.New("timeout")}}
	res, err := svc.AddACL(context.Background(), "")
	if err != nil {
		t.Fatalf("AddACL: %v", err)
	}
	if strings.Join(res.Added, ",") != "198.51.100.1/32,198.51.100.2/32,203.0.113.9/32" {
		t.Errorf("added = %v", res.Added)
	}
	if res.Detection == nil || len(res.Detection.Failures) != 1 {
		t.Errorf("the detection (with its failures) must reach the caller: %+v", res.Detection)
	}
	if len(waf.updates) != 1 || strings.Join(waf.updates[0], ",") != "198.51.100.1/32,198.51.100.2/32,203.0.113.5/32,203.0.113.9/32" {
		t.Errorf("updates = %v, want one update adding the two missing exits", waf.updates)
	}
	for _, c := range res.Added {
		if v := ssmf.params[aclTimestampPath(c)]; v == "" || v == "2026-09-01T00:00:00Z" {
			t.Errorf("added-at of %s = %q, want it refreshed", c, v)
		}
	}
}

func TestAddACL_AllPresentChangesNoEntry(t *testing.T) {
	t.Parallel()
	svc, waf, _, ip := aclService([]string{"198.51.100.1/32", "198.51.100.2/32"})
	ip.ips = []string{"198.51.100.2", "198.51.100.1"}
	res, err := svc.AddACL(context.Background(), "")
	if err != nil || len(res.Added) != 2 || len(waf.updates) != 0 {
		t.Errorf("AddACL = %+v, %v; updates %v (want none)", res, err, waf.updates)
	}
}

func TestAddACL_GivenCIDRSkipsTheDetection(t *testing.T) {
	t.Parallel()
	svc, waf, _, ip := aclService([]string{"203.0.113.5/32"})
	res, err := svc.AddACL(context.Background(), "198.51.100.7")
	if err != nil || strings.Join(res.Added, ",") != "198.51.100.7/32" || res.Detection != nil || ip.calls != 0 {
		t.Errorf("AddACL = %+v, %v; detection calls %d", res, err, ip.calls)
	}
	if len(waf.updates) != 1 || strings.Join(waf.updates[0], ",") != "198.51.100.7/32,203.0.113.5/32" {
		t.Errorf("updates = %v", waf.updates)
	}
}

func TestAddACL_DetectionFailureAddsNothing(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, ip := aclService([]string{"203.0.113.5/32"})
	ip.err = errors.New("no check site answered with a public IPv4 address")
	if _, err := svc.AddACL(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "auto-detect public IP: no check site") {
		t.Errorf("AddACL error = %v", err)
	}
	if len(waf.updates) != 0 || len(ssmf.params) != 0 {
		t.Errorf("nothing may change: updates %v, params %v", waf.updates, ssmf.params)
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

// --- prune ----------------------------------------------------------------

// pruneCutoff is the cutoff of the prune tests: entries last confirmed
// before it are old.
var pruneCutoff = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

// pruneService: the IP set holds the keys of stamps; a non-empty value
// is the entry's added-at record. The caller's IP is 153.242.166.137.
func pruneService(stamps map[string]string) (*Service, *fakeACLWAF, *fakeACLSSM, *fakeIP) {
	var addrs []string
	for c := range stamps {
		addrs = append(addrs, c)
	}
	svc, waf, ssmf, ip := aclService(sortedCopy(addrs))
	for c, ts := range stamps {
		if ts != "" {
			ssmf.params[aclTimestampPath(c)] = ts
		}
	}
	return svc, waf, ssmf, ip
}

func cidrsOf(entries []ACLEntry) string {
	var out []string
	for _, e := range entries {
		out = append(out, e.CIDR)
	}
	return strings.Join(out, ",")
}

// mixedPruneStamps has one entry of every kind, the boundary included.
var mixedPruneStamps = map[string]string{
	"203.0.113.5/32":  "2026-09-01T00:00:00Z", // old
	"198.51.100.9/32": "2026-10-06T23:59:59Z", // one second before the cutoff: old
	"198.51.100.8/32": "2026-10-07T00:00:00Z", // exactly the cutoff: not old
	"198.51.100.7/32": "2026-11-01T00:00:00Z", // recent
	"153.242.0.0/16":  "2026-08-01T00:00:00Z", // old, but contains my IP
	"10.0.0.0/8":      "",                     // added outside nakama
	"192.0.2.0/24":    "not-a-time",           // a record that does not parse
}

func TestPlanACLPrune_SortsTheEntries(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, ip := pruneService(mixedPruneStamps)
	p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
	if err != nil {
		t.Fatalf("PlanACLPrune: %v", err)
	}
	if got := cidrsOf(p.Delete); got != "198.51.100.9/32,203.0.113.5/32" {
		t.Errorf("delete = %s", got)
	}
	if got := cidrsOf(p.Keep); got != "198.51.100.7/32,198.51.100.8/32" {
		t.Errorf("keep = %s, want the recent one and the one exactly at the cutoff", got)
	}
	if got := cidrsOf(p.Mine); got != "153.242.0.0/16" {
		t.Errorf("mine = %s", got)
	}
	if got := cidrsOf(p.Unrecorded); got != "10.0.0.0/8,192.0.2.0/24" {
		t.Errorf("unrecorded = %s", got)
	}
	if strings.Join(p.MyIPs, ",") != "153.242.166.137" || p.Detection == nil || ip.calls != 1 || !p.Cutoff.Equal(pruneCutoff) {
		t.Errorf("plan = %+v (detections %d)", p, ip.calls)
	}
	if len(waf.updates) != 0 || len(ssmf.deleted) != 0 {
		t.Errorf("planning must not change anything")
	}
}

func TestPruneACL_DeletesOnlyTheOldEntries(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := pruneService(mixedPruneStamps)
	p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
	if err != nil {
		t.Fatalf("PlanACLPrune: %v", err)
	}
	res, err := svc.PruneACL(context.Background(), p)
	if err != nil {
		t.Fatalf("PruneACL: %v", err)
	}
	want := "10.0.0.0/8,153.242.0.0/16,192.0.2.0/24,198.51.100.7/32,198.51.100.8/32"
	if len(waf.updates) != 1 || strings.Join(sortedCopy(waf.updates[0]), ",") != want {
		t.Errorf("updates = %v, want one update keeping %s", waf.updates, want)
	}
	if strings.Join(res.Deleted, ",") != "198.51.100.9/32,203.0.113.5/32" || strings.Join(res.Remaining, ",") != want {
		t.Errorf("result = %+v", res)
	}
	if got := strings.Join(sortedCopy(ssmf.deleted), ","); got != "198.51.100.9/32,203.0.113.5/32" || res.TimestampsRemoved != 2 {
		t.Errorf("records deleted = %s (count %d), want only the deleted entries'", got, res.TimestampsRemoved)
	}
}

// TestPruneACL_ActsOnTheIPSetReadAgain: what changed between the
// question and the answer is respected.
func TestPruneACL_ActsOnTheIPSetReadAgain(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := pruneService(map[string]string{
		"203.0.113.5/32":  "2026-09-01T00:00:00Z",
		"203.0.113.6/32":  "2026-09-01T00:00:00Z",
		"198.51.100.9/32": "2026-09-01T00:00:00Z",
		"198.51.100.7/32": "2026-11-01T00:00:00Z",
	})
	p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
	if err != nil {
		t.Fatalf("PlanACLPrune: %v", err)
	}
	if got := cidrsOf(p.Delete); got != "198.51.100.9/32,203.0.113.5/32,203.0.113.6/32" {
		t.Fatalf("delete = %s", got)
	}
	// Meanwhile: someone adds 192.0.2.50 (with acl add), someone removes
	// 203.0.113.6 outside nakama (its record stays), and someone runs
	// acl add from 198.51.100.9 (its record is refreshed).
	waf.addrs = []string{"198.51.100.7/32", "198.51.100.9/32", "192.0.2.50/32", "203.0.113.5/32"}
	ssmf.params[aclTimestampPath("192.0.2.50/32")] = "2026-11-06T00:00:00Z"
	ssmf.params[aclTimestampPath("198.51.100.9/32")] = "2026-11-06T00:00:00Z"

	res, err := svc.PruneACL(context.Background(), p)
	if err != nil {
		t.Fatalf("PruneACL: %v", err)
	}
	if len(waf.updates) != 1 || strings.Join(sortedCopy(waf.updates[0]), ",") != "192.0.2.50/32,198.51.100.7/32,198.51.100.9/32" {
		t.Errorf("updates = %v, want one update removing only 203.0.113.5", waf.updates)
	}
	if strings.Join(res.Deleted, ",") != "203.0.113.5/32" || strings.Join(res.Gone, ",") != "203.0.113.6/32" ||
		strings.Join(res.Refreshed, ",") != "198.51.100.9/32" {
		t.Errorf("result = %+v", res)
	}
	// The gone entry's old record goes too; the refreshed one stays.
	if got := strings.Join(sortedCopy(ssmf.deleted), ","); got != "203.0.113.5/32,203.0.113.6/32" || res.TimestampsRemoved != 2 {
		t.Errorf("records deleted = %s (count %d)", got, res.TimestampsRemoved)
	}
	if ssmf.params[aclTimestampPath("198.51.100.9/32")] == "" || ssmf.params[aclTimestampPath("192.0.2.50/32")] == "" {
		t.Errorf("the records of entries confirmed in the meantime must stay: %v", ssmf.params)
	}
}

func TestPruneACL_NothingOldChangesNothing(t *testing.T) {
	t.Parallel()
	for name, stamps := range map[string]map[string]string{
		"empty":      {},
		"recent":     {"198.51.100.7/32": "2026-11-01T00:00:00Z", "10.0.0.0/8": ""},
		"only mine":  {"153.242.166.137/32": "2026-08-01T00:00:00Z", "198.51.100.7/32": "2026-11-01T00:00:00Z"},
		"unrecorded": {"10.0.0.0/8": "", "192.0.2.0/24": ""},
	} {
		svc, waf, ssmf, ip := pruneService(stamps)
		p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
		if err != nil {
			t.Fatalf("%s: PlanACLPrune: %v", name, err)
		}
		if len(p.Delete) != 0 {
			t.Errorf("%s: delete = %v", name, p.Delete)
		}
		if wantCalls := len(p.Mine); ip.calls != wantCalls {
			t.Errorf("%s: %d detections; detect only when an entry is old", name, ip.calls)
		}
		// Someone adds an entry before the answer: nothing is updated,
		// and Remaining is the IP set as it is now.
		waf.addrs = append(waf.addrs, "192.0.2.99/32")
		want := strings.Join(sortedCopy(waf.addrs), ",")
		res, err := svc.PruneACL(context.Background(), p)
		if err != nil {
			t.Fatalf("%s: PruneACL: %v", name, err)
		}
		if len(waf.updates) != 0 || len(ssmf.deleted) != 0 || res.Deleted == nil || len(res.Deleted) != 0 {
			t.Errorf("%s: nothing may change: updates %v, records %v, result %+v", name, waf.updates, ssmf.deleted, res)
		}
		if strings.Join(res.Remaining, ",") != want {
			t.Errorf("%s: remaining = %v, want %s", name, res.Remaining, want)
		}
	}
}

func TestPruneACL_EveryEntryOld(t *testing.T) {
	t.Parallel()
	svc, waf, ssmf, _ := pruneService(map[string]string{
		"203.0.113.5/32": "2026-09-01T00:00:00Z",
		"192.0.2.0/24":   "2026-01-01T00:00:00Z",
	})
	p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
	if err != nil {
		t.Fatalf("PlanACLPrune: %v", err)
	}
	res, err := svc.PruneACL(context.Background(), p)
	if err != nil {
		t.Fatalf("PruneACL: %v", err)
	}
	if len(waf.updates) != 1 || waf.updates[0] == nil || len(waf.updates[0]) != 0 {
		t.Errorf("updates = %#v, want a single update to an empty list", waf.updates)
	}
	if len(res.Deleted) != 2 || res.Remaining == nil || len(res.Remaining) != 0 || len(ssmf.params) != 0 {
		t.Errorf("result = %+v, records left %v", res, ssmf.params)
	}
}

func TestPlanACLPrune_FailsClosed(t *testing.T) {
	t.Parallel()
	old := map[string]string{"203.0.113.5/32": "2026-09-01T00:00:00Z"}
	for cause, ip := range map[string]*fakeIP{
		"timeout":          {err: errors.New("dial tcp: timeout")},
		"not an IPv4":      {ip: "2001:db8::1"},
		"no address found": {ips: []string{}},
	} {
		svc, waf, ssmf, _ := pruneService(old)
		svc.clients.IPDiscoverer = ip
		if _, err := svc.PlanACLPrune(context.Background(), pruneCutoff); err == nil ||
			!strings.Contains(err.Error(), "detect your public IP") || !strings.Contains(err.Error(), cause) {
			t.Errorf("error = %v, want a detection error saying %q", err, cause)
		}
		if len(waf.updates) != 0 || len(ssmf.deleted) != 0 {
			t.Errorf("nothing may change")
		}
	}

	// Without the records every entry would look added outside nakama.
	svc, waf, ssmf, _ := pruneService(old)
	ssmf.pathErr = errors.New("AccessDenied")
	if _, err := svc.PlanACLPrune(context.Background(), pruneCutoff); err == nil || !strings.Contains(err.Error(), "added-at records") {
		t.Errorf("plan error = %v, want the records' read error", err)
	}
	ssmf.pathErr = nil
	p, err := svc.PlanACLPrune(context.Background(), pruneCutoff)
	if err != nil {
		t.Fatalf("PlanACLPrune: %v", err)
	}
	ssmf.pathErr = errors.New("AccessDenied")
	if _, err := svc.PruneACL(context.Background(), p); err == nil || len(waf.updates) != 0 {
		t.Errorf("PruneACL error = %v, updates %v; want an error before any change", err, waf.updates)
	}
}

func TestACLEntry_AddedTime(t *testing.T) {
	t.Parallel()
	if got, ok := (ACLEntry{AddedAt: "2026-09-01T00:00:00Z"}).AddedTime(); !ok || !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("AddedTime = %v, %v", got, ok)
	}
	for _, s := range []string{"", "not-a-time", "2026-09-01"} {
		if _, ok := (ACLEntry{AddedAt: s}).AddedTime(); ok {
			t.Errorf("AddedTime(%q) ok, want not", s)
		}
	}
}
