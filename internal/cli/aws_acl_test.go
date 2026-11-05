package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

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

func TestParseACLAge(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"30d": 30 * day, "1d": day, " 7d ": 7 * day, "36h": 36 * time.Hour, "90m": 90 * time.Minute, "1h30m": 90 * time.Minute,
	} {
		if got, err := parseACLAge(in); err != nil || got != want {
			t.Errorf("parseACLAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "d", "0d", "-1d", "1.5d", "30", "30D", "30 d", "abc", "0s", "-5h", "1d12h", "99999999999d",
	} {
		if got, err := parseACLAge(in); err == nil || !strings.Contains(err.Error(), "--older-than") {
			t.Errorf("parseACLAge(%q) = %v, %v; want an --older-than error", in, got, err)
		}
	}
}

func TestFormatACLTable_Age(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	got := formatACLTable([]service.ACLEntry{
		{CIDR: "203.0.113.5/32", AddedAt: "2026-09-01T00:00:00Z"},
		{CIDR: "198.51.100.7/32", AddedAt: "2026-11-06T00:00:00Z"},
		{CIDR: "198.51.100.8/32", AddedAt: "2026-11-07T00:00:00Z"}, // ahead of this clock
		{CIDR: "10.0.0.0/8"},
	}, now)
	want := "CIDR                  ADDED_AT              AGE\n" +
		"203.0.113.5/32        2026-09-01T00:00:00Z  66d\n" +
		"198.51.100.7/32       2026-11-06T00:00:00Z  0d\n" +
		"198.51.100.8/32       2026-11-07T00:00:00Z  0d\n" +
		"10.0.0.0/8                                  -"
	if got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
	if got := formatACLTable(nil, now); got != "(allowlist is empty)" {
		t.Errorf("empty: %q", got)
	}
}

func TestACLPrunePrompt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	p := &service.ACLPrunePlan{
		Cutoff:     now.Add(-30 * 24 * time.Hour),
		Delete:     []service.ACLEntry{{CIDR: "203.0.113.5/32", AddedAt: "2026-09-01T00:00:00Z"}},
		Mine:       []service.ACLEntry{{CIDR: "153.242.0.0/16", AddedAt: "2026-08-01T00:00:00Z"}},
		Keep:       []service.ACLEntry{{CIDR: "198.51.100.7/32", AddedAt: "2026-11-01T00:00:00Z"}},
		Unrecorded: []service.ACLEntry{{CIDR: "10.0.0.0/8"}},
		MyIPs:      []string{"153.242.166.137"},
	}
	got := aclPrunePrompt(p, "30d", now)
	for _, want := range []string{
		"Delete 1 of 4 entries", "more than 30d ago (before 2026-10-07T00:00:00Z)",
		"203.0.113.5/32        added 2026-09-01T00:00:00Z, 66d ago",
		"Keep, though old (contains your IP 153.242.166.137):", "153.242.0.0/16",
		"Keep (confirmed since then):", "198.51.100.7/32", "5d ago",
		"Keep (no added-at record):", "10.0.0.0/8", "(added outside nakama)",
		"403", "nakama aws acl add", "connect, ssh and tunnel are not affected. [y/N]: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prune prompt lacks %q:\n%s", want, got)
		}
	}
	only := aclPrunePrompt(&service.ACLPrunePlan{Cutoff: p.Cutoff, Delete: p.Delete}, "30d", now)
	if strings.Contains(only, "Keep") {
		t.Errorf("no kept entry, no Keep section:\n%s", only)
	}
}

func TestACLPruneSummary(t *testing.T) {
	t.Parallel()
	old := []service.ACLEntry{{CIDR: "203.0.113.5/32"}, {CIDR: "203.0.113.6/32"}}
	cases := []struct {
		name string
		plan *service.ACLPrunePlan
		res  *service.ACLPruneResult
		want []string
	}{
		{
			name: "empty",
			plan: &service.ACLPrunePlan{},
			res:  &service.ACLPruneResult{},
			want: []string{"nothing to prune: the allowlist is empty"},
		},
		{
			name: "nothing old",
			plan: &service.ACLPrunePlan{Keep: old},
			res:  &service.ACLPruneResult{},
			want: []string{`nothing to prune: no entry was last confirmed by "acl add" more than 30d ago`},
		},
		{
			name: "only mine",
			plan: &service.ACLPrunePlan{Mine: old, MyIPs: []string{"203.0.113.5"}},
			res:  &service.ACLPruneResult{},
			want: []string{"nothing to prune: every old entry contains your IP 203.0.113.5"},
		},
		{
			name: "deleted",
			plan: &service.ACLPrunePlan{Delete: old},
			res: &service.ACLPruneResult{Deleted: []string{"203.0.113.5/32"}, Remaining: []string{"10.0.0.0/8"},
				Refreshed: []string{"203.0.113.6/32"}},
			want: []string{"deleted: 203.0.113.5/32", "deleted 1 entry; remaining: 10.0.0.0/8",
				"kept, confirmed again in the meantime: 203.0.113.6/32"},
		},
		{
			name: "emptied",
			plan: &service.ACLPrunePlan{Delete: old},
			res:  &service.ACLPruneResult{Deleted: []string{"203.0.113.5/32", "203.0.113.6/32"}, Remaining: []string{}},
			want: []string{"deleted 2 entries; the allowlist is now empty", "nakama aws acl add"},
		},
		{
			name: "all changed",
			plan: &service.ACLPrunePlan{Delete: old},
			res:  &service.ACLPruneResult{Deleted: []string{}, Gone: []string{"203.0.113.5/32", "203.0.113.6/32"}},
			want: []string{"nothing deleted", "already removed by someone else: 203.0.113.5/32, 203.0.113.6/32"},
		},
	}
	for _, c := range cases {
		got := aclPruneSummary(c.plan, c.res, "30d")
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: summary lacks %q:\n%s", c.name, w, got)
			}
		}
	}
}

func TestACLPruneCommand(t *testing.T) {
	t.Parallel()
	cmd := newAWSACLPruneCommand()
	for _, name := range []string{"older-than", "yes"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
	// The time is only refreshed by acl add, and the help says so.
	for _, want := range []string{`Only "acl add" records`, "nakama aws open", "does not", "30d", "36h", "--yes", "added outside nakama"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("acl prune help lacks %q:\n%s", want, cmd.Long)
		}
	}
	found := false
	for _, c := range newAWSACLCommand().Commands() {
		found = found || c.Name() == "prune"
	}
	if !found {
		t.Error("acl has no prune subcommand")
	}
	// Argument errors come before any AWS call.
	for _, args := range [][]string{{}, {"--older-than", "abc"}, {"--older-than", "0d"}, {"--older-than", "30d", "203.0.113.5"}} {
		c := newAWSACLPruneCommand()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("args %v: expected an error", args)
		}
	}
}
