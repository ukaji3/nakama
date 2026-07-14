package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// adminManagedPolicyARN is the AWS-managed AdministratorAccess policy
// attached to the shared admin instance role (`--with-admin`). It is a
// superset of AmazonSSMManagedInstanceCore and of the nakama inline
// SSM policy, so no additional policies are attached (ADR 0010).
const adminManagedPolicyARN = "arn:aws:iam::aws:policy/AdministratorAccess"

// ec2ServicePrincipal is the service principal an instance role's
// trust policy must allow so that EC2 can assume the role.
const ec2ServicePrincipal = "ec2.amazonaws.com"

// iamRoleNamePattern validates a user-supplied IAM role name
// (`--with-role`). Mirrors the IAM naming rules for roles:
// alphanumeric plus `+=,.@_-`, 1-64 chars.
var iamRoleNamePattern = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)

// instanceProfileSelection is the result of resolving `--with-admin` /
// `--with-role` into a concrete IAM role + instance profile pair. It
// carries the existence flags the preflight uses to decide which IAM
// write actions must be simulated (ADR 0010).
type instanceProfileSelection struct {
	// admin is true for `--with-admin` (shared nakama-managed admin
	// role), false for `--with-role` (user-supplied role).
	admin bool

	roleName   string
	roleARN    string
	roleExists bool

	profileName   string
	profileExists bool

	// profileHasRole is true when an existing instance profile already
	// contains the role, i.e. no IAM writes are needed at all. Set for
	// the `--with-role` reuse path (first profile returned by
	// ListInstanceProfilesForRole) and for an admin profile that
	// already wraps the admin role.
	profileHasRole bool
}

// profileARN returns the ARN of the selected instance profile in the
// given account (used as the SimulatePrincipalPolicy resource when the
// profile does not exist yet).
func (sel *instanceProfileSelection) profileARN(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:instance-profile/%s", accountID, sel.profileName)
}

// ensureInstanceProfileForCreate is the `--with-admin` / `--with-role`
// entry point called by CreateInstance right after the environment
// checks and BEFORE any resource is created:
//
//  1. resolve the target role/profile and their current existence,
//  2. preflight the caller's IAM permissions (fail-closed), and
//  3. idempotently provision the role/profile.
//
// On any error, no nakama resource has been created yet, satisfying
// the "check permissions before creating anything" requirement.
func (s *Service) ensureInstanceProfileForCreate(ctx context.Context, in CreateInstanceInput) (*instanceProfileSelection, error) {
	sel, err := s.resolveInstanceProfileSelection(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.preflightInstanceRolePermissions(ctx, sel); err != nil {
		return nil, err
	}
	if err := s.provisionInstanceProfile(ctx, sel); err != nil {
		return nil, err
	}
	return sel, nil
}

// resolveInstanceProfileSelection maps the create input to a concrete
// role/profile selection without performing any IAM writes.
func (s *Service) resolveInstanceProfileSelection(ctx context.Context, in CreateInstanceInput) (*instanceProfileSelection, error) {
	if in.WithAdmin {
		slog.Warn("create: --with-admin attaches AdministratorAccess to the instance; " +
			"anyone who can reach the code-server through CloudFront and the WAF allowlist " +
			"effectively holds administrator access to this AWS account")
		return s.resolveAdminProfileSelection(ctx)
	}
	return s.resolveCustomProfileSelection(ctx, in.WithRole)
}

// resolveAdminProfileSelection inspects the shared admin role/profile
// (nakama-instance-role-admin / nakama-instance-profile-admin).
func (s *Service) resolveAdminProfileSelection(ctx context.Context) (*instanceProfileSelection, error) {
	sel := &instanceProfileSelection{
		admin:       true,
		roleName:    naming.AdminInstanceRole(),
		profileName: naming.AdminInstanceProfile(),
	}

	getRole, err := s.clients.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(sel.roleName)})
	switch {
	case err == nil:
		if getRole.Role == nil || getRole.Role.Arn == nil {
			return nil, errors.New("GetRole returned nil")
		}
		sel.roleExists = true
		sel.roleARN = *getRole.Role.Arn
	case awshelper.IsAPIError(err, "NoSuchEntity"):
		sel.roleARN = fmt.Sprintf("arn:aws:iam::%s:role/%s", s.clients.AccountID, sel.roleName)
	default:
		return nil, fmt.Errorf("GetRole(%s): %w", sel.roleName, err)
	}

	profile, err := s.clients.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(sel.profileName),
	})
	switch {
	case err == nil:
		if profile.InstanceProfile == nil {
			return nil, errors.New("GetInstanceProfile returned nil")
		}
		sel.profileExists = true
		sel.profileHasRole = profileContainsRole(profile.InstanceProfile.Roles, sel.roleName)
	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// created during provisioning
	default:
		return nil, fmt.Errorf("GetInstanceProfile(%s): %w", sel.profileName, err)
	}
	return sel, nil
}

// resolveCustomProfileSelection validates the user-supplied role
// (existence + EC2 trust policy), warns best-effort when the role
// lacks SSM permissions, and picks the instance profile to use:
// the first existing profile that already contains the role, or a
// new nakama-managed profile named after the role.
func (s *Service) resolveCustomProfileSelection(ctx context.Context, roleName string) (*instanceProfileSelection, error) {
	if !iamRoleNamePattern.MatchString(roleName) {
		return nil, fmt.Errorf("invalid IAM role name %q: must match %s",
			roleName, iamRoleNamePattern.String())
	}

	sel := &instanceProfileSelection{roleName: roleName}

	getRole, err := s.clients.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	switch {
	case err == nil:
		if getRole.Role == nil || getRole.Role.Arn == nil {
			return nil, errors.New("GetRole returned nil")
		}
		sel.roleExists = true
		sel.roleARN = *getRole.Role.Arn
	case awshelper.IsAPIError(err, "NoSuchEntity"):
		return nil, fmt.Errorf("IAM role %q not found; --with-role requires an existing role", roleName)
	default:
		return nil, fmt.Errorf("GetRole(%s): %w", roleName, err)
	}

	if err := validateEC2TrustPolicy(aws.ToString(getRole.Role.AssumeRolePolicyDocument)); err != nil {
		return nil, fmt.Errorf("IAM role %q cannot be used as an EC2 instance role: %w "+
			"(the trust policy must allow sts:AssumeRole for the %s service principal)",
			roleName, err, ec2ServicePrincipal)
	}

	// Best-effort: warn when the role carries neither
	// AmazonSSMManagedInstanceCore nor AdministratorAccess. Without SSM
	// permissions, `nakama aws connect`/`update` (Session Manager, Run
	// Command) and the userdata password fetch will not work.
	s.warnIfRoleLacksCorePolicies(ctx, roleName)

	// Reuse the first instance profile that already contains the role.
	listOut, err := s.clients.IAM.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		return nil, fmt.Errorf("ListInstanceProfilesForRole(%s): %w", roleName, err)
	}
	if len(listOut.InstanceProfiles) > 0 && listOut.InstanceProfiles[0].InstanceProfileName != nil {
		sel.profileName = *listOut.InstanceProfiles[0].InstanceProfileName
		sel.profileExists = true
		sel.profileHasRole = true
		slog.Info("iam: reusing existing instance profile for role",
			"role", roleName, "profile", sel.profileName)
		return sel, nil
	}

	// No profile wraps the role yet: use (and later create if missing)
	// the nakama-managed profile named after the role.
	sel.profileName = naming.CustomInstanceProfile(roleName)
	profile, err := s.clients.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(sel.profileName),
	})
	switch {
	case err == nil:
		if profile.InstanceProfile == nil {
			return nil, errors.New("GetInstanceProfile returned nil")
		}
		sel.profileExists = true
		sel.profileHasRole = profileContainsRole(profile.InstanceProfile.Roles, roleName)
	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// created during provisioning
	default:
		return nil, fmt.Errorf("GetInstanceProfile(%s): %w", sel.profileName, err)
	}
	return sel, nil
}

// warnIfRoleLacksCorePolicies emits a slog.Warn when the role has
// neither AmazonSSMManagedInstanceCore nor AdministratorAccess
// attached. Errors are logged (not returned): this is a best-effort
// heads-up, never a blocker.
func (s *Service) warnIfRoleLacksCorePolicies(ctx context.Context, roleName string) {
	var marker *string
	for {
		out, err := s.clients.IAM.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{
			RoleName: aws.String(roleName),
			Marker:   marker,
		})
		if err != nil {
			slog.Warn("iam: could not list attached policies to verify SSM permissions (continuing)",
				"role", roleName, "error", err)
			return
		}
		for _, p := range out.AttachedPolicies {
			switch aws.ToString(p.PolicyArn) {
			case ssmCoreManagedPolicyARN, adminManagedPolicyARN:
				return
			}
		}
		if !out.IsTruncated || out.Marker == nil {
			break
		}
		marker = out.Marker
	}
	slog.Warn("iam: role has neither AmazonSSMManagedInstanceCore nor AdministratorAccess attached; "+
		"SSM-based features (nakama aws connect/update) and the userdata password fetch may not work",
		"role", roleName)
}

// preflightInstanceRolePermissions verifies via SimulatePrincipalPolicy
// that the calling principal may pass (and, where needed, create) the
// selected role/profile. It is fail-closed: any denied action and any
// failure to run the simulation aborts the create before resources are
// made (ADR 0010).
func (s *Service) preflightInstanceRolePermissions(ctx context.Context, sel *instanceProfileSelection) error {
	sourceARN, err := policySourceARNFromCaller(s.clients.CallerARN)
	if err != nil {
		return fmt.Errorf("preflight permission check could not run: %w; "+
			"nothing was created", err)
	}

	type simGroup struct {
		actions        []string
		resourceARN    string
		contextEntries []iamtypes.ContextEntry
	}
	groups := []simGroup{{
		actions:     []string{"iam:PassRole"},
		resourceARN: sel.roleARN,
		contextEntries: []iamtypes.ContextEntry{{
			ContextKeyName:   aws.String("iam:PassedToService"),
			ContextKeyType:   iamtypes.ContextKeyTypeEnumString,
			ContextKeyValues: []string{ec2ServicePrincipal},
		}},
	}}
	if sel.admin && !sel.roleExists {
		groups = append(groups, simGroup{
			actions:     []string{"iam:CreateRole", "iam:AttachRolePolicy"},
			resourceARN: sel.roleARN,
		})
	}
	if !sel.profileExists {
		groups = append(groups, simGroup{
			actions:     []string{"iam:CreateInstanceProfile", "iam:AddRoleToInstanceProfile"},
			resourceARN: sel.profileARN(s.clients.AccountID),
		})
	}

	var denied []string
	for _, g := range groups {
		var marker *string
		for {
			out, err := s.clients.IAM.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
				PolicySourceArn: aws.String(sourceARN),
				ActionNames:     g.actions,
				ResourceArns:    []string{g.resourceARN},
				ContextEntries:  g.contextEntries,
				Marker:          marker,
			})
			if err != nil {
				return fmt.Errorf("preflight permission check could not run: SimulatePrincipalPolicy: %w; "+
					"the calling principal needs iam:SimulatePrincipalPolicy to use --with-admin/--with-role; "+
					"nothing was created", err)
			}
			for _, r := range out.EvaluationResults {
				if r.EvalDecision != iamtypes.PolicyEvaluationDecisionTypeAllowed {
					denied = append(denied, aws.ToString(r.EvalActionName))
				}
			}
			if !out.IsTruncated || out.Marker == nil {
				break
			}
			marker = out.Marker
		}
	}
	if len(denied) > 0 {
		return fmt.Errorf("insufficient permissions to attach IAM role %q to the instance: "+
			"denied actions: %s; nothing was created", sel.roleName, strings.Join(denied, ", "))
	}
	return nil
}

// policySourceARNFromCaller converts the STS caller ARN into a
// SimulatePrincipalPolicy PolicySourceArn. Assumed-role session ARNs
// (arn:<partition>:sts::<acct>:assumed-role/<RoleName>/<Session>) are
// not valid policy sources and are rewritten to the underlying IAM
// role ARN (arn:<partition>:iam::<acct>:role/<RoleName>). IAM user and
// root ARNs pass through unchanged.
func policySourceARNFromCaller(callerARN string) (string, error) {
	if callerARN == "" {
		return "", errors.New("caller ARN is unavailable (STS GetCallerIdentity returned no ARN)")
	}
	parts := strings.SplitN(callerARN, ":", 6)
	if len(parts) != 6 {
		return "", fmt.Errorf("malformed caller ARN %q", callerARN)
	}
	if parts[2] != "sts" || !strings.HasPrefix(parts[5], "assumed-role/") {
		return callerARN, nil
	}
	seg := strings.Split(parts[5], "/")
	if len(seg) < 3 || seg[1] == "" {
		return "", fmt.Errorf("malformed assumed-role ARN %q", callerARN)
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", parts[1], parts[4], seg[1]), nil
}

// provisionInstanceProfile performs the idempotent IAM writes for the
// selection. For `--with-role` reuse (profile already contains the
// role) it is a no-op; the user's role is never modified.
func (s *Service) provisionInstanceProfile(ctx context.Context, sel *instanceProfileSelection) error {
	if sel.admin {
		arn, err := s.upsertAdminInstanceRole(ctx)
		if err != nil {
			return err
		}
		sel.roleARN = arn
		return s.upsertNamedInstanceProfile(ctx, sel.profileName, sel.roleName)
	}
	if sel.profileHasRole {
		return nil
	}
	return s.upsertNamedInstanceProfile(ctx, sel.profileName, sel.roleName)
}

// upsertAdminInstanceRole creates (or reuses) the shared admin
// instance role and (re-)attaches AdministratorAccess. Same
// lookup-then-create pattern as upsertInstanceRole in init_iam.go;
// the shared base role nakama-instance-role is never touched.
func (s *Service) upsertAdminInstanceRole(ctx context.Context) (string, error) {
	roleName := naming.AdminInstanceRole()

	getOut, err := s.clients.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	switch {
	case err == nil:
		if getOut.Role == nil || getOut.Role.Arn == nil {
			return "", errors.New("GetRole returned nil")
		}
		slog.Info("iam role: reusing existing", "name", roleName, "arn", *getOut.Role.Arn)
		// Best-effort re-attach so manual detachments are healed.
		if err := s.attachAdminRolePolicy(ctx, roleName); err != nil {
			return "", err
		}
		return *getOut.Role.Arn, nil

	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// fall through to creation
	default:
		return "", fmt.Errorf("GetRole(%s): %w", roleName, err)
	}

	tags := tagging.ForShared(tagging.ComponentIAM, roleName)
	c, err := s.clients.IAM.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(ec2AssumeRolePolicy),
		Description:              aws.String("nakama-managed EC2 instance role with AdministratorAccess (--with-admin)"),
		Tags:                     awshelper.IAMTags(tags),
	})
	if err != nil {
		return "", fmt.Errorf("CreateRole: %w", err)
	}
	if c.Role == nil || c.Role.Arn == nil {
		return "", errors.New("CreateRole returned nil")
	}
	slog.Info("iam role: created", "name", roleName, "arn", *c.Role.Arn)

	if err := s.attachAdminRolePolicy(ctx, roleName); err != nil {
		return "", err
	}
	return *c.Role.Arn, nil
}

func (s *Service) attachAdminRolePolicy(ctx context.Context, roleName string) error {
	if _, err := s.clients.IAM.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(roleName),
		PolicyArn: aws.String(adminManagedPolicyARN),
	}); err != nil {
		return fmt.Errorf("AttachRolePolicy(%s): %w", adminManagedPolicyARN, err)
	}
	return nil
}

// upsertNamedInstanceProfile creates (or reuses) the given instance
// profile and ensures the role is attached, tolerating LimitExceeded
// for the already-attached case (same pattern as init_iam.go).
func (s *Service) upsertNamedInstanceProfile(ctx context.Context, profileName, roleName string) error {
	getOut, err := s.clients.IAM.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	switch {
	case err == nil:
		if getOut.InstanceProfile == nil {
			return errors.New("GetInstanceProfile returned nil")
		}
		slog.Info("iam instance profile: reusing existing", "name", profileName)
		return s.attachRoleToProfileIfNeeded(ctx, profileName, roleName, getOut.InstanceProfile.Roles)

	case awshelper.IsAPIError(err, "NoSuchEntity"):
		// create below
	default:
		return fmt.Errorf("GetInstanceProfile(%s): %w", profileName, err)
	}

	tags := tagging.ForShared(tagging.ComponentIAM, profileName)
	c, err := s.clients.IAM.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
		Tags:                awshelper.IAMTags(tags),
	})
	if err != nil {
		return fmt.Errorf("CreateInstanceProfile: %w", err)
	}
	if c.InstanceProfile == nil {
		return errors.New("CreateInstanceProfile returned nil")
	}
	slog.Info("iam instance profile: created", "name", profileName)

	return s.attachRoleToProfileIfNeeded(ctx, profileName, roleName, nil)
}

// profileContainsRole reports whether the given instance-profile role
// list already includes roleName.
func profileContainsRole(roles []iamtypes.Role, roleName string) bool {
	for _, r := range roles {
		if r.RoleName != nil && *r.RoleName == roleName {
			return true
		}
	}
	return false
}

// trustPolicyDocument is the subset of an IAM trust policy nakama
// inspects to decide whether EC2 may assume a user-supplied role.
type trustPolicyDocument struct {
	Statement trustPolicyStatements `json:"Statement"`
}

// trustPolicyStatements accepts both the array form and the
// single-object form IAM allows for "Statement".
type trustPolicyStatements []trustPolicyStatement

// UnmarshalJSON accepts both a JSON array of statements and a single
// statement object.
func (s *trustPolicyStatements) UnmarshalJSON(b []byte) error {
	var list []trustPolicyStatement
	if err := json.Unmarshal(b, &list); err == nil {
		*s = list
		return nil
	}
	var single trustPolicyStatement
	if err := json.Unmarshal(b, &single); err != nil {
		return err
	}
	*s = trustPolicyStatements{single}
	return nil
}

type trustPolicyStatement struct {
	Effect    string          `json:"Effect"`
	Action    stringOrSlice   `json:"Action"`
	Principal *trustPrincipal `json:"Principal"`
}

type trustPrincipal struct {
	Service stringOrSlice `json:"Service"`
}

// stringOrSlice unmarshals a JSON value that IAM policies allow to be
// either a single string or an array of strings.
type stringOrSlice []string

// UnmarshalJSON accepts both a JSON string and a JSON array of strings.
func (v *stringOrSlice) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*v = stringOrSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*v = many
	return nil
}

func (v stringOrSlice) contains(want string) bool {
	for _, s := range v {
		if s == want {
			return true
		}
	}
	return false
}

// validateEC2TrustPolicy parses the (URL-encoded, per the IAM API)
// AssumeRolePolicyDocument and errors out unless at least one
// statement allows sts:AssumeRole for the ec2.amazonaws.com service
// principal.
func validateEC2TrustPolicy(encodedDoc string) error {
	if encodedDoc == "" {
		return errors.New("role has no trust policy document")
	}
	decoded, err := url.QueryUnescape(encodedDoc)
	if err != nil {
		return fmt.Errorf("decode trust policy: %w", err)
	}
	var doc trustPolicyDocument
	if err := json.Unmarshal([]byte(decoded), &doc); err != nil {
		return fmt.Errorf("parse trust policy: %w", err)
	}
	for _, st := range doc.Statement {
		if st.Effect != "Allow" {
			continue
		}
		if !st.Action.contains("sts:AssumeRole") {
			continue
		}
		if st.Principal != nil && st.Principal.Service.contains(ec2ServicePrincipal) {
			return nil
		}
	}
	return errors.New("trust policy does not allow EC2 to assume the role")
}
