package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

// fakeCloudFrontBackend records what the cloudfront commands asked for.
type fakeCloudFrontBackend struct {
	addOut  *service.AddCloudFrontOutput
	addErr  error
	plan    *service.DeleteCloudFrontPlan
	planErr error
	delOut  *service.DeleteCloudFrontOutput
	added   []string
	deleted []*service.DeleteCloudFrontPlan

	// Kiro Crew's part (--kirocrew).
	enableOut  *service.EnableKiroCrewWebOutput
	enableErr  error
	disableOut *service.DisableKiroCrewWebOutput
	enabled    []string
	disabled   []string
	// calls is the order of the AWS-side calls, e.g. "add", "enable".
	calls []string

	acl []service.ACLEntry
	ip  string
}

func (f *fakeCloudFrontBackend) AddCloudFront(_ context.Context, name string) (*service.AddCloudFrontOutput, error) {
	f.added = append(f.added, name)
	f.calls = append(f.calls, "add")
	return f.addOut, f.addErr
}

func (f *fakeCloudFrontBackend) PlanDeleteCloudFront(context.Context, string) (*service.DeleteCloudFrontPlan, error) {
	f.calls = append(f.calls, "plan")
	return f.plan, f.planErr
}

func (f *fakeCloudFrontBackend) DeleteCloudFront(_ context.Context, p *service.DeleteCloudFrontPlan) (*service.DeleteCloudFrontOutput, error) {
	f.deleted = append(f.deleted, p)
	f.calls = append(f.calls, "delete")
	return f.delOut, nil
}

func (f *fakeCloudFrontBackend) EnableKiroCrewWeb(_ context.Context, name string) (*service.EnableKiroCrewWebOutput, error) {
	f.enabled = append(f.enabled, name)
	f.calls = append(f.calls, "enable")
	return f.enableOut, f.enableErr
}

func (f *fakeCloudFrontBackend) DisableKiroCrewWeb(_ context.Context, name string) (*service.DisableKiroCrewWebOutput, error) {
	f.disabled = append(f.disabled, name)
	f.calls = append(f.calls, "disable")
	if f.disableOut == nil {
		return &service.DisableKiroCrewWebOutput{Name: name}, nil
	}
	return f.disableOut, nil
}

func (f *fakeCloudFrontBackend) ListACL(context.Context) ([]service.ACLEntry, error) {
	return f.acl, nil
}

func (f *fakeCloudFrontBackend) PublicIPv4(context.Context) (string, error) { return f.ip, nil }

// runCloudFront runs `nakama aws cloudfront <args>` with the fake and
// the given answer to the prompt. asked reports whether it was asked.
func runCloudFront(t *testing.T, b *fakeCloudFrontBackend, answer bool, args ...string) (stdout, stderr string, asked bool, err error) {
	t.Helper()
	deps := cloudfrontDeps{
		connect: func(context.Context) (cloudfrontBackend, error) { return b, nil },
		confirm: func() (bool, error) { asked = true; return answer, nil },
	}
	cmd := newAWSCloudFrontCommandWithDeps(deps)
	// The root command silences the usage text on errors; so does this
	// stand-alone tree.
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), asked, err
}

func withCloudFrontPlan() *service.DeleteCloudFrontPlan {
	return &service.DeleteCloudFrontPlan{
		Name: "dev01", InstanceID: "i-1", State: "running", SecurityGroupID: "sg-1",
		VPCOriginID: "vo-1", DistributionID: "E1", DistributionDomain: "d1.cloudfront.net",
		KiroCrewDistributionID: "E2", KiroCrewDomain: "d2.cloudfront.net",
	}
}

func TestCloudFrontAdd(t *testing.T) {
	b := &fakeCloudFrontBackend{
		addOut: &service.AddCloudFrontOutput{Name: "dev01", DistributionID: "E1", URL: "https://d1.cloudfront.net"},
		acl:    []service.ACLEntry{{CIDR: "198.51.100.0/24"}},
		ip:     "203.0.113.7",
	}
	out, stderr, _, err := runCloudFront(t, b, false, "add", "dev01")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if strings.Join(b.added, ",") != "dev01" {
		t.Errorf("added %v", b.added)
	}
	for _, want := range []string{"CloudFront added", "https://d1.cloudfront.net", "nakama aws code --internet dev01"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The URL answers 403 outside the allowlist; add says so at once.
	if !strings.Contains(stderr, "15-20 minutes") || !strings.Contains(stderr, "203.0.113.7 is not in the WAF allowlist") {
		t.Errorf("stderr:\n%s", stderr)
	}

	b.addOut.AlreadyPresent = true
	b.acl = []service.ACLEntry{{CIDR: "203.0.113.0/24"}}
	out, stderr, _, err = runCloudFront(t, b, false, "add", "dev01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CloudFront already in place") || strings.Contains(stderr, "allowlist") {
		t.Errorf("second add: out %q, stderr %q", out, stderr)
	}

	b.addErr = errors.New(`instance "dev01" is stopped; start it first`)
	if _, _, _, err := runCloudFront(t, b, false, "add", "dev01"); err == nil || !strings.Contains(err.Error(), "start it first") {
		t.Errorf("the service error must surface: %v", err)
	}
}

func TestCloudFrontDelete_AsksFirst(t *testing.T) {
	b := &fakeCloudFrontBackend{plan: withCloudFrontPlan(), delOut: &service.DeleteCloudFrontOutput{
		Name: "dev01", DistributionID: "E1", KiroCrewDistributionID: "E2", IngressRuleRemoved: true,
	}}

	out, _, asked, err := runCloudFront(t, b, false, "delete", "dev01")
	if !asked || err == nil || !strings.Contains(err.Error(), "aborted") || len(b.deleted) != 0 {
		t.Fatalf("declined: asked %v, err %v, deleted %d", asked, err, len(b.deleted))
	}
	for _, want := range []string{
		`Delete the CloudFront access of "dev01"?`, "E1 (https://d1.cloudfront.net)", "E2 (https://d2.cloudfront.net)",
		"vo-1", "8080/tcp", "sg-1", "new ones", "nakama aws code dev01", "[y/N]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}

	out, stderr, asked, err := runCloudFront(t, b, true, "delete", "dev01")
	if !asked || err != nil || len(b.deleted) != 1 || b.deleted[0] != b.plan {
		t.Fatalf("accepted: asked %v, err %v, deleted %v", asked, err, b.deleted)
	}
	if !strings.Contains(out, "CloudFront deleted for dev01 (Kiro Crew's included)") || !strings.Contains(stderr, "several minutes") {
		t.Errorf("out:\n%s\nstderr:\n%s", out, stderr)
	}

	_, _, asked, err = runCloudFront(t, b, false, "delete", "dev01", "--yes")
	if asked || err != nil || len(b.deleted) != 2 {
		t.Errorf("--yes: asked %v, err %v, deleted %d", asked, err, len(b.deleted))
	}
}

// TestCloudFrontDelete_PlanErrorStopsEverything: a failed plan (an
// unknown instance, a denied read) surfaces as is, without a prompt and
// without a delete (Amazon Q on PR #121).
func TestCloudFrontDelete_PlanErrorStopsEverything(t *testing.T) {
	b := &fakeCloudFrontBackend{planErr: errors.New(`nakama instance "nope" not found`)}
	out, _, asked, err := runCloudFront(t, b, true, "delete", "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the plan error", err)
	}
	if asked || len(b.deleted) != 0 || out != "" {
		t.Errorf("asked %v, deleted %d, stdout %q; want nothing after a failed plan", asked, len(b.deleted), out)
	}
}

// TestCloudFrontDelete_WithoutCloudFront: nothing to confirm; the
// summary says what (if anything) was removed.
func TestCloudFrontDelete_WithoutCloudFront(t *testing.T) {
	plan := &service.DeleteCloudFrontPlan{Name: "box1", InstanceID: "i-1", SecurityGroupID: "sg-1", Bare: true}
	b := &fakeCloudFrontBackend{plan: plan, delOut: &service.DeleteCloudFrontOutput{Name: "box1", IngressRuleRemoved: true}}
	out, _, asked, err := runCloudFront(t, b, false, "delete", "box1")
	if asked || err != nil || len(b.deleted) != 1 {
		t.Fatalf("asked %v, err %v, deleted %d", asked, err, len(b.deleted))
	}
	if !strings.Contains(out, "removed the leftover inbound rule") || strings.Contains(out, "nakama aws code") {
		t.Errorf("summary for a bare instance:\n%s", out)
	}
	b.delOut = &service.DeleteCloudFrontOutput{Name: "box1"}
	out, _, _, _ = runCloudFront(t, b, false, "delete", "box1")
	if !strings.Contains(out, "nothing to delete") {
		t.Errorf("summary with nothing to remove:\n%s", out)
	}
}

func TestCloudFrontDeleteSummary_StoppedInstanceNote(t *testing.T) {
	t.Parallel()
	s := cloudFrontDeleteSummary(withCloudFrontPlan(), &service.DeleteCloudFrontOutput{
		Name: "dev01", KiroCrewDistributionID: "E2", InstanceSkipped: true,
	})
	for _, want := range []string{"not running", "nakama aws start dev01", "nakama aws cloudfront delete dev01 --kirocrew", "nakama aws code dev01"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}

func TestCloudFrontDelete_JSONOutput(t *testing.T) {
	prev := global.Output
	global.Output = "json"
	t.Cleanup(func() { global.Output = prev })
	b := &fakeCloudFrontBackend{plan: withCloudFrontPlan(), delOut: &service.DeleteCloudFrontOutput{
		Name: "dev01", DistributionID: "E1", IngressRuleRemoved: true,
	}}
	out, _, _, err := runCloudFront(t, b, false, "delete", "dev01", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var res service.DeleteCloudFrontOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.DistributionID != "E1" || !res.IngressRuleRemoved {
		t.Errorf("result = %+v", res)
	}
}

func TestCloudFrontCommand_Help(t *testing.T) {
	t.Parallel()
	cmd := newAWSCloudFrontCommand()
	for _, sub := range []string{"add", "delete"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil || c.Name() != sub {
			t.Fatalf("subcommand %s missing: %v", sub, err)
		}
		if c.Flags().Lookup("kirocrew") == nil {
			t.Errorf("%s must offer --kirocrew", sub)
		}
	}
	del, _, _ := cmd.Find([]string{"delete"})
	if del.Flags().Lookup("yes") == nil {
		t.Error("delete must offer --yes")
	}
	// The rule is removed first: the help says so, because it is what
	// makes a stopped delete safe.
	if !strings.Contains(del.Long, "first, so nothing reaches the instance") {
		t.Errorf("delete help must explain the order:\n%s", del.Long)
	}
}

func TestCreateCommand_BareAndCloudFrontMutuallyExclusive(t *testing.T) {
	t.Parallel()
	cmd := newAWSCreateCommand()
	cmd.SetArgs([]string{"dev01", "--instance-type", "t3.medium", "--bare", "--cloudfront"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cloudfront") {
		t.Fatalf("expected the --bare/--cloudfront mutual-exclusion error, got %v", err)
	}
	in := createInputFromFlags(&createFlags{name: "dev01", instanceType: "t3.medium", cloudFront: true})
	if !in.CloudFront {
		t.Error("--cloudfront must reach the service input")
	}
	if createInputFromFlags(&createFlags{name: "dev01", instanceType: "t3.medium"}).CloudFront {
		t.Error("CloudFront must be off by default (ADR 0028)")
	}
}

func TestFormatInfo_CloudFrontOrNot(t *testing.T) {
	t.Parallel()
	without := formatInfo(&service.InstanceInfo{Name: "dev01", Kind: "vscode"}, false)
	for _, want := range []string{"cloudfront        : none", "nakama aws code dev01", "nakama aws cloudfront add dev01"} {
		if !strings.Contains(without, want) {
			t.Errorf("info without CloudFront lacks %q:\n%s", want, without)
		}
	}
	if strings.Contains(without, "url ") || strings.Contains(without, "distribution_id") {
		t.Errorf("info without CloudFront must not show empty CloudFront fields:\n%s", without)
	}
	with := formatInfo(&service.InstanceInfo{
		Name: "dev01", Kind: "vscode", CloudFront: true, DistributionID: "E1", URL: "https://d1.cloudfront.net", VPCOriginID: "vo-1",
	}, false)
	for _, want := range []string{"distribution_id   : E1", "url               : https://d1.cloudfront.net", "vpc_origin_id     : vo-1"} {
		if !strings.Contains(with, want) {
			t.Errorf("info with CloudFront lacks %q:\n%s", want, with)
		}
	}
	bare := formatInfo(&service.InstanceInfo{Name: "box1", Kind: "bare"}, false)
	if strings.Contains(bare, "cloudfront") {
		t.Errorf("a bare instance gets no CloudFront hint:\n%s", bare)
	}
}

// TestCloudFrontAdd_KiroCrew: --kirocrew builds code-server's part
// first (idempotent) and then Kiro Crew's, which `kirocrew enable` did
// on its own before Issue #129.
func TestCloudFrontAdd_KiroCrew(t *testing.T) {
	b := &fakeCloudFrontBackend{
		addOut:    &service.AddCloudFrontOutput{Name: "dev01", DistributionID: "E1", URL: "https://d1.cloudfront.net"},
		enableOut: &service.EnableKiroCrewWebOutput{Name: "dev01", DistributionID: "E2", URL: "https://d2.cloudfront.net/"},
		ip:        "203.0.113.7",
		acl:       []service.ACLEntry{{CIDR: "203.0.113.0/24"}},
	}
	out, stderr, _, err := runCloudFront(t, b, false, "add", "dev01", "--kirocrew")
	if err != nil {
		t.Fatalf("add --kirocrew: %v", err)
	}
	if got := strings.Join(b.calls, ","); got != "add,enable" {
		t.Errorf("calls %s, want add,enable", got)
	}
	for _, want := range []string{
		"CloudFront added, Kiro Crew's included", "https://d1.cloudfront.net", "kirocrew : https://d2.cloudfront.net/",
		"nakama aws code --internet dev01", "nakama aws kirocrew --internet dev01",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr, "Kiro Crew's included") || !strings.Contains(stderr, "code-server restarts once") {
		t.Errorf("stderr:\n%s", stderr)
	}

	// Kiro Crew's part failing leaves code-server's in place; the error
	// says so and how to continue.
	b.calls, b.enableErr = nil, errors.New("Kiro Crew is not installed")
	_, _, _, err = runCloudFront(t, b, false, "add", "dev01", "--kirocrew")
	if err == nil || !strings.Contains(err.Error(), "is in place (https://d1.cloudfront.net), but Kiro Crew's is not: Kiro Crew is not installed") ||
		!strings.Contains(err.Error(), "nakama aws cloudfront add dev01 --kirocrew") {
		t.Errorf("err = %v", err)
	}

	// code-server's part failing stops before Kiro Crew's.
	b.calls, b.addErr = nil, errors.New(`instance "dev01" is stopped; start it first`)
	if _, _, _, err := runCloudFront(t, b, false, "add", "dev01", "--kirocrew"); err == nil || strings.Join(b.calls, ",") != "add" {
		t.Errorf("err %v, calls %v; want the add error and no Kiro Crew call", err, b.calls)
	}

	// Without --kirocrew, Kiro Crew is left alone.
	b.calls, b.addErr = nil, nil
	if _, _, _, err := runCloudFront(t, b, false, "add", "dev01"); err != nil || strings.Join(b.calls, ",") != "add" {
		t.Errorf("err %v, calls %v; want only add", err, b.calls)
	}
}

// TestCloudFrontAdd_KiroCrewJSON: the JSON result is a plain add's,
// plus a kirocrew object.
func TestCloudFrontAdd_KiroCrewJSON(t *testing.T) {
	prev := global.Output
	t.Cleanup(func() { global.Output = prev })
	b := &fakeCloudFrontBackend{
		addOut:    &service.AddCloudFrontOutput{Name: "dev01", DistributionID: "E1", URL: "https://d1.cloudfront.net"},
		enableOut: &service.EnableKiroCrewWebOutput{Name: "dev01", DistributionID: "E2", URL: "https://d2.cloudfront.net/"},
	}
	global.Output = "json"
	out, _, _, err := runCloudFront(t, b, false, "add", "dev01", "--kirocrew")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Name           string `json:"name"`
		DistributionID string `json:"distribution_id"`
		URL            string `json:"url"`
		KiroCrew       struct {
			DistributionID string `json:"distribution_id"`
			URL            string `json:"url"`
		} `json:"kirocrew"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Name != "dev01" || res.DistributionID != "E1" || res.URL != "https://d1.cloudfront.net" ||
		res.KiroCrew.DistributionID != "E2" || res.KiroCrew.URL != "https://d2.cloudfront.net/" {
		t.Errorf("result = %+v\n%s", res, out)
	}

	global.Output = "yaml"
	out, _, _, err = runCloudFront(t, b, false, "add", "dev01", "--kirocrew")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: dev01", "kirocrew:", "url: https://d2.cloudfront.net/"} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "addcloudfrontoutput") {
		t.Errorf("YAML nests the add result instead of inlining it:\n%s", out)
	}
}

// TestCloudFrontDelete_KiroCrewOnly: --kirocrew removes only Kiro
// Crew's part, and asks first like a full delete (it did not as
// `kirocrew disable`).
func TestCloudFrontDelete_KiroCrewOnly(t *testing.T) {
	b := &fakeCloudFrontBackend{plan: withCloudFrontPlan()}

	out, _, asked, err := runCloudFront(t, b, false, "delete", "dev01", "--kirocrew")
	if !asked || err == nil || !strings.Contains(err.Error(), "aborted") || len(b.disabled) != 0 || len(b.deleted) != 0 {
		t.Fatalf("declined: asked %v, err %v, disabled %v, deleted %d", asked, err, b.disabled, len(b.deleted))
	}
	for _, want := range []string{
		`Delete the Kiro Crew CloudFront access of "dev01"?`, "E2 (https://d2.cloudfront.net)",
		"code-server's CloudFront access is kept", "nakama aws kirocrew dev01", "[y/N]: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "E1") || strings.Contains(out, "vo-1") {
		t.Errorf("the prompt lists code-server's parts, which stay:\n%s", out)
	}

	out, stderr, asked, err := runCloudFront(t, b, true, "delete", "dev01", "--kirocrew")
	if !asked || err != nil || strings.Join(b.disabled, ",") != "dev01" || len(b.deleted) != 0 {
		t.Fatalf("accepted: asked %v, err %v, disabled %v, deleted %d", asked, err, b.disabled, len(b.deleted))
	}
	if !strings.Contains(out, "Kiro Crew's CloudFront access deleted for dev01; code-server's is kept") ||
		!strings.Contains(stderr, "several minutes") {
		t.Errorf("out:\n%s\nstderr:\n%s", out, stderr)
	}

	_, _, asked, err = runCloudFront(t, b, false, "delete", "dev01", "--kirocrew", "-y")
	if asked || err != nil || len(b.disabled) != 2 {
		t.Errorf("-y: asked %v, err %v, disabled %v", asked, err, b.disabled)
	}

	// No Kiro Crew distribution: nothing to confirm, leftovers cleared.
	b.plan = &service.DeleteCloudFrontPlan{Name: "dev01", DistributionID: "E1", VPCOriginID: "vo-1"}
	b.disableOut = &service.DisableKiroCrewWebOutput{Name: "dev01", InstanceSkipped: true}
	out, _, asked, err = runCloudFront(t, b, false, "delete", "dev01", "--kirocrew")
	if asked || err != nil || len(b.disabled) != 3 {
		t.Fatalf("no distribution: asked %v, err %v, disabled %v", asked, err, b.disabled)
	}
	for _, want := range []string{"had no Kiro Crew distribution", "not running", "nakama aws start dev01",
		"nakama aws cloudfront delete dev01 --kirocrew"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}

	b.planErr = errors.New(`nakama instance "nope" not found`)
	if _, _, _, err := runCloudFront(t, b, true, "delete", "nope", "--kirocrew"); err == nil || len(b.disabled) != 3 {
		t.Errorf("a failed plan must stop the delete: err %v, disabled %v", err, b.disabled)
	}
}
