package cli

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

func TestRestorePrompt(t *testing.T) {
	t.Parallel()
	b := service.Backup{Label: "before-x", CreatedAt: "2026-10-01T00:00:00Z"}

	inPlace := restorePrompt(&service.RestorePlan{Name: "dev01", Mode: service.RestoreModeInPlace, Backup: b})
	for _, want := range []string{`"dev01" in place`, `"before-x"`, "deletes the current root volume", "[y/N]: "} {
		if !strings.Contains(inPlace, want) {
			t.Errorf("in-place prompt lacks %q:\n%s", want, inPlace)
		}
	}

	fresh := restorePrompt(&service.RestorePlan{
		Name: "dev01", Mode: service.RestoreModeNewInstance, Backup: b,
		InstanceType: "t4g.medium", Architecture: "arm64", WithRole: "my-role",
	})
	for _, want := range []string{
		"no longer exists", "as a new instance", `"before-x"`, "t4g.medium (arm64)", "code-server",
		"my-role", "code-server password is new", "CloudFront    : no", "nakama aws cloudfront add", "left stopped", "[y/N]: ",
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("new-instance prompt lacks %q:\n%s", want, fresh)
		}
	}
	if strings.Contains(fresh, "deletes the current root volume") || strings.Contains(fresh, "15-20 minutes") {
		t.Errorf("nothing is deleted when the instance is gone, and nothing waits on CloudFront:\n%s", fresh)
	}

	withCF := restorePrompt(&service.RestorePlan{
		Name: "dev01", Mode: service.RestoreModeNewInstance, Backup: b,
		InstanceType: "t4g.medium", Architecture: "arm64", CloudFront: true,
	})
	for _, want := range []string{"CloudFront    : yes", "new URL", "15-20 minutes"} {
		if !strings.Contains(withCF, want) {
			t.Errorf("--cloudfront prompt lacks %q:\n%s", want, withCF)
		}
	}

	bare := restorePrompt(&service.RestorePlan{
		Name: "box1", Mode: service.RestoreModeNewInstance, Backup: b,
		InstanceType: "t4g.medium", Architecture: "arm64", Bare: true,
	})
	if !strings.Contains(bare, "bare") || strings.Contains(bare, "password") || !strings.Contains(bare, "IAM role      : none") {
		t.Errorf("bare prompt:\n%s", bare)
	}
}

func TestRestoreSummary(t *testing.T) {
	t.Parallel()
	in := restoreSummary(&service.RestoreResult{Name: "dev01", Mode: service.RestoreModeInPlace, Label: "l1"})
	if !strings.Contains(in, "in place") || !strings.Contains(in, `nakama aws start dev01`) {
		t.Errorf("in-place summary: %s", in)
	}
	fresh := restoreSummary(&service.RestoreResult{
		Name: "dev01", Mode: service.RestoreModeNewInstance, Label: "l1", InstanceID: "i-1",
		InstanceType: "t4g.medium", URL: "https://d.cloudfront.net", CloudFront: true,
	})
	for _, want := range []string{"as a new instance", "i-1", "https://d.cloudfront.net", "info dev01 --show-password", "nakama aws start dev01"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("new-instance summary lacks %q:\n%s", want, fresh)
		}
	}
	noCF := restoreSummary(&service.RestoreResult{
		Name: "dev01", Mode: service.RestoreModeNewInstance, Label: "l1", InstanceID: "i-1", InstanceType: "t4g.medium",
	})
	for _, want := range []string{"cloudfront        : none", "nakama aws cloudfront add dev01", "nakama aws code dev01", "info dev01 --show-password"} {
		if !strings.Contains(noCF, want) {
			t.Errorf("summary without CloudFront lacks %q:\n%s", want, noCF)
		}
	}
	if strings.Contains(noCF, "url ") {
		t.Errorf("a summary without CloudFront has no URL:\n%s", noCF)
	}
	bare := restoreSummary(&service.RestoreResult{Name: "box1", Mode: service.RestoreModeNewInstance, Bare: true})
	if strings.Contains(bare, "url") || !strings.Contains(bare, "nakama aws shell box1") {
		t.Errorf("bare summary: %s", bare)
	}
}

func TestRestoreCommand_Flags(t *testing.T) {
	t.Parallel()
	cmd := newAWSRestoreCommand()
	for _, name := range []string{"label", "yes", "instance-type", "bare", "with-admin", "with-role", "cloudfront"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
	cmd.SetArgs([]string{"dev01", "--with-admin", "--with-role", "r"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "with-admin") {
		t.Fatalf("expected the mutual-exclusion error, got %v", err)
	}
	cmd = newAWSRestoreCommand()
	cmd.SetArgs([]string{"dev01", "--bare", "--cloudfront"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cloudfront") {
		t.Fatalf("expected the --bare/--cloudfront mutual-exclusion error, got %v", err)
	}
}
