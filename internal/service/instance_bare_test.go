package service

import (
	"strings"
	"testing"
)

func TestInstanceMetaEntriesBareOmitsBrowserPath(t *testing.T) {
	bare := instanceMetaEntries("dev01", instanceMetaValues{
		InstanceID:      "i-1",
		ENIID:           "eni-1",
		SecurityGroupID: "sg-1",
		Bare:            true,
	})
	// SSM rejects empty parameter values, so the browser-path keys must
	// be absent rather than empty.
	for _, absent := range []string{
		ssmVPCOriginIDPath("dev01"),
		ssmDistributionIDPath("dev01"),
		ssmDistributionDomainPath("dev01"),
	} {
		if _, ok := bare[absent]; ok {
			t.Errorf("bare metadata must not contain %s", absent)
		}
	}
	if bare[ssmKindPath("dev01")] != instanceKindBare {
		t.Errorf("kind = %q, want %q", bare[ssmKindPath("dev01")], instanceKindBare)
	}
	for _, want := range []string{ssmInstanceIDPath("dev01"), ssmENIIDPath("dev01"), ssmSGIDPath("dev01")} {
		if bare[want] == "" {
			t.Errorf("bare metadata must still contain %s", want)
		}
	}
}

func TestInstanceMetaEntriesRegularKeepsBrowserPathAndNoKind(t *testing.T) {
	regular := instanceMetaEntries("dev01", instanceMetaValues{
		InstanceID:         "i-1",
		ENIID:              "eni-1",
		SecurityGroupID:    "sg-1",
		VPCOriginID:        "vo-1",
		DistributionID:     "d-1",
		DistributionDomain: "d.cloudfront.net",
	})
	if regular[ssmDistributionDomainPath("dev01")] != "d.cloudfront.net" {
		t.Error("a regular instance must record its distribution domain")
	}
	// Absent kind means "vscode", so instances created before --bare
	// existed keep their meaning without a migration.
	if _, ok := regular[ssmKindPath("dev01")]; ok {
		t.Error("a regular instance must not write the kind parameter")
	}
}

func TestBuildBareUpdateScriptHasNoCodeServer(t *testing.T) {
	script := buildBareUpdateScript()
	if strings.Contains(script, "code-server") {
		t.Errorf("the bare update script must not touch code-server:\n%s", script)
	}
	for _, want := range []string{"dist-upgrade", "NAKAMA_REBOOT_REQUIRED=1", "NAKAMA_REBOOT_REQUIRED=0"} {
		if !strings.Contains(script, want) {
			t.Errorf("the bare update script lacks %q:\n%s", want, script)
		}
	}
	// The caller's reboot detection is shared with the regular path.
	if !rebootRequired("NAKAMA_REBOOT_REQUIRED=1") {
		t.Error("the shared reboot marker must be recognized")
	}
}
