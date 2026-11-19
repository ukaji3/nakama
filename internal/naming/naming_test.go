package naming

import (
	"strings"
	"testing"
)

func TestNamesHaveNakamaPrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"VPC":                   VPC(),
		"IGW":                   IGW(),
		"NATGW":                 NATGW(),
		"NATGWEIP":              NATGWEIP(),
		"PublicSubnet":          PublicSubnet("1a"),
		"PrivateSubnet":         PrivateSubnet("1c"),
		"PublicRouteTable":      PublicRouteTable(),
		"PrivateRouteTable":     PrivateRouteTable(),
		"WAFWebACL":             WAFWebACL(),
		"WAFIPSet":              WAFIPSet(),
		"EC2":                   EC2("dev01"),
		"SecurityGroup":         SecurityGroup("dev01"),
		"ENI":                   ENI("dev01"),
		"InstanceProfile":       InstanceProfile(),
		"InstanceRole":          InstanceRole(),
		"AdminInstanceRole":     AdminInstanceRole(),
		"AdminInstanceProfile":  AdminInstanceProfile(),
		"CustomInstanceProfile": CustomInstanceProfile("my-role"),
		"NATInstance":           NATInstance(),
		"NATInstanceSG":         NATInstanceSG(),
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

func TestInstanceRoleAndProfileNames(t *testing.T) {
	t.Parallel()
	if got := AdminInstanceRole(); got != "nakama-instance-role-admin" {
		t.Errorf("AdminInstanceRole()=%q", got)
	}
	if got := AdminInstanceProfile(); got != "nakama-instance-profile-admin" {
		t.Errorf("AdminInstanceProfile()=%q", got)
	}
	if got := CustomInstanceProfile("my-role"); got != "nakama-instance-profile-my-role" {
		t.Errorf("CustomInstanceProfile(...)=%q", got)
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
	if got := SSMMetaNATMode(); got != "/nakama/_meta/nat-mode" {
		t.Errorf("SSMMetaNATMode()=%q", got)
	}
}

// TestNATGatewayNamesStay pins the NAT gateway and EIP names: nakama
// versions before ADR 0026 delete a NAT gateway of exactly this name on
// init, which keeps an environment consistent when an old binary runs
// init on an environment switched to the NAT gateway (ADR 0026).
func TestNATGatewayNamesStay(t *testing.T) {
	t.Parallel()
	if got := NATGW(); got != "nakama-natgw" {
		t.Errorf("NATGW()=%q, want nakama-natgw", got)
	}
	if got := NATGWEIP(); got != "nakama-natgw-eip" {
		t.Errorf("NATGWEIP()=%q, want nakama-natgw-eip", got)
	}
}
