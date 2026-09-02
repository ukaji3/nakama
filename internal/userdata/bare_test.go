package userdata

import (
	"strings"
	"testing"
)

func TestRenderBare(t *testing.T) {
	got, err := RenderBare(BareParams{InstanceName: "dev01", Region: "ap-northeast-1"})
	if err != nil {
		t.Fatalf("RenderBare: %v", err)
	}
	for _, want := range []string{
		`INSTANCE_NAME="dev01"`,
		`REGION="ap-northeast-1"`,
		"amazon-ssm-agent", // connect / tunnel / ssh depend on the agent
		"systemctl enable --now ssh",
		"git curl unzip jq build-essential",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered bare user data lacks %q", want)
		}
	}
	// A bare instance has no code-server and no password parameter.
	for _, absent := range []string{"code-server", "PASSWORD_PARAM", "get-parameter"} {
		if strings.Contains(got, absent) {
			t.Errorf("rendered bare user data must not mention %q", absent)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("template placeholders were left unrendered:\n%s", got)
	}
}
