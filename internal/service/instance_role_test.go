package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/naming"
)

// fakeIAM is a hand-rolled mock for awsx.IAMAPI following the fakeEC2
// pattern: unimplemented methods panic via the nil embedded interface
// so each test overrides only the surface it exercises.
type fakeIAM struct {
	awsx.IAMAPI // nil; calling un-overridden methods panics

	getRole                     func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	createRole                  func(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	attachRolePolicy            func(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	getInstanceProfile          func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	createInstanceProfile       func(context.Context, *iam.CreateInstanceProfileInput, ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	addRoleToInstanceProfile    func(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	simulatePrincipalPolicy     func(context.Context, *iam.SimulatePrincipalPolicyInput, ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error)
	listInstanceProfilesForRole func(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	listAttachedRolePolicies    func(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
}

func (f *fakeIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return f.getRole(ctx, in, opts...)
}
func (f *fakeIAM) CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	return f.createRole(ctx, in, opts...)
}
func (f *fakeIAM) AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, opts ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	return f.attachRolePolicy(ctx, in, opts...)
}
func (f *fakeIAM) GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	return f.getInstanceProfile(ctx, in, opts...)
}
func (f *fakeIAM) CreateInstanceProfile(ctx context.Context, in *iam.CreateInstanceProfileInput, opts ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	return f.createInstanceProfile(ctx, in, opts...)
}
func (f *fakeIAM) AddRoleToInstanceProfile(ctx context.Context, in *iam.AddRoleToInstanceProfileInput, opts ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	return f.addRoleToInstanceProfile(ctx, in, opts...)
}
func (f *fakeIAM) SimulatePrincipalPolicy(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, opts ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	return f.simulatePrincipalPolicy(ctx, in, opts...)
}
func (f *fakeIAM) ListInstanceProfilesForRole(ctx context.Context, in *iam.ListInstanceProfilesForRoleInput, opts ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return f.listInstanceProfilesForRole(ctx, in, opts...)
}
func (f *fakeIAM) ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return f.listAttachedRolePolicies(ctx, in, opts...)
}

const (
	testAccountID = "123456789012"
	testCallerARN = "arn:aws:iam::123456789012:user/dev"
)

func newServiceWithIAM(iamAPI awsx.IAMAPI) *Service {
	return &Service{clients: &awsx.Clients{
		IAM:       iamAPI,
		AccountID: testAccountID,
		CallerARN: testCallerARN,
	}}
}

// noSuchEntity mimics the IAM NoSuchEntity API error.
func noSuchEntity() error {
	return &smithy.GenericAPIError{Code: "NoSuchEntity", Message: "not found", Fault: smithy.FaultClient}
}

// limitExceeded mimics the IAM LimitExceeded API error returned when a
// role is already attached to an instance profile.
func limitExceeded() error {
	return &smithy.GenericAPIError{Code: "LimitExceeded", Message: "already has a role", Fault: smithy.FaultClient}
}

// allowAll returns a SimulatePrincipalPolicy stub that allows every
// requested action.
func allowAll() func(context.Context, *iam.SimulatePrincipalPolicyInput, ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	return func(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
		out := &iam.SimulatePrincipalPolicyOutput{}
		for _, a := range in.ActionNames {
			out.EvaluationResults = append(out.EvaluationResults, iamtypes.EvaluationResult{
				EvalActionName: aws.String(a),
				EvalDecision:   iamtypes.PolicyEvaluationDecisionTypeAllowed,
			})
		}
		return out, nil
	}
}

// encodedTrustPolicy URL-encodes a trust policy document the way the
// IAM GetRole API returns AssumeRolePolicyDocument.
func encodedTrustPolicy(doc string) string {
	return url.QueryEscape(doc)
}

const ec2TrustDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

// --- D4: assumed-role ARN normalization (pure function) ---

func TestPolicySourceARNFromCaller(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "IAM user passthrough",
			in:   "arn:aws:iam::123456789012:user/dev",
			want: "arn:aws:iam::123456789012:user/dev",
		},
		{
			name: "root passthrough",
			in:   "arn:aws:iam::123456789012:root",
			want: "arn:aws:iam::123456789012:root",
		},
		{
			name: "assumed-role converted to iam role",
			in:   "arn:aws:sts::123456789012:assumed-role/AdminRole/my-session",
			want: "arn:aws:iam::123456789012:role/AdminRole",
		},
		{
			name: "assumed-role keeps partition",
			in:   "arn:aws-cn:sts::123456789012:assumed-role/AdminRole/s",
			want: "arn:aws-cn:iam::123456789012:role/AdminRole",
		},
		{
			name:    "empty ARN",
			in:      "",
			wantErr: true,
		},
		{
			name:    "malformed assumed-role without session",
			in:      "arn:aws:sts::123456789012:assumed-role/OnlyRole",
			wantErr: true,
		},
		{
			name:    "not an ARN",
			in:      "bogus",
			wantErr: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := policySourceARNFromCaller(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("policySourceARNFromCaller(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- D3: trust policy validation ---

func TestValidateEC2TrustPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		doc     string
		wantErr bool
	}{
		{
			name: "service as string",
			doc:  ec2TrustDoc,
		},
		{
			name: "service as slice",
			doc:  `{"Statement":[{"Effect":"Allow","Principal":{"Service":["ssm.amazonaws.com","ec2.amazonaws.com"]},"Action":"sts:AssumeRole"}]}`,
		},
		{
			name: "action as slice",
			doc:  `{"Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`,
		},
		{
			name: "single statement object",
			doc:  `{"Statement":{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}}`,
		},
		{
			name:    "deny effect does not count",
			doc:     `{"Statement":[{"Effect":"Deny","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			wantErr: true,
		},
		{
			name:    "lambda-only principal",
			doc:     `{"Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			wantErr: true,
		},
		{
			name:    "aws principal only (no service)",
			doc:     `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::123456789012:root"},"Action":"sts:AssumeRole"}]}`,
			wantErr: true,
		},
		{
			name:    "wrong action",
			doc:     `{"Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:TagSession"}]}`,
			wantErr: true,
		},
		{
			name:    "empty document",
			doc:     "",
			wantErr: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var in string
			if c.doc != "" {
				in = encodedTrustPolicy(c.doc)
			}
			err := validateEC2TrustPolicy(in)
			if c.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- D4: preflight assembles the right simulations, fail-closed ---

func TestPreflight_AllAllowed_ChecksExpectedActions(t *testing.T) {
	t.Parallel()
	var inputs []*iam.SimulatePrincipalPolicyInput
	f := &fakeIAM{
		simulatePrincipalPolicy: func(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, opts ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			inputs = append(inputs, in)
			return allowAll()(ctx, in, opts...)
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		roleARN:     "arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole(),
		profileName: naming.AdminInstanceProfile(),
		// role and profile do not exist yet -> all writes simulated
	}
	if err := s.preflightInstanceRolePermissions(context.Background(), sel); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(inputs) != 3 {
		t.Fatalf("SimulatePrincipalPolicy calls = %d, want 3", len(inputs))
	}

	// Group 1: iam:PassRole against the role ARN with PassedToService.
	g1 := inputs[0]
	if aws.ToString(g1.PolicySourceArn) != testCallerARN {
		t.Errorf("PolicySourceArn = %q, want %q", aws.ToString(g1.PolicySourceArn), testCallerARN)
	}
	if len(g1.ActionNames) != 1 || g1.ActionNames[0] != "iam:PassRole" {
		t.Errorf("group1 actions = %v, want [iam:PassRole]", g1.ActionNames)
	}
	if len(g1.ResourceArns) != 1 || g1.ResourceArns[0] != sel.roleARN {
		t.Errorf("group1 resources = %v, want [%s]", g1.ResourceArns, sel.roleARN)
	}
	if len(g1.ContextEntries) != 1 ||
		aws.ToString(g1.ContextEntries[0].ContextKeyName) != "iam:PassedToService" ||
		g1.ContextEntries[0].ContextKeyType != iamtypes.ContextKeyTypeEnumString ||
		len(g1.ContextEntries[0].ContextKeyValues) != 1 ||
		g1.ContextEntries[0].ContextKeyValues[0] != "ec2.amazonaws.com" {
		t.Errorf("group1 context entries = %+v", g1.ContextEntries)
	}

	// Group 2: role creation actions against the role ARN.
	g2 := inputs[1]
	if want := []string{"iam:CreateRole", "iam:AttachRolePolicy"}; strings.Join(g2.ActionNames, ",") != strings.Join(want, ",") {
		t.Errorf("group2 actions = %v, want %v", g2.ActionNames, want)
	}

	// Group 3: profile creation actions against the profile ARN.
	g3 := inputs[2]
	if want := []string{"iam:CreateInstanceProfile", "iam:AddRoleToInstanceProfile"}; strings.Join(g3.ActionNames, ",") != strings.Join(want, ",") {
		t.Errorf("group3 actions = %v, want %v", g3.ActionNames, want)
	}
	wantProfileARN := "arn:aws:iam::" + testAccountID + ":instance-profile/" + naming.AdminInstanceProfile()
	if len(g3.ResourceArns) != 1 || g3.ResourceArns[0] != wantProfileARN {
		t.Errorf("group3 resources = %v, want [%s]", g3.ResourceArns, wantProfileARN)
	}
}

func TestPreflight_ExistingRoleAndProfile_OnlyPassRole(t *testing.T) {
	t.Parallel()
	var inputs []*iam.SimulatePrincipalPolicyInput
	f := &fakeIAM{
		simulatePrincipalPolicy: func(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, opts ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			inputs = append(inputs, in)
			return allowAll()(ctx, in, opts...)
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		roleName:       "my-role",
		roleARN:        "arn:aws:iam::123456789012:role/my-role",
		roleExists:     true,
		profileName:    "my-profile",
		profileExists:  true,
		profileHasRole: true,
	}
	if err := s.preflightInstanceRolePermissions(context.Background(), sel); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("SimulatePrincipalPolicy calls = %d, want 1 (PassRole only)", len(inputs))
	}
	if len(inputs[0].ActionNames) != 1 || inputs[0].ActionNames[0] != "iam:PassRole" {
		t.Errorf("actions = %v, want [iam:PassRole]", inputs[0].ActionNames)
	}
}

func TestPreflight_DeniedActionsListed(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		simulatePrincipalPolicy: func(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			out := &iam.SimulatePrincipalPolicyOutput{}
			for _, a := range in.ActionNames {
				decision := iamtypes.PolicyEvaluationDecisionTypeAllowed
				if a == "iam:CreateRole" || a == "iam:PassRole" {
					decision = iamtypes.PolicyEvaluationDecisionTypeImplicitDeny
				}
				out.EvaluationResults = append(out.EvaluationResults, iamtypes.EvaluationResult{
					EvalActionName: aws.String(a),
					EvalDecision:   decision,
				})
			}
			return out, nil
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		roleARN:     "arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole(),
		profileName: naming.AdminInstanceProfile(),
	}
	err := s.preflightInstanceRolePermissions(context.Background(), sel)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{"iam:PassRole", "iam:CreateRole"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list denied action %s", err.Error(), want)
		}
	}
}

func TestPreflight_TruncatedResultsArePaged(t *testing.T) {
	t.Parallel()
	calls := 0
	f := &fakeIAM{
		simulatePrincipalPolicy: func(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			calls++
			if in.Marker == nil {
				return &iam.SimulatePrincipalPolicyOutput{
					EvaluationResults: []iamtypes.EvaluationResult{{
						EvalActionName: aws.String("iam:PassRole"),
						EvalDecision:   iamtypes.PolicyEvaluationDecisionTypeAllowed,
					}},
					IsTruncated: true,
					Marker:      aws.String("page2"),
				}, nil
			}
			return &iam.SimulatePrincipalPolicyOutput{
				EvaluationResults: []iamtypes.EvaluationResult{{
					EvalActionName: aws.String("iam:PassRole"),
					EvalDecision:   iamtypes.PolicyEvaluationDecisionTypeExplicitDeny,
				}},
			}, nil
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		roleName:       "my-role",
		roleARN:        "arn:aws:iam::123456789012:role/my-role",
		roleExists:     true,
		profileName:    "my-profile",
		profileExists:  true,
		profileHasRole: true,
	}
	err := s.preflightInstanceRolePermissions(context.Background(), sel)
	if err == nil {
		t.Fatal("expected error from denial on page 2, got nil")
	}
	if calls != 2 {
		t.Errorf("SimulatePrincipalPolicy calls = %d, want 2 (Marker loop)", calls)
	}
}

func TestPreflight_SimulateFailureIsFailClosed(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		simulatePrincipalPolicy: func(context.Context, *iam.SimulatePrincipalPolicyInput, ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "no simulate for you", Fault: smithy.FaultClient}
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		roleName:       "my-role",
		roleARN:        "arn:aws:iam::123456789012:role/my-role",
		roleExists:     true,
		profileName:    "p",
		profileExists:  true,
		profileHasRole: true,
	}
	err := s.preflightInstanceRolePermissions(context.Background(), sel)
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "iam:SimulatePrincipalPolicy") {
		t.Errorf("error %q should mention the iam:SimulatePrincipalPolicy requirement", err.Error())
	}
	if !strings.Contains(err.Error(), "could not run") {
		t.Errorf("error %q should state that the preflight could not run", err.Error())
	}
}

func TestPreflight_AssumedRoleCallerIsNormalized(t *testing.T) {
	t.Parallel()
	var gotSource string
	f := &fakeIAM{
		simulatePrincipalPolicy: func(ctx context.Context, in *iam.SimulatePrincipalPolicyInput, opts ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			gotSource = aws.ToString(in.PolicySourceArn)
			return allowAll()(ctx, in, opts...)
		},
	}
	s := &Service{clients: &awsx.Clients{
		IAM:       f,
		AccountID: testAccountID,
		CallerARN: "arn:aws:sts::123456789012:assumed-role/DevRole/session-1",
	}}
	sel := &instanceProfileSelection{
		roleName:       "my-role",
		roleARN:        "arn:aws:iam::123456789012:role/my-role",
		roleExists:     true,
		profileName:    "p",
		profileExists:  true,
		profileHasRole: true,
	}
	if err := s.preflightInstanceRolePermissions(context.Background(), sel); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if want := "arn:aws:iam::123456789012:role/DevRole"; gotSource != want {
		t.Errorf("PolicySourceArn = %q, want %q", gotSource, want)
	}
}

// --- D4/D11(1): denied preflight aborts CreateInstance before any resource ---

// fakeCreateSSM reports the environment as initialized and records
// every PutParameter call (which must stay at zero when the preflight
// denies the create).
type fakeCreateSSM struct {
	awsx.SSMAPI
	putCalls int
}

func (f *fakeCreateSSM) GetParameter(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String("true")}}, nil
}

func (f *fakeCreateSSM) PutParameter(_ context.Context, _ *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.putCalls++
	return &ssm.PutParameterOutput{}, nil
}

// fakeCreateEC2 answers the name-availability check with "no such
// instance"; every other method panics, proving that a denied
// preflight prevents any EC2 resource call.
type fakeCreateEC2 struct {
	awsx.EC2API
}

func (f *fakeCreateEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func TestCreateInstance_PreflightDenied_NothingCreated(t *testing.T) {
	t.Parallel()
	ssmFake := &fakeCreateSSM{}
	createRoleCalls := 0
	iamFake := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return nil, noSuchEntity()
		},
		getInstanceProfile: func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
			return nil, noSuchEntity()
		},
		createRole: func(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
			createRoleCalls++
			return nil, errors.New("must not be called")
		},
		simulatePrincipalPolicy: func(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
			out := &iam.SimulatePrincipalPolicyOutput{}
			for _, a := range in.ActionNames {
				out.EvaluationResults = append(out.EvaluationResults, iamtypes.EvaluationResult{
					EvalActionName: aws.String(a),
					EvalDecision:   iamtypes.PolicyEvaluationDecisionTypeImplicitDeny,
				})
			}
			return out, nil
		},
	}
	s := &Service{clients: &awsx.Clients{
		EC2:       &fakeCreateEC2{},
		SSM:       ssmFake,
		IAM:       iamFake,
		AccountID: testAccountID,
		CallerARN: testCallerARN,
	}}

	_, err := s.CreateInstance(context.Background(), CreateInstanceInput{
		Name:         "dev01",
		InstanceType: "t3.medium",
		WithAdmin:    true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "iam:PassRole") {
		t.Errorf("error %q should list the denied actions", err.Error())
	}
	if ssmFake.putCalls != 0 {
		t.Errorf("PutParameter calls = %d, want 0 (no resource may be created)", ssmFake.putCalls)
	}
	if createRoleCalls != 0 {
		t.Errorf("CreateRole calls = %d, want 0", createRoleCalls)
	}
}

func TestCreateInstance_WithAdminAndWithRoleAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	s := &Service{clients: &awsx.Clients{}}
	_, err := s.CreateInstance(context.Background(), CreateInstanceInput{
		Name:         "dev01",
		InstanceType: "t3.medium",
		WithAdmin:    true,
		WithRole:     "my-role",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

// --- D2/D11(3): --with-admin upsert idempotency ---

func TestProvisionAdmin_FreshCreatesRoleAndProfile(t *testing.T) {
	t.Parallel()
	var createdRole, attachedPolicy, createdProfile, addedRole string
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return nil, noSuchEntity()
		},
		createRole: func(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
			createdRole = aws.ToString(in.RoleName)
			if !strings.Contains(aws.ToString(in.AssumeRolePolicyDocument), "ec2.amazonaws.com") {
				t.Errorf("trust policy should target ec2.amazonaws.com: %s", aws.ToString(in.AssumeRolePolicyDocument))
			}
			return &iam.CreateRoleOutput{Role: &iamtypes.Role{
				Arn: aws.String("arn:aws:iam::123456789012:role/" + createdRole),
			}}, nil
		},
		attachRolePolicy: func(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
			attachedPolicy = aws.ToString(in.PolicyArn)
			return &iam.AttachRolePolicyOutput{}, nil
		},
		getInstanceProfile: func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
			return nil, noSuchEntity()
		},
		createInstanceProfile: func(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
			createdProfile = aws.ToString(in.InstanceProfileName)
			return &iam.CreateInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
				Arn: aws.String("arn:aws:iam::123456789012:instance-profile/" + createdProfile),
			}}, nil
		},
		addRoleToInstanceProfile: func(_ context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
			addedRole = aws.ToString(in.RoleName)
			return &iam.AddRoleToInstanceProfileOutput{}, nil
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		roleARN:     "arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole(),
		profileName: naming.AdminInstanceProfile(),
	}
	if err := s.provisionInstanceProfile(context.Background(), sel); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if createdRole != naming.AdminInstanceRole() {
		t.Errorf("created role = %q, want %q", createdRole, naming.AdminInstanceRole())
	}
	if attachedPolicy != adminManagedPolicyARN {
		t.Errorf("attached policy = %q, want %q", attachedPolicy, adminManagedPolicyARN)
	}
	if createdProfile != naming.AdminInstanceProfile() {
		t.Errorf("created profile = %q, want %q", createdProfile, naming.AdminInstanceProfile())
	}
	if addedRole != naming.AdminInstanceRole() {
		t.Errorf("added role = %q, want %q", addedRole, naming.AdminInstanceRole())
	}
}

func TestProvisionAdmin_ExistingIsIdempotent(t *testing.T) {
	t.Parallel()
	attachCalls := 0
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{
				Arn: aws.String("arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole()),
			}}, nil
		},
		attachRolePolicy: func(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
			attachCalls++
			return &iam.AttachRolePolicyOutput{}, nil
		},
		getInstanceProfile: func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
			return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
				Arn:   aws.String("arn:aws:iam::123456789012:instance-profile/" + naming.AdminInstanceProfile()),
				Roles: []iamtypes.Role{{RoleName: aws.String(naming.AdminInstanceRole())}},
			}}, nil
		},
		// createRole / createInstanceProfile / addRoleToInstanceProfile
		// deliberately unset: calling them would panic the test.
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		roleARN:     "arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole(),
		roleExists:  true,
		profileName: naming.AdminInstanceProfile(),
	}
	// Run twice to prove idempotency.
	for i := 0; i < 2; i++ {
		if err := s.provisionInstanceProfile(context.Background(), sel); err != nil {
			t.Fatalf("run %d: expected nil, got %v", i+1, err)
		}
	}
	if attachCalls != 2 {
		t.Errorf("AttachRolePolicy calls = %d, want 2 (healing re-attach per run)", attachCalls)
	}
}

func TestProvisionAdmin_AddRoleLimitExceededTolerated(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{
				Arn: aws.String("arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole()),
			}}, nil
		},
		attachRolePolicy: func(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
			return &iam.AttachRolePolicyOutput{}, nil
		},
		getInstanceProfile: func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
			// Profile exists but reports no roles (stale read):
			// AddRoleToInstanceProfile then fails with LimitExceeded.
			return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
				Arn: aws.String("arn:aws:iam::123456789012:instance-profile/" + naming.AdminInstanceProfile()),
			}}, nil
		},
		addRoleToInstanceProfile: func(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
			return nil, limitExceeded()
		},
	}
	s := newServiceWithIAM(f)
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		roleARN:     "arn:aws:iam::123456789012:role/" + naming.AdminInstanceRole(),
		roleExists:  true,
		profileName: naming.AdminInstanceProfile(),
	}
	if err := s.provisionInstanceProfile(context.Background(), sel); err != nil {
		t.Fatalf("LimitExceeded must be tolerated, got %v", err)
	}
}

// --- D3/D11(4): --with-role resolution ---

func TestResolveCustomRole_NotFound(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return nil, noSuchEntity()
		},
	}
	s := newServiceWithIAM(f)
	_, err := s.resolveCustomProfileSelection(context.Background(), "ghost-role")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestResolveCustomRole_InvalidName(t *testing.T) {
	t.Parallel()
	s := newServiceWithIAM(&fakeIAM{})
	cases := []string{"", "role with space", "role/slash", strings.Repeat("a", 65)}
	for _, name := range cases {
		if _, err := s.resolveCustomProfileSelection(context.Background(), name); err == nil {
			t.Errorf("expected validation error for %q, got nil", name)
		}
	}
}

func TestResolveCustomRole_TrustPolicyMismatch(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{
				Arn: aws.String("arn:aws:iam::123456789012:role/lambda-role"),
				AssumeRolePolicyDocument: aws.String(encodedTrustPolicy(
					`{"Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`)),
			}}, nil
		},
	}
	s := newServiceWithIAM(f)
	_, err := s.resolveCustomProfileSelection(context.Background(), "lambda-role")
	if err == nil || !strings.Contains(err.Error(), "trust policy") {
		t.Fatalf("expected trust-policy error, got %v", err)
	}
}

func TestResolveCustomRole_ReusesExistingProfile(t *testing.T) {
	t.Parallel()
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{
				Arn:                      aws.String("arn:aws:iam::123456789012:role/my-role"),
				AssumeRolePolicyDocument: aws.String(encodedTrustPolicy(ec2TrustDoc)),
			}}, nil
		},
		listAttachedRolePolicies: func(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
			return &iam.ListAttachedRolePoliciesOutput{AttachedPolicies: []iamtypes.AttachedPolicy{
				{PolicyArn: aws.String("arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore")},
			}}, nil
		},
		listInstanceProfilesForRole: func(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
			return &iam.ListInstanceProfilesForRoleOutput{InstanceProfiles: []iamtypes.InstanceProfile{
				{InstanceProfileName: aws.String("existing-profile")},
				{InstanceProfileName: aws.String("second-profile")},
			}}, nil
		},
	}
	s := newServiceWithIAM(f)
	sel, err := s.resolveCustomProfileSelection(context.Background(), "my-role")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if sel.profileName != "existing-profile" {
		t.Errorf("profileName = %q, want the first existing profile", sel.profileName)
	}
	if !sel.profileExists || !sel.profileHasRole {
		t.Errorf("profileExists/profileHasRole = %v/%v, want true/true", sel.profileExists, sel.profileHasRole)
	}
	// Reuse path: provisioning must not perform any IAM write (the
	// write funcs are unset and would panic).
	if err := s.provisionInstanceProfile(context.Background(), sel); err != nil {
		t.Fatalf("reuse provisioning should be a no-op, got %v", err)
	}
}

func TestResolveCustomRole_NewProfileCreated(t *testing.T) {
	t.Parallel()
	var createdProfile, addedRole string
	f := &fakeIAM{
		getRole: func(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iamtypes.Role{
				Arn:                      aws.String("arn:aws:iam::123456789012:role/my-role"),
				AssumeRolePolicyDocument: aws.String(encodedTrustPolicy(ec2TrustDoc)),
			}}, nil
		},
		listAttachedRolePolicies: func(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
			// No SSM policy: triggers the best-effort warning only.
			return &iam.ListAttachedRolePoliciesOutput{}, nil
		},
		listInstanceProfilesForRole: func(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
			return &iam.ListInstanceProfilesForRoleOutput{}, nil
		},
		getInstanceProfile: func(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
			return nil, noSuchEntity()
		},
		createInstanceProfile: func(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
			createdProfile = aws.ToString(in.InstanceProfileName)
			return &iam.CreateInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
				Arn: aws.String("arn:aws:iam::123456789012:instance-profile/" + createdProfile),
			}}, nil
		},
		addRoleToInstanceProfile: func(_ context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
			addedRole = aws.ToString(in.RoleName)
			return &iam.AddRoleToInstanceProfileOutput{}, nil
		},
		// createRole / attachRolePolicy unset: the user's role must
		// never be created or modified (calling them panics).
	}
	s := newServiceWithIAM(f)
	sel, err := s.resolveCustomProfileSelection(context.Background(), "my-role")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if want := naming.CustomInstanceProfile("my-role"); sel.profileName != want {
		t.Errorf("profileName = %q, want %q", sel.profileName, want)
	}
	if sel.profileExists {
		t.Errorf("profileExists = true, want false")
	}
	if err := s.provisionInstanceProfile(context.Background(), sel); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if createdProfile != naming.CustomInstanceProfile("my-role") {
		t.Errorf("created profile = %q, want %q", createdProfile, naming.CustomInstanceProfile("my-role"))
	}
	if addedRole != "my-role" {
		t.Errorf("added role = %q, want my-role", addedRole)
	}
}

// --- D5/D11(5): runInstance uses the selected profile name ---

// fakeRunEC2 captures the RunInstances input.
type fakeRunEC2 struct {
	awsx.EC2API
	got *ec2.RunInstancesInput
}

func (f *fakeRunEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.got = in
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-test")}}}, nil
}

func TestRunInstance_UsesSelectedInstanceProfile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		profileName string
		want        string
	}{
		{name: "default (empty) uses shared base profile", profileName: "", want: naming.InstanceProfile()},
		{name: "admin profile", profileName: naming.AdminInstanceProfile(), want: naming.AdminInstanceProfile()},
		{name: "custom profile", profileName: "existing-profile", want: "existing-profile"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRunEC2{}
			s := newServiceWithEC2(f)
			id, err := s.runInstance(context.Background(), runInstanceArgs{
				Name:                "dev01",
				AMIID:               "ami-123",
				InstanceType:        "t3.medium",
				EBSGiB:              20,
				ENIID:               "eni-123",
				UserDataBase64:      "dGVzdA==",
				InstanceProfileName: c.profileName,
			})
			if err != nil {
				t.Fatalf("runInstance: %v", err)
			}
			if id != "i-test" {
				t.Errorf("instance id = %q, want i-test", id)
			}
			if f.got.IamInstanceProfile == nil || aws.ToString(f.got.IamInstanceProfile.Name) != c.want {
				t.Errorf("IamInstanceProfile = %+v, want name %q", f.got.IamInstanceProfile, c.want)
			}
		})
	}
}

// --- D6/D11(6): instance metadata keys ---

func TestInstanceMetaEntries_WithoutRole(t *testing.T) {
	t.Parallel()
	m := instanceMetaEntries("dev01", instanceMetaValues{
		InstanceID:         "i-1",
		ENIID:              "eni-1",
		SecurityGroupID:    "sg-1",
		VPCOriginID:        "vo-1",
		DistributionID:     "d-1",
		DistributionDomain: "dom",
	})
	if len(m) != 6 {
		t.Fatalf("len = %d, want 6: %v", len(m), m)
	}
	for _, k := range []string{
		"/nakama/dev01/meta/iam-role",
		"/nakama/dev01/meta/instance-profile",
	} {
		if _, ok := m[k]; ok {
			t.Errorf("key %s must be absent without --with-admin/--with-role", k)
		}
	}
}

func TestInstanceMetaEntries_WithRole(t *testing.T) {
	t.Parallel()
	m := instanceMetaEntries("dev01", instanceMetaValues{
		InstanceID:         "i-1",
		ENIID:              "eni-1",
		SecurityGroupID:    "sg-1",
		VPCOriginID:        "vo-1",
		DistributionID:     "d-1",
		DistributionDomain: "dom",
		IAMRole:            "my-role",
		InstanceProfile:    "my-profile",
	})
	if len(m) != 8 {
		t.Fatalf("len = %d, want 8: %v", len(m), m)
	}
	if got := m["/nakama/dev01/meta/iam-role"]; got != "my-role" {
		t.Errorf("iam-role = %q, want my-role", got)
	}
	if got := m["/nakama/dev01/meta/instance-profile"]; got != "my-profile" {
		t.Errorf("instance-profile = %q, want my-profile", got)
	}
}
