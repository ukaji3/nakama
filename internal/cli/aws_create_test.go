package cli

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

// Flag mutual exclusion (--with-admin vs --with-role) is enforced by
// cobra's MarkFlagsMutuallyExclusive; these tests only cover the
// flag-to-service-input mapping.
func TestCreateInputFromFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   createFlags
		want service.CreateInstanceInput
	}{
		{
			name: "base flags only",
			in:   createFlags{name: "dev01", instanceType: "t3.medium", codeServerVersion: "4.90.0"},
			want: service.CreateInstanceInput{Name: "dev01", InstanceType: "t3.medium", CodeServerVersion: "4.90.0"},
		},
		{
			name: "with-admin",
			in:   createFlags{name: "dev01", instanceType: "t3.medium", withAdmin: true},
			want: service.CreateInstanceInput{Name: "dev01", InstanceType: "t3.medium", WithAdmin: true},
		},
		{
			name: "with-role",
			in:   createFlags{name: "dev01", instanceType: "t3.medium", withRole: "my-role"},
			want: service.CreateInstanceInput{Name: "dev01", InstanceType: "t3.medium", WithRole: "my-role"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := createInputFromFlags(&c.in); got != c.want {
				t.Errorf("createInputFromFlags(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestCreateCommand_WithAdminAndWithRoleMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cmd := newAWSCreateCommand()
	cmd.SetArgs([]string{
		"dev01", "--instance-type", "t3.medium",
		"--with-admin", "--with-role", "my-role",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cobra mutual-exclusion error, got nil")
	}
}

func TestCreateCommand_BareAndCodeServerVersionMutuallyExclusive(t *testing.T) {
	// A bare instance has no code-server, so a pinned version would be
	// silently meaningless (Issue #61).
	t.Parallel()
	cmd := newAWSCreateCommand()
	cmd.SetArgs([]string{
		"dev01", "--instance-type", "t3.medium",
		"--bare", "--code-server-version", "4.96.4",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cobra mutual-exclusion error for --bare with --code-server-version, got nil")
	}
}

// TestCreateInputFromFlagsNoKiro pins the --no-kiro wiring: Kiro is
// installed by default, so the flag has to reach the service layer
// (Issue #68).
func TestCreateInputFromFlagsNoKiro(t *testing.T) {
	t.Parallel()
	in := createFlags{name: "dev01", instanceType: "t3.medium", noKiro: true}
	if got := createInputFromFlags(&in); !got.NoKiro {
		t.Error("--no-kiro must set CreateInstanceInput.NoKiro")
	}
	base := createFlags{name: "dev01", instanceType: "t3.medium"}
	if got := createInputFromFlags(&base); got.NoKiro {
		t.Error("NoKiro must default to false so Kiro is installed by default")
	}
}

// TestCreateCommandHasNoKiroFlag pins that the flag is registered and
// documented (the help text is how users discover the opt-out).
func TestCreateCommandHasNoKiroFlag(t *testing.T) {
	t.Parallel()
	cmd := newAWSCreateCommand()
	f := cmd.Flags().Lookup("no-kiro")
	if f == nil {
		t.Fatal("create must offer --no-kiro")
	}
	if f.DefValue != "false" {
		t.Errorf("--no-kiro default = %q, want false", f.DefValue)
	}
	if !strings.Contains(strings.ToLower(f.Usage), "kiro") {
		t.Errorf("--no-kiro usage = %q, want it to mention Kiro", f.Usage)
	}
}

// TestResolveCreateName: the name is the argument; the deprecated
// --name still works, alone or with the same value (Issue #129).
func TestResolveCreateName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc     string
		args     []string
		flagName string
		flagSet  bool
		want     string
		wantErr  string
	}{
		{desc: "argument", args: []string{"dev01"}, want: "dev01"},
		{desc: "deprecated flag", flagName: "dev01", flagSet: true, want: "dev01"},
		{desc: "both, same value", args: []string{"dev01"}, flagName: "dev01", flagSet: true, want: "dev01"},
		{desc: "both, different values", args: []string{"dev01"}, flagName: "dev02", flagSet: true,
			wantErr: `given twice, as "dev01" and as --name "dev02"`},
		{desc: "an empty --name is still a value", args: []string{"dev01"}, flagName: "", flagSet: true,
			wantErr: "given twice"},
		{desc: "neither", wantErr: "nakama aws create <name> --instance-type <type>"},
	} {
		got, err := resolveCreateName(tc.args, tc.flagName, tc.flagSet)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err %v, want it to contain %q", tc.desc, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: (%q, %v), want %q", tc.desc, got, err, tc.want)
		}
	}
}

// TestCreateCommandTakesTheNameAsArgument: help shows `create <name>`
// without --name, and --name is deprecated with a pointer to the
// argument. The cases stop before any AWS call.
func TestCreateCommandTakesTheNameAsArgument(t *testing.T) {
	t.Parallel()
	cmd := newAWSCreateCommand()
	if cmd.Use != "create <name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if strings.Contains(cmd.Flags().FlagUsages(), "--name") {
		t.Error("help still lists --name")
	}
	for _, rule := range []string{"2-32 characters", "lowercase letters, digits", "starting and ending with a letter or digit"} {
		if !strings.Contains(cmd.Long, rule) {
			t.Errorf("help does not state the name rule %q", rule)
		}
	}
	for _, tc := range []struct {
		args    []string
		wantErr string
		warns   bool
	}{
		{[]string{"--instance-type", "t3.medium"}, "give the name of the new instance", false},
		{[]string{"dev01", "--name", "dev02", "--instance-type", "t3.medium"}, "given twice", true},
		{[]string{"dev01", "dev02", "--instance-type", "t3.medium"}, "accepts at most 1 arg", false},
	} {
		cmd := newAWSCreateCommand()
		cmd.SilenceUsage = true
		var out, errOut strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(tc.args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.wantErr)
		}
		const warning = "Flag --name has been deprecated, give the name as the argument: nakama aws create <name>"
		if got := strings.Contains(out.String()+errOut.String(), warning); got != tc.warns {
			t.Errorf("%v: deprecation warning shown = %v, want %v (output %q)", tc.args, got, tc.warns, out.String()+errOut.String())
		}
	}
}
