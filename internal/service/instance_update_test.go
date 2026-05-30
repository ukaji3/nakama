package service

import (
	"strings"
	"testing"
)

func TestRebootRequired(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"explicit yes", "foo\nNAKAMA_REBOOT_REQUIRED=1\n", true},
		{"explicit no", "foo\nNAKAMA_REBOOT_REQUIRED=0\n", false},
		{"absent", "some other output\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rebootRequired(c.in); got != c.want {
				t.Fatalf("rebootRequired(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestBuildUpdateScript(t *testing.T) {
	script := buildUpdateScript("ap-northeast-1", "/nakama/dev01/code-server/version")
	musts := []string{
		"apt-get update",
		"apt-get dist-upgrade -y",
		"--force-confdef",
		"--force-confold",
		"DEBIAN_FRONTEND=noninteractive",
		`export HOME="${HOME:-/root}"`,
		"https://code-server.dev/install.sh",
		"systemctl restart code-server@ubuntu",
		"/nakama/dev01/code-server/version",
		"ap-northeast-1",
		"NAKAMA_REBOOT_REQUIRED=1",
		"NAKAMA_REBOOT_REQUIRED=0",
		"/var/run/reboot-required",
	}
	for _, m := range musts {
		if !strings.Contains(script, m) {
			t.Errorf("update script missing %q", m)
		}
	}
}
