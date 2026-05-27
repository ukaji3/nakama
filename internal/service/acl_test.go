package service

import (
	"strings"
	"testing"
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
