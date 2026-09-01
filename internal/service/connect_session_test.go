package service

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	smpsession "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"

	"github.com/ukaji3/nakama/internal/awsx"
)

func TestInstallPluginResumeConfigInjectsNakamaConfig(t *testing.T) {
	t.Cleanup(func() { smpsession.NakamaAWSConfigProvider = nil })
	installPluginResumeConfig(&awsx.Clients{Config: aws.Config{Region: "ap-northeast-1"}})
	if smpsession.NakamaAWSConfigProvider == nil {
		t.Fatal("NakamaAWSConfigProvider must be installed for the session-runner child")
	}
	got, err := smpsession.NakamaAWSConfigProvider(context.Background())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got.Region != "ap-northeast-1" {
		t.Errorf("provider config region = %q, want the injected nakama config", got.Region)
	}
}
