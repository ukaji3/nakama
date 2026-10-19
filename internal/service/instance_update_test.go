package service

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
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

func TestApplyToolMarkers(t *testing.T) {
	cases := []struct {
		stdout       string
		wantAWS      string
		wantKiroCrew string
	}{
		{"NAKAMA_AWSCLI=2.38.1\nNAKAMA_KIROCREW=updated\nNAKAMA_REBOOT_REQUIRED=0\n", "2.38.1", KiroCrewUpdated},
		{"NAKAMA_KIROCREW=failed\nNAKAMA_REBOOT_REQUIRED=1\n", "", KiroCrewFailed},
		{"NAKAMA_REBOOT_REQUIRED=0\n", "", ""},         // an instance from before Issue #101, no Kiro
		{"NAKAMA_KIROCREW=maybe\n", "", ""},            // unknown values are ignored
		{"  NAKAMA_AWSCLI=2.38.1  \r\n", "2.38.1", ""}, // surrounding whitespace
	}
	for _, c := range cases {
		var res UpdateResult
		applyToolMarkers(&res, c.stdout)
		if res.AWSCLIVersion != c.wantAWS || res.KiroCrew != c.wantKiroCrew {
			t.Errorf("applyToolMarkers(%q) = %q / %q, want %q / %q", c.stdout, res.AWSCLIVersion, res.KiroCrew, c.wantAWS, c.wantKiroCrew)
		}
	}
}

// fakeKindFailSSM knows the instance but fails to read its kind.
type fakeKindFailSSM struct{ fakeParamSSM }

func (f *fakeKindFailSSM) GetParameter(ctx context.Context, in *ssm.GetParameterInput, opts ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if aws.ToString(in.Name) == ssmKindPath("dev01") {
		return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}
	}
	return f.fakeParamSSM.GetParameter(ctx, in, opts...)
}

// fakeUpdateEC2 serves a stopped instance and fails the test on any
// call that changes something.
type fakeUpdateEC2 struct {
	fakeEC2
	t *testing.T
}

func (f *fakeUpdateEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: aws.String("i-1"), InstanceType: "t3.medium",
		State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
	}}}}}, nil
}

func (f *fakeUpdateEC2) StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.t.Error("the instance must not be started when a read fails first")
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeUpdateEC2) CreateSnapshot(context.Context, *ec2.CreateSnapshotInput, ...func(*ec2.Options)) (*ec2.CreateSnapshotOutput, error) {
	f.t.Error("no backup may be taken when a read fails first")
	return &ec2.CreateSnapshotOutput{}, nil
}

// TestUpdate_ReadsTheKindBeforeChangingAnything: a failure to read the
// kind used to come after a stopped instance was started, and returned
// without stopping it again.
func TestUpdate_ReadsTheKindBeforeChangingAnything(t *testing.T) {
	ssmf := &fakeKindFailSSM{fakeParamSSM{params: map[string]string{ssmInstanceIDPath("dev01"): "i-1"}}}
	svc := &Service{clients: &awsx.Clients{EC2: &fakeUpdateEC2{t: t}, SSM: ssmf}}
	_, err := svc.Update(context.Background(), UpdateInput{Name: "dev01"})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("Update error = %v, want the kind read failure", err)
	}
}
