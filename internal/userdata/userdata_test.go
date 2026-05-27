package userdata

import (
	"strings"
	"testing"
)

func TestRenderCodeServer_Substitutes(t *testing.T) {
	t.Parallel()

	got, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev01",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev01/code-server/password",
		SSMVersionParameter:  "/nakama/dev01/code-server/version",
		PinnedVersion:        "",
	})
	if err != nil {
		t.Fatalf("RenderCodeServer returned unexpected error: %v", err)
	}

	wantSubstrings := []string{
		`INSTANCE_NAME="dev01"`,
		`REGION="ap-northeast-1"`,
		`PORT="8080"`,
		`PASSWORD_PARAM="/nakama/dev01/code-server/password"`,
		`VERSION_PARAM="/nakama/dev01/code-server/version"`,
		`PINNED_VERSION=""`,
		`bind-addr: 0.0.0.0:`,
		`auth: password`,
		`code-server@`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("rendered userdata is missing %q", s)
		}
	}
}

func TestRenderCodeServer_PinnedVersion(t *testing.T) {
	t.Parallel()

	got, err := RenderCodeServer(CodeServerParams{
		InstanceName:         "dev02",
		Region:               "ap-northeast-1",
		Port:                 8080,
		SSMPasswordParameter: "/nakama/dev02/code-server/password",
		SSMVersionParameter:  "/nakama/dev02/code-server/version",
		PinnedVersion:        "4.95.3",
	})
	if err != nil {
		t.Fatalf("RenderCodeServer returned unexpected error: %v", err)
	}
	if !strings.Contains(got, `PINNED_VERSION="4.95.3"`) {
		t.Errorf("rendered userdata does not contain pinned version 4.95.3")
	}
}
