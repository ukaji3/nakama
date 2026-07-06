package naming

import (
	"strings"
	"testing"
)

func TestNamesHaveNakamaPrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"VPC":               VPC(),
		"IGW":               IGW(),
		"NATGW":             NATGW(),
		"NATGWEIP":          NATGWEIP(),
		"PublicSubnet":      PublicSubnet("1a"),
		"PrivateSubnet":     PrivateSubnet("1c"),
		"PublicRouteTable":  PublicRouteTable(),
		"PrivateRouteTable": PrivateRouteTable(),
		"WAFWebACL":         WAFWebACL(),
		"WAFIPSet":          WAFIPSet(),
		"EC2":               EC2("dev01"),
		"SecurityGroup":     SecurityGroup("dev01"),
		"ENI":               ENI("dev01"),
		"InstanceProfile":   InstanceProfile(),
		"InstanceRole":      InstanceRole(),
		"NATInstance":       NATInstance(),
		"NATInstanceSG":     NATInstanceSG(),
	}
	for k, v := range cases {
		if !strings.HasPrefix(v, "nakama") {
			t.Errorf("%s = %q, must start with 'nakama'", k, v)
		}
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	if got := Snapshot("dev01", "before-x"); got != "nakama-backup-dev01-before-x" {
		t.Errorf("Snapshot(...)=%q", got)
	}
}

func TestSSMPath(t *testing.T) {
	t.Parallel()
	got := SSMPath("dev01", "code-server", "password")
	want := "/nakama/dev01/code-server/password"
	if got != want {
		t.Errorf("SSMPath(...)=%q, want %q", got, want)
	}
}

func TestSSMMetaPaths(t *testing.T) {
	t.Parallel()
	if got := SSMMetaInitialized(); got != "/nakama/_meta/initialized" {
		t.Errorf("SSMMetaInitialized()=%q", got)
	}
	if got := SSMMetaVersion(); got != "/nakama/_meta/version" {
		t.Errorf("SSMMetaVersion()=%q", got)
	}
}
