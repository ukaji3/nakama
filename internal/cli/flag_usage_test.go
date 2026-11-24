package cli

import (
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
)

// flagHelpLine matches the start of a flag's line in `--help`:
// "  -y, --yes   Skip ..." or "      --label string   Backup ...".
var flagHelpLine = regexp.MustCompile(`(?m)^\s+(?:-[^,\s], )?--([0-9A-Za-z][0-9A-Za-z-]*)(.*)$`)

// TestFlagUsagesHaveNoBackQuotes walks every command and checks how
// `--help` renders each flag. pflag takes the first back-quoted word of
// a usage string as the name of the flag's value: "used by
// `open --internet`" turned `--cloudfront` into "--cloudfront open
// --internet" in the v0.28.0 help (Issue #122). So no usage may contain
// a back quote, and a boolean flag's line has no value name: its name is
// followed by the padding before the description.
func TestFlagUsagesHaveNoBackQuotes(t *testing.T) {
	t.Parallel()
	checked := 0
	var visit func(cmd *cobra.Command)
	visit = func(cmd *cobra.Command) {
		for _, fs := range []interface {
			FlagUsages() string
		}{cmd.LocalNonPersistentFlags(), cmd.PersistentFlags()} {
			for _, m := range flagHelpLine.FindAllStringSubmatch(fs.FlagUsages(), -1) {
				name, rest := m[1], m[2]
				f := cmd.Flags().Lookup(name)
				if f == nil {
					f = cmd.PersistentFlags().Lookup(name)
				}
				if f == nil {
					t.Errorf("%s: help names --%s, which the command does not have", cmd.CommandPath(), name)
					continue
				}
				checked++
				if strings.Contains(f.Usage, "`") {
					t.Errorf("%s --%s: remove the back quotes from %q", cmd.CommandPath(), name, f.Usage)
				}
				if f.Value.Type() == "bool" && !strings.HasPrefix(rest, "  ") && rest != "" {
					t.Errorf("%s --%s: the help line shows a value name for a boolean flag: %q",
						cmd.CommandPath(), name, strings.TrimSpace(m[0]))
				}
			}
		}
		for _, sub := range cmd.Commands() {
			visit(sub)
		}
	}
	visit(NewRootCommand())
	if checked < 50 {
		t.Fatalf("checked only %d flags; the command tree walk is broken", checked)
	}
}

// TestCloudFrontFlagHelp pins the help lines that v0.28.0 rendered
// with a value name (Issue #122).
func TestCloudFrontFlagHelp(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		cmd  *cobra.Command
		flag string
		want string
	}{
		{newAWSCreateCommand(), "cloudfront", "used by code --internet"},
		{newAWSRestoreCommand(), "cloudfront", "as create --cloudfront does"},
		{newAWSCodeCommand(), "internet", "added with nakama aws cloudfront add"},
		{newAWSKirocrewCommand(), "internet", "added with nakama aws cloudfront add --kirocrew"},
	} {
		var line string
		for _, l := range strings.Split(c.cmd.Flags().FlagUsages(), "\n") {
			if strings.Contains(l, "--"+c.flag+" ") {
				line = l
			}
		}
		// pflag pads a boolean flag's name with at least two spaces before
		// the description; a value name would follow after one space.
		if !strings.Contains(line, "--"+c.flag+"  ") || !strings.Contains(line, c.want) {
			t.Errorf("%s --%s: help line %q, want no value name and %q", c.cmd.Name(), c.flag, line, c.want)
		}
	}
}

// lowercaseUsageWords may start a flag's description in lowercase: the
// names of programs, which are written that way.
var lowercaseUsageWords = map[string]bool{"sshd": true}

// TestFlagUsagesStartWithACapital: every flag description in the
// public help starts like a sentence (`list --show-nat` did not, Issue
// #129). Hidden commands (the daemons) are internal and not checked;
// hidden flags do not appear in FlagUsages. Like the test above, it
// reads flags through cobra only, so pflag stays an indirect module.
// Not parallel: NewRootCommand writes the global flag variables, which
// the other walk (parallel) writes too.
func TestFlagUsagesStartWithACapital(t *testing.T) {
	checked := 0
	var visit func(cmd *cobra.Command)
	visit = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		for _, m := range flagHelpLine.FindAllStringSubmatch(cmd.LocalNonPersistentFlags().FlagUsages(), -1) {
			f := cmd.Flags().Lookup(m[1])
			if f == nil || f.Usage == "" {
				continue
			}
			checked++
			first := strings.Fields(f.Usage)[0]
			if r := []rune(first)[0]; unicode.IsLower(r) && !lowercaseUsageWords[first] {
				t.Errorf("%s --%s: the description starts in lowercase: %q", cmd.CommandPath(), f.Name, f.Usage)
			}
		}
		for _, sub := range cmd.Commands() {
			visit(sub)
		}
	}
	visit(NewRootCommand())
	if checked < 50 {
		t.Fatalf("checked only %d flags; the command tree walk is broken", checked)
	}
}
