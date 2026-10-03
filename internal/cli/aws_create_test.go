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
		"--name", "dev01", "--instance-type", "t3.medium",
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
		"--name", "dev01", "--instance-type", "t3.medium",
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
