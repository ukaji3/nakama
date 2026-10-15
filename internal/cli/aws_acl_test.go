package cli

import (
	"strings"
	"testing"

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
		Delete: entries, KeepMine: true, MyIP: "153.242.166.137",
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
	kept := aclDeleteAllPrompt(&service.ACLDeleteAllPlan{Delete: entries[:1], Keep: entries[1:], MyIP: "10.0.0.1"})
	if !strings.Contains(kept, "Delete 1 of 2 entries") {
		t.Errorf("one-of-two prompt:\n%s", kept)
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
			plan: &service.ACLDeleteAllPlan{Delete: del, Keep: []service.ACLEntry{{CIDR: "153.242.166.137/32"}}, MyIP: "153.242.166.137"},
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
			plan: &service.ACLDeleteAllPlan{Keep: []service.ACLEntry{{CIDR: "153.242.166.137/32"}}, MyIP: "153.242.166.137"},
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
