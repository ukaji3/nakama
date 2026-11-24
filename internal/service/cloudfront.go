package service

// CloudFront access per instance (Issue #120, ADR 0028).
//
// The browser reaches code-server and Kiro Crew over SSM port
// forwarding by default (ADR 0027), so an instance has no internet
// entry path unless one is asked for: `nakama aws create --cloudfront`
// builds it with the instance, and `nakama aws cloudfront add` /
// `delete` add it to, and remove it from, an existing instance:
//
//	browser ─ https://<distribution>.cloudfront.net  (shared WAF web ACL)
//	  → VPC origin (CloudFront's ENI in the private subnet)
//	  → instance SG: 8080/tcp from the CloudFront managed prefix list
//	  → code-server:8080
//
// Both commands can be re-run: every resource is recorded in SSM the
// moment it exists, every step checks what exists first, and a step
// that finds its resource gone moves on.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/ukaji3/nakama/internal/awshelper"
	"github.com/ukaji3/nakama/internal/naming"
)

// cloudFrontMeta is the instance metadata the CloudFront operations
// read from /nakama/<name>/meta/.
type cloudFrontMeta struct {
	name               string
	instanceID         string
	kind               string
	sgID               string
	vpcOriginID        string
	distributionID     string
	distributionDomain string
	crewID             string
	crewDomain         string
	// deleting is set while a `cloudfront delete` is under way, and
	// stays set when one stopped part way (ssmCloudFrontDeletingPath).
	deleting bool
}

// ssmCloudFrontDeletingPath marks an instance whose CloudFront a
// `cloudfront delete` is taking apart. It is written before the first
// step and removed with the other records after the last one, so that
// `cloudfront add` can refuse to build on a half-deleted entry path:
// CloudFront itself does not say so (a VPC origin being deleted
// reports "Deploying" until it is gone).
func ssmCloudFrontDeletingPath(name string) string {
	return naming.SSMPath(name, "meta", "cloudfront-deleting")
}

func (m *cloudFrontMeta) resources() cloudFrontResources {
	return cloudFrontResources{name: m.name, distributionID: m.distributionID, crewID: m.crewID, vpcOriginID: m.vpcOriginID}
}

// readCloudFrontMeta reads the metadata of an existing instance. A read
// failure is an error: treating it as "not recorded" could create a
// second VPC origin or skip a resource that still exists.
func (s *Service) readCloudFrontMeta(ctx context.Context, name string) (*cloudFrontMeta, error) {
	if err := ValidateInstanceName(name); err != nil {
		return nil, err
	}
	m := &cloudFrontMeta{name: name}
	var deleting string
	for _, f := range []struct {
		dst  *string
		path string
	}{
		{&m.instanceID, ssmInstanceIDPath(name)},
		{&m.kind, ssmKindPath(name)},
		{&m.sgID, ssmSGIDPath(name)},
		{&m.vpcOriginID, ssmVPCOriginIDPath(name)},
		{&m.distributionID, ssmDistributionIDPath(name)},
		{&m.distributionDomain, ssmDistributionDomainPath(name)},
		{&m.crewID, ssmKirocrewDistributionIDPath(name)},
		{&m.crewDomain, ssmKirocrewDistributionDomainPath(name)},
		{&deleting, ssmCloudFrontDeletingPath(name)},
	} {
		v, err := s.ssmGetOptional(ctx, f.path)
		if err != nil {
			return nil, err
		}
		*f.dst = v
	}
	m.deleting = deleting != ""
	if m.instanceID == "" {
		return nil, fmt.Errorf("nakama instance %q not found (no /nakama/%s/meta/instance-id)", name, name)
	}
	return m, nil
}

// cloudFrontMetaKeys are the parameters `cloudfront delete` removes,
// its own marker last.
func cloudFrontMetaKeys(name string) []string {
	return []string{
		ssmDistributionIDPath(name),
		ssmDistributionDomainPath(name),
		ssmVPCOriginIDPath(name),
		ssmKirocrewDistributionIDPath(name),
		ssmKirocrewDistributionDomainPath(name),
		ssmCloudFrontDeletingPath(name),
	}
}

// --- add ------------------------------------------------------------------

// AddCloudFrontOutput is the result of AddCloudFront.
type AddCloudFrontOutput struct {
	Name               string `json:"name"`
	VPCOriginID        string `json:"vpc_origin_id"`
	DistributionID     string `json:"distribution_id"`
	DistributionDomain string `json:"distribution_domain"`
	URL                string `json:"url"`
	// AlreadyPresent is true when the VPC origin and the distribution
	// existed before this run, so nothing was created.
	AlreadyPresent bool `json:"already_present,omitempty"`
}

// AddCloudFront gives an existing code-server instance the CloudFront
// entry path `create --cloudfront` builds: the security group's inbound
// rule from the CloudFront managed prefix list, the VPC origin and the
// distribution behind the shared WAF web ACL. It waits until the
// distribution is Deployed. A run that stopped part way is continued;
// on an instance that has everything already, nothing is created.
//
// The instance must be running: CloudFront accepts only a fully
// deployed origin resource for a VPC origin. Kiro Crew web access is
// not part of this; EnableKiroCrewWeb builds it on top (`cloudfront add --kirocrew`).
func (s *Service) AddCloudFront(ctx context.Context, name string) (*AddCloudFrontOutput, error) {
	m, err := s.readCloudFrontMeta(ctx, name)
	if err != nil {
		return nil, err
	}
	if m.kind == instanceKindBare {
		return nil, fmt.Errorf("%q is a bare instance: it has no code-server to put behind CloudFront", name)
	}
	if err := s.requireInitialized(ctx); err != nil {
		return nil, err
	}
	state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
	if err != nil {
		return nil, err
	}
	if state != "running" {
		return nil, fmt.Errorf("instance %q is %s; start it first with `nakama aws start %s` and retry `nakama aws cloudfront add`",
			name, orUnknown(state), name)
	}
	if m.sgID == "" {
		return nil, fmt.Errorf("instance %q has no security group recorded (/nakama/%s/meta/sg-id)", name, name)
	}
	if m.deleting {
		return nil, fmt.Errorf("a `nakama aws cloudfront delete %s` stopped part way, so the CloudFront of %q is half removed; "+
			"run `nakama aws cloudfront delete %s` to finish it, then `nakama aws cloudfront add %s` again",
			name, name, name, name)
	}

	ic, err := s.existingInstanceCloudFront(ctx, m)
	if err != nil {
		return nil, err
	}
	already := ic.VPCOriginID != "" && ic.DistributionID != ""

	pre, err := s.resolveCloudFrontPrereqs(ctx)
	if err != nil {
		return nil, err
	}
	slog.Info("cloudfront: adding", "name", name, "vpc_origin", ic.VPCOriginID, "distribution", ic.DistributionID)
	if err := s.ensureSGIngress(ctx, m.sgID, pre.prefixListID); err != nil {
		return nil, fmt.Errorf("security group: %w", err)
	}
	if err := s.ensureInstanceCloudFront(ctx, name, m.instanceID, pre.wafWebACLARN, &ic); err != nil {
		return nil, err
	}
	if err := s.waitDistributionDeployed(ctx, ic.DistributionID); err != nil {
		return nil, fmt.Errorf("wait distribution: %w", err)
	}
	slog.Info("cloudfront: added", "name", name, "distribution", ic.DistributionID, "domain", ic.DistributionDomain)
	return &AddCloudFrontOutput{
		Name:               name,
		VPCOriginID:        ic.VPCOriginID,
		DistributionID:     ic.DistributionID,
		DistributionDomain: ic.DistributionDomain,
		URL:                "https://" + ic.DistributionDomain,
		AlreadyPresent:     already,
	}, nil
}

// existingInstanceCloudFront checks the recorded VPC origin and
// distribution against CloudFront and returns the ones AddCloudFront
// can build on. One that no longer exists is dropped, so it is created
// again. A disabled distribution, or a combination nakama never
// builds, is an error that says how to get out of it.
func (s *Service) existingInstanceCloudFront(ctx context.Context, m *cloudFrontMeta) (instanceCloudFront, error) {
	ic := instanceCloudFront{VPCOriginID: m.vpcOriginID, DistributionID: m.distributionID, DistributionDomain: m.distributionDomain}

	if ic.VPCOriginID != "" {
		_, err := s.clients.CloudFront.GetVpcOrigin(ctx, &cloudfront.GetVpcOriginInput{Id: aws.String(ic.VPCOriginID)})
		switch {
		case awshelper.IsAPIError(err, "EntityNotFound", "NoSuchResource"):
			slog.Warn("cloudfront: the recorded VPC origin is gone; creating a new one", "id", ic.VPCOriginID)
			ic.VPCOriginID = ""
		case err != nil:
			return ic, fmt.Errorf("GetVpcOrigin(%s): %w", ic.VPCOriginID, err)
		}
	}

	if ic.DistributionID != "" {
		out, err := s.clients.CloudFront.GetDistribution(ctx, &cloudfront.GetDistributionInput{Id: aws.String(ic.DistributionID)})
		switch {
		case awshelper.IsAPIError(err, "NoSuchDistribution"):
			slog.Warn("cloudfront: the recorded distribution is gone; creating a new one", "id", ic.DistributionID)
			ic.DistributionID, ic.DistributionDomain = "", ""
		case err != nil:
			return ic, fmt.Errorf("GetDistribution(%s): %w", ic.DistributionID, err)
		case out.Distribution == nil:
			return ic, fmt.Errorf("GetDistribution(%s) returned nil", ic.DistributionID)
		default:
			d := out.Distribution
			// nakama disables a distribution only to delete it.
			if d.DistributionConfig != nil && !aws.ToBool(d.DistributionConfig.Enabled) {
				return ic, fmt.Errorf("the distribution %s of %q is disabled, the first step of deleting it; "+
					"run `nakama aws cloudfront delete %s` to finish removing it, then `nakama aws cloudfront add %s` again",
					ic.DistributionID, m.name, m.name, m.name)
			}
			// The domain is CloudFront's, not a guess: it is what the
			// output and `code --internet` use.
			if dom := aws.ToString(d.DomainName); dom != "" && dom != ic.DistributionDomain {
				ic.DistributionDomain = dom
				if err := s.writeInstanceMeta(ctx, m.name, map[string]string{ssmDistributionDomainPath(m.name): dom}); err != nil {
					return ic, fmt.Errorf("write instance metadata: %w", err)
				}
			}
		}
	}

	// A distribution needs its VPC origin, and the Kiro Crew distribution
	// sits on the code-server one. nakama never leaves either half on
	// its own; it is cleared with `cloudfront delete`, not guessed at.
	if ic.DistributionID != "" && ic.VPCOriginID == "" {
		return ic, fmt.Errorf("the distribution %s of %q exists but its VPC origin does not; "+
			"run `nakama aws cloudfront delete %s`, then `nakama aws cloudfront add %s`",
			ic.DistributionID, m.name, m.name, m.name)
	}
	if ic.DistributionID == "" && m.crewID != "" {
		return ic, fmt.Errorf("%q has a Kiro Crew distribution (%s) but no code-server distribution; "+
			"run `nakama aws cloudfront delete %s`, then `nakama aws cloudfront add %s`",
			m.name, m.crewID, m.name, m.name)
	}
	return ic, nil
}

// --- delete ---------------------------------------------------------------

// DeleteCloudFrontPlan is what DeleteCloudFront removes, read with
// read-only calls so that the CLI can show it before asking.
type DeleteCloudFrontPlan struct {
	Name                   string `json:"name"`
	InstanceID             string `json:"instance_id"`
	State                  string `json:"state"`
	Bare                   bool   `json:"bare,omitempty"`
	SecurityGroupID        string `json:"security_group_id,omitempty"`
	VPCOriginID            string `json:"vpc_origin_id,omitempty"`
	DistributionID         string `json:"distribution_id,omitempty"`
	DistributionDomain     string `json:"distribution_domain,omitempty"`
	KiroCrewDistributionID string `json:"kirocrew_distribution_id,omitempty"`
	KiroCrewDomain         string `json:"kirocrew_domain,omitempty"`

	meta *cloudFrontMeta
}

// HasCloudFront reports whether the instance has any CloudFront
// resource recorded. Without one, DeleteCloudFront only removes a
// leftover inbound rule from the security group.
func (p *DeleteCloudFrontPlan) HasCloudFront() bool {
	return p.VPCOriginID != "" || p.DistributionID != "" || p.KiroCrewDistributionID != ""
}

// PlanDeleteCloudFront reads what `nakama aws cloudfront delete` would
// remove. It changes nothing. A bare instance is accepted: bare
// instances created before ADR 0028 carry the inbound rule too.
func (s *Service) PlanDeleteCloudFront(ctx context.Context, name string) (*DeleteCloudFrontPlan, error) {
	m, err := s.readCloudFrontMeta(ctx, name)
	if err != nil {
		return nil, err
	}
	state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
	if err != nil {
		return nil, err
	}
	return &DeleteCloudFrontPlan{
		Name: name, InstanceID: m.instanceID, State: state, Bare: m.kind == instanceKindBare, SecurityGroupID: m.sgID,
		VPCOriginID: m.vpcOriginID, DistributionID: m.distributionID, DistributionDomain: m.distributionDomain,
		KiroCrewDistributionID: m.crewID, KiroCrewDomain: m.crewDomain,
		meta: m,
	}, nil
}

// DeleteCloudFrontOutput is the result of DeleteCloudFront.
type DeleteCloudFrontOutput struct {
	Name                   string `json:"name"`
	DistributionID         string `json:"distribution_id,omitempty"`
	KiroCrewDistributionID string `json:"kirocrew_distribution_id,omitempty"`
	VPCOriginID            string `json:"vpc_origin_id,omitempty"`
	// IngressRuleRemoved is false when the security group had no
	// CloudFront inbound rule left.
	IngressRuleRemoved bool `json:"ingress_rule_removed"`
	// InstanceSkipped is true when Kiro Crew web access was enabled but
	// the instance was not running, so its nginx site and code-server
	// proxy settings were left in place (harmless without the
	// distribution; `nakama aws cloudfront delete --kirocrew` removes them once the
	// instance runs).
	InstanceSkipped bool `json:"instance_skipped,omitempty"`
}

// DeleteCloudFront removes an instance's CloudFront entry path, Kiro
// Crew's included, in this order:
//
//  0. a marker in SSM, so that `cloudfront add` refuses to build on a
//     half-removed entry path until a delete has finished
//  1. the security group's CloudFront inbound rule: from here on
//     nothing reaches the instance through CloudFront, even if a later
//     step fails
//  2. the Kiro Crew distribution, the code-server distribution, the
//     redirect function and the VPC origin (deleteCloudFrontResources,
//     shared with `nakama aws delete`)
//  3. with Kiro Crew web access, its instance side (nginx site,
//     code-server proxy settings) when the instance is running
//  4. the records and the marker, last, so that a re-run still finds
//     everything
//
// The instance itself is not touched and stays reachable over SSM.
func (s *Service) DeleteCloudFront(ctx context.Context, p *DeleteCloudFrontPlan) (*DeleteCloudFrontOutput, error) {
	if p == nil || p.meta == nil {
		return nil, errors.New("cloudfront delete: no plan")
	}
	m := p.meta
	out := &DeleteCloudFrontOutput{
		Name: m.name, DistributionID: m.distributionID, KiroCrewDistributionID: m.crewID, VPCOriginID: m.vpcOriginID,
	}
	slog.Info("cloudfront: deleting", "name", m.name, "distribution", m.distributionID,
		"kirocrew_distribution", m.crewID, "vpc_origin", m.vpcOriginID, "sg", m.sgID)

	// 0. Mark the instance first: until the records go at the end,
	// `cloudfront add` refuses to build on what is half removed.
	if p.HasCloudFront() {
		if err := s.writeInstanceMeta(ctx, m.name, map[string]string{ssmCloudFrontDeletingPath(m.name): "true"}); err != nil {
			return nil, fmt.Errorf("write instance metadata: %w", err)
		}
	}

	// 1. Cut the path first.
	if m.sgID == "" {
		slog.Warn("cloudfront: no security group recorded; its inbound rule was not checked", "name", m.name)
	} else {
		prefixListID, err := s.cloudFrontPrefixListID(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve CloudFront prefix list: %w", err)
		}
		if out.IngressRuleRemoved, err = s.revokeSGIngress(ctx, m.sgID, prefixListID); err != nil {
			return nil, fmt.Errorf("security group: %w", err)
		}
	}

	// 2. The CloudFront resources.
	if err := s.deleteCloudFrontResources(ctx, m.resources(), false); err != nil {
		return nil, err
	}

	// 3. Kiro Crew's instance side. The state is read again: the plan
	// may be minutes old by now (the confirmation prompt).
	if m.crewID != "" {
		state, _, err := s.fetchInstanceStateAndType(ctx, m.instanceID)
		if err != nil {
			return nil, err
		}
		if state == "running" {
			stdout, runErr := s.runRemoteScript(ctx, m.instanceID, kirocrewDisableScript(),
				"nakama: cloudfront delete "+m.name, kirocrewRemoteTimeout)
			if err := parseKirocrewScriptResult(stdout, runErr); err != nil {
				return nil, fmt.Errorf("remove the Kiro Crew proxy settings on the instance: %w", err)
			}
		} else {
			out.InstanceSkipped = true
			slog.Warn("cloudfront: instance is not running; left its Kiro Crew nginx site and code-server proxy-domain in place",
				"instance", m.instanceID, "state", orUnknown(state))
		}
	}

	// 4. The records, now that nothing they point at is left. This runs
	// even when the plan found no CloudFront: it also clears what a
	// record-only leftover would otherwise keep, such as a marker without
	// the IDs it guarded, which would make `cloudfront add` refuse
	// forever. Names that do not exist are not an error.
	if _, err := s.clients.SSM.DeleteParameters(ctx, &ssm.DeleteParametersInput{Names: cloudFrontMetaKeys(m.name)}); err != nil {
		return nil, fmt.Errorf("DeleteParameters(cloudfront meta): %w", err)
	}
	slog.Info("cloudfront: deleted", "name", m.name)
	return out, nil
}
