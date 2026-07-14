package cli

import (
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
