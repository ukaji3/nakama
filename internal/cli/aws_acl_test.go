package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func TestACLDeleteMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args     []string
		keepMine bool
		wantAll  bool
		wantErr  string
	}{
		{args: []string{"203.0.113.5"}},
		{args: []string{"203.0.113.5", "198.51.100.0/24"}},
		{args: []string{"all"}, wantAll: true},
		{args: []string{"all"}, keepMine: true, wantAll: true},
		{args: []string{"all", "203.0.113.5"}, wantErr: "cannot be combined"},
		{args: []string{"203.0.113.5", "all"}, wantErr: "cannot be combined"},
		{args: []string{"203.0.113.5"}, keepMine: true, wantErr: "--keep-mine"},
	}
	for _, c := range cases {
		all, err := aclDeleteMode(c.args, c.keepMine)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("aclDeleteMode(%v, %v) error = %v, want %q", c.args, c.keepMine, err, c.wantErr)
			}
			continue
		}
		if err != nil || all != c.wantAll {
			t.Errorf("aclDeleteMode(%v, %v) = %v, %v; want %v", c.args, c.keepMine, all, err, c.wantAll)
		}
	}
}

func TestACLDeleteAllPrompt(t *testing.T) {
	t.Parallel()
	entries := []service.ACLEntry{
		{CIDR: "203.0.113.5/32", AddedAt: "2026-09-01T00:00:00Z"},
		{CIDR: "10.0.0.0/8"},
	}
	all := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{Delete: entries})
	for _, want := range []string{
		"Delete all 2 entries", "203.0.113.5/32", "added 2026-09-01T00:00:00Z", "(added outside nakama)",
		"403 to every browser", "yours included", "nakama aws acl add", "Kiro Crew",
		"connect, ssh and tunnel are not affected", "[y/N]: ",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("delete-all prompt lacks %q:\n%s", want, all)
		}
	}

	keep := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{
		Delete: entries, KeepMine: true, MyIPs: []string{"153.242.166.137"},
		Keep: []service.ACLEntry{{CIDR: "153.242.166.137/32", AddedAt: "2026-09-03T19:21:22Z"}},
	})
	for _, want := range []string{"Delete 2 of 3 entries", "Keep (contains your IP 153.242.166.137)", "153.242.166.137/32", "[y/N]: "} {
		if !strings.Contains(keep, want) {
			t.Errorf("keep-mine prompt lacks %q:\n%s", want, keep)
		}
	}
	if strings.Contains(keep, "yours included") {
		t.Errorf("with a kept entry the caller is not locked out:\n%s", keep)
	}

	one := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{Delete: entries[:1]})
	if !strings.Contains(one, "Delete the only entry") || strings.Contains(one, "1 entries") {
		t.Errorf("one-entry prompt:\n%s", one)
	}
	kept := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{Delete: entries[:1], Keep: entries[1:], MyIPs: []string{"10.0.0.1"}})
	if !strings.Contains(kept, "Delete 1 of 2 entries") {
		t.Errorf("one-of-two prompt:\n%s", kept)
	}
	exits := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{
		Delete: entries[:1], KeepMine: true, MyIPs: []string{"198.51.100.1", "198.51.100.2"},
		Keep: []service.ACLEntry{{CIDR: "198.51.100.1/32"}, {CIDR: "198.51.100.2/32"}},
	})
	if !strings.Contains(exits, "Keep (contains one of your IPs 198.51.100.1, 198.51.100.2):") {
		t.Errorf("keep-mine prompt with two exits:\n%s", exits)
	}
}

func TestACLAddOutput(t *testing.T) {
	t.Parallel()
	// One entry: the output from before Issue #104.
	data, text := aclAddOutput([]string{"198.51.100.1/32"})
	if m, ok := data.(map[string]string); !ok || m["added"] != "198.51.100.1/32" || text != "added: 198.51.100.1/32" {
		t.Errorf("one entry: %#v, %q", data, text)
	}
	data, text = aclAddOutput([]string{"198.51.100.1/32", "198.51.100.2/32"})
	if m, ok := data.(map[string][]string); !ok || strings.Join(m["added"], ",") != "198.51.100.1/32,198.51.100.2/32" ||
		text != "added: 198.51.100.1/32\nadded: 198.51.100.2/32" {
		t.Errorf("two entries: %#v, %q", data, text)
	}
}

func TestWarnFailedIPChecks(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	warnFailedIPChecks(&b, &awsx.DetectedIPs{IPs: []string{"198.51.100.1"}, Checks: 6}, "consequence")
	warnFailedIPChecks(&b, nil, "consequence")
	if b.Len() != 0 {
		t.Errorf("no failure, no warning: %q", b.String())
	}
	warnFailedIPChecks(&b, &awsx.DetectedIPs{IPs: []string{"198.51.100.1"}, Checks: 6, Failures: []awsx.IPCheckFailure{
		{Site: "ifconfig.io", Err: errors.New("timeout")}, {Site: "ifconfig.io", Err: errors.New("timeout")},
	}}, "one may be missing")
	if got := b.String(); got != "warning: IP check: ifconfig.io: 2 of 3 checks failed: timeout\nwarning: one may be missing\n" {
		t.Errorf("warning = %q", got)
	}
}

func TestYourIPs(t *testing.T) {
	t.Parallel()
	if got := yourIPs([]string{"198.51.100.1"}); got != "your IP 198.51.100.1" {
		t.Errorf("one: %q", got)
	}
	if got := yourIPs([]string{"198.51.100.1", "198.51.100.2"}); got != "one of your IPs 198.51.100.1, 198.51.100.2" {
		t.Errorf("two: %q", got)
	}
}

func TestACLAddCommand_Help(t *testing.T) {
	t.Parallel()
	long := newAWSACLAddCommand().Long
	for _, want := range []string{"https://checkip.amazonaws.com/", "https://ifconfig.io/ip", "three times each", "new connection", "IPv4", "every distinct address"} {
		if !strings.Contains(long, want) {
			t.Errorf("acl add help lacks %q:\n%s", want, long)
		}
	}
}

func TestACLDeleteAllSummary(t *testing.T) {
	t.Parallel()
	del := []service.ACLEntry{{CIDR: "203.0.113.5/32"}, {CIDR: "10.0.0.0/8"}}
	cases := []struct {
		name string
		plan *service.ACLDeleteAllPlan
		res  *service.ACLDeleteAllResult
		want []string
	}{
		{
			name: "emptied",
			plan: &service.ACLDeleteAllPlan{Delete: del},
			res:  &service.ACLDeleteAllResult{Deleted: []string{"10.0.0.0/8", "203.0.113.5/32"}, Remaining: []string{}},
			want: []string{"deleted: 10.0.0.0/8", "deleted: 203.0.113.5/32", "deleted 2 entries; the allowlist is now empty", "nakama aws acl add"},
		},
		{
			name: "kept mine",
			plan: &service.ACLDeleteAllPlan{Delete: del, Keep: []service.ACLEntry{{CIDR: "153.242.166.137/32"}}, MyIPs: []string{"153.242.166.137"}},
			res:  &service.ACLDeleteAllResult{Deleted: []string{"10.0.0.0/8"}, Gone: []string{"203.0.113.5/32"}, Remaining: []string{"153.242.166.137/32"}},
			want: []string{"deleted 1 entry; remaining: 153.242.166.137/32", "already removed by someone else: 203.0.113.5/32"},
		},
		{
			name: "already empty",
			plan: &service.ACLDeleteAllPlan{},
			res:  &service.ACLDeleteAllResult{Deleted: []string{}, Remaining: []string{}, TimestampsRemoved: 2},
			want: []string{"the allowlist is already empty", "removed 2 added-at records"},
		},
		{
			name: "only mine",
			plan: &service.ACLDeleteAllPlan{Keep: []service.ACLEntry{{CIDR: "153.242.166.137/32"}}, MyIPs: []string{"153.242.166.137"}},
			res:  &service.ACLDeleteAllResult{Deleted: []string{}, Remaining: []string{"153.242.166.137/32"}},
			want: []string{"nothing to delete: every entry contains your IP 153.242.166.137"},
		},
	}
	for _, c := range cases {
		got := aclDeleteAllSummary(c.plan, c.res)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: summary lacks %q:\n%s", c.name, w, got)
			}
		}
	}
}

func TestACLDeleteCommand_FlagsAndArgs(t *testing.T) {
	t.Parallel()
	cmd := newAWSACLDeleteCommand()
	for _, name := range []string{"keep-mine", "yes"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
	// Argument errors come before any AWS call.
	for _, args := range [][]string{{}, {"all", "203.0.113.5"}, {"203.0.113.5", "--keep-mine"}} {
		c := newAWSACLDeleteCommand()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("args %v: expected an error", args)
		}
	}
}
