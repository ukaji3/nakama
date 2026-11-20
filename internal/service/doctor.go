package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/cliproxy"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/profilestore"
	"github.com/ukaji3/nakama/internal/version"
)

// CheckStatus is the outcome of one diagnostic check.
type CheckStatus string

const (
	// CheckOK means the check passed.
	CheckOK CheckStatus = "ok"
	// CheckWarn means nakama works but something needs attention.
	CheckWarn CheckStatus = "warn"
	// CheckFail means nakama cannot work until this is fixed.
	CheckFail CheckStatus = "fail"
	// CheckSkip means the check could not run (a prerequisite check
	// failed); it never affects the exit code on its own.
	CheckSkip CheckStatus = "skip"
)

// DoctorCheck is one line of the diagnosis.
type DoctorCheck struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
	Advice string      `json:"advice,omitempty"`
}

// DoctorReport aggregates the checks. Counts make the exit code and the
// summary line trivial for the CLI.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OKs    int           `json:"ok_count"`
	Warns  int           `json:"warn_count"`
	Fails  int           `json:"fail_count"`
	Skips  int           `json:"skip_count"`
}

// add appends a check and updates the counters.
func (r *DoctorReport) add(c DoctorCheck) {
	r.Checks = append(r.Checks, c)
	switch c.Status {
	case CheckOK:
		r.OKs++
	case CheckWarn:
		r.Warns++
	case CheckFail:
		r.Fails++
	case CheckSkip:
		r.Skips++
	}
}

// DoctorOptions parameterizes Diagnose. Clients may be nil when
// credential resolution failed: the local checks (proxy, TLS) still run,
// which is exactly the situation where the diagnosis is most useful.
type DoctorOptions struct {
	// Clients is the resolved AWS client set, or nil when resolution
	// failed.
	Clients *awsx.Clients
	// ClientErr is the credential/client resolution error, if any.
	ClientErr error

	// detect overrides proxy/TLS detection in tests.
	detect func(ctx context.Context) (*cliproxy.Detection, error)
	// activeProfile overrides the profile lookup in tests.
	activeProfile func() (string, bool)
	// aclEntries overrides the WAF allowlist read in tests.
	aclEntries func(ctx context.Context) ([]ACLEntry, error)
	// publicIP overrides this machine's public IP lookup in tests.
	publicIP func(ctx context.Context) (string, error)
	// cloudFrontInstances overrides the lookup of the instances with
	// CloudFront in tests.
	cloudFrontInstances func(ctx context.Context) ([]string, error)
	// tunnelNames overrides the tunnel state listing in tests.
	tunnelNames func() ([]string, error)
	// tunnelStatus overrides the per-tunnel status probe in tests.
	tunnelStatus func(ctx context.Context, name string) (*TunnelStatus, error)
	// latestRelease overrides the latest-release check in tests.
	latestRelease func(ctx context.Context) version.CheckResult
	// egress overrides the egress inspection in tests.
	egress func(ctx context.Context) (*EgressStatus, error)
}

// Diagnose runs the read-only environment diagnosis behind
// `nakama aws doctor`: proxy resolution and TLS interception (reusing
// the cliproxy detector from ADR 0018), credential resolution, WAF
// allowlist membership for this machine's public IP, the egress of the
// private subnets (NAT instance or NAT gateway, ADR 0026), one
// capability line per feature derived from featurePermissionMatrix
// (permissions.go), the local tunnel state, and whether a newer nakama
// release is out (the GitHub Releases API). It performs no mutation of
// any kind and never returns credentials, tokens or passwords in the
// report.
func Diagnose(ctx context.Context, opts DoctorOptions) *DoctorReport {
	rep := &DoctorReport{}
	det := diagnoseProxyAndTLS(ctx, opts, rep)
	_ = det // the detection is only reported, not reused further

	if opts.ClientErr != nil {
		rep.add(DoctorCheck{
			Name:   "credentials",
			Status: CheckFail,
			Detail: fmt.Sprintf("could not resolve AWS credentials: %v", opts.ClientErr),
			Advice: "run `nakama aws login` (or `aws login`), or select a saved profile with `nakama aws profile load <name>`",
		})
		for _, name := range []string{"waf-allowlist", "egress", "capabilities"} {
			rep.add(DoctorCheck{Name: name, Status: CheckSkip, Detail: "skipped: no AWS credentials"})
		}
		diagnoseTunnels(ctx, opts, rep)
		diagnoseVersion(ctx, opts, rep)
		return rep
	}

	diagnoseCredentials(opts, rep)
	diagnoseWAF(ctx, opts, rep)
	diagnoseEgress(ctx, opts, rep)
	diagnoseCapabilities(ctx, opts, rep)
	diagnoseTunnels(ctx, opts, rep)
	diagnoseVersion(ctx, opts, rep)
	return rep
}

// diagnoseProxyAndTLS reports the resolved proxy and whether a
// TLS-intercepting proxy re-signs AWS traffic.
func diagnoseProxyAndTLS(ctx context.Context, opts DoctorOptions, rep *DoctorReport) *cliproxy.Detection {
	detect := opts.detect
	if detect == nil {
		detect = cliproxy.NewDetector().Detect
	}
	det, err := detect(ctx)
	if err != nil {
		var authErr *cliproxy.ProxyAuthError
		if errors.As(err, &authErr) {
			advice := "run a local authenticating proxy relay (px, Cntlm) and point HTTP(S)_PROXY at it"
			if authErr.SupportsBasic() {
				advice = "embed credentials in the proxy URL: HTTPS_PROXY=http://user:password@proxy:port"
			}
			rep.add(DoctorCheck{Name: "proxy", Status: CheckFail, Detail: authErr.Error(), Advice: advice})
		} else {
			rep.add(DoctorCheck{
				Name:   "proxy",
				Status: CheckFail,
				Detail: fmt.Sprintf("could not reach the AWS endpoint: %v", err),
				Advice: "check network connectivity and HTTP_PROXY/HTTPS_PROXY (Windows: the WinHTTP/PAC settings are auto-detected)",
			})
		}
		rep.add(DoctorCheck{Name: "tls", Status: CheckSkip, Detail: "skipped: the endpoint probe failed"})
		return nil
	}

	switch {
	case det.ProxyURL == "":
		rep.add(DoctorCheck{Name: "proxy", Status: CheckOK, Detail: "direct connection (no proxy configured)"})
	case det.ProxyFromEnv:
		rep.add(DoctorCheck{Name: "proxy", Status: CheckOK, Detail: det.ProxyURL + " (from environment variables)"})
	default:
		rep.add(DoctorCheck{Name: "proxy", Status: CheckOK, Detail: det.ProxyURL + " (auto-detected from the OS settings)"})
	}

	tls := DoctorCheck{Name: "tls", Status: CheckOK}
	if det.Intercepted {
		tls.Detail = fmt.Sprintf("TLS interception detected and trusted by the OS store (re-signed by %s)", det.RootSubject)
		if det.BundlePath != "" {
			tls.Advice = "the aws CLI needs this bundle: `nakama aws cli shell` sets AWS_CA_BUNDLE=" + det.BundlePath
		}
	} else {
		tls.Detail = "no TLS interception detected (public Amazon Trust Services chain)"
	}
	if det.CheckErr != nil {
		tls.Status = CheckWarn
		tls.Detail += fmt.Sprintf("; verification with the resolved settings failed: %v", det.CheckErr)
	}
	rep.add(tls)
	return det
}

// diagnoseCredentials reports which identity nakama operates as.
func diagnoseCredentials(opts DoctorOptions, rep *DoctorReport) {
	source := "default credential chain"
	lookup := opts.activeProfile
	if lookup == nil {
		lookup = activeProfileName
	}
	if name, ok := lookup(); ok {
		source = fmt.Sprintf("active nakama profile %q", name)
	}
	rep.add(DoctorCheck{
		Name:   "credentials",
		Status: CheckOK,
		Detail: fmt.Sprintf("account %s as %s (%s)", opts.Clients.AccountID, opts.Clients.CallerARN, source),
	})
}

// activeProfileName reports the active nakama profile, if any.
func activeProfileName() (string, bool) {
	data, err := profilestore.New().Load()
	if err != nil {
		return "", false
	}
	p, ok := data.ActiveProfile()
	if !ok {
		return "", false
	}
	if p.Alias != "" {
		return p.Alias, true
	}
	return p.AccountID, true
}

// diagnoseWAF reports whether this machine's public IP can reach
// code-server through the WAF allowlist. The allowlist matters only for
// instances with CloudFront (ADR 0028): without any, the check is
// skipped instead of warning about an allowlist nothing uses.
func diagnoseWAF(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	listACL := opts.aclEntries
	if listACL == nil {
		listACL = NewService(opts.Clients).ListACL
	}
	publicIP := opts.publicIP
	if publicIP == nil {
		publicIP = opts.Clients.IPDiscoverer.PublicIPv4
	}
	cloudFrontInstances := opts.cloudFrontInstances
	if cloudFrontInstances == nil {
		cloudFrontInstances = NewService(opts.Clients).cloudFrontInstanceNames
	}

	// "" when the lookup failed: the allowlist is checked as before,
	// since it may be in use.
	using := ""
	names, err := cloudFrontInstances(ctx)
	switch {
	case err != nil:
		slog.Debug("doctor: could not list the instances with CloudFront", "error", err)
	case len(names) == 0:
		rep.add(DoctorCheck{
			Name:   "waf-allowlist",
			Status: CheckSkip,
			Detail: "no instance has CloudFront, so the allowlist is not used " +
				"(open/kirocrew over SSM, connect, tunnel and ssh do not need it)",
		})
		return
	default:
		using = "; CloudFront on " + strings.Join(names, ", ")
	}

	entries, err := listACL(ctx)
	if err != nil {
		rep.add(DoctorCheck{
			Name:   "waf-allowlist",
			Status: CheckWarn,
			Detail: fmt.Sprintf("could not read the WAF IP set: %v", err),
			Advice: "run `nakama aws init` if the shared infrastructure has not been created yet",
		})
		return
	}
	ip, err := publicIP(ctx)
	if err != nil {
		rep.add(DoctorCheck{
			Name:   "waf-allowlist",
			Status: CheckWarn,
			Detail: fmt.Sprintf("%d entr%s in the IP set; could not determine this machine's public IP: %v",
				len(entries), plural(len(entries)), err) + using,
		})
		return
	}
	for _, e := range entries {
		if aclEntryCoversIP(e.CIDR, ip) {
			rep.add(DoctorCheck{
				Name:   "waf-allowlist",
				Status: CheckOK,
				Detail: fmt.Sprintf("this machine (%s) is allowed by %s", ip, e.CIDR) + using,
			})
			return
		}
	}
	rep.add(DoctorCheck{
		Name:   "waf-allowlist",
		Status: CheckWarn,
		Detail: fmt.Sprintf("this machine (%s) is not in the allowlist (%d entr%s)", ip, len(entries), plural(len(entries))) + using,
		Advice: "run `nakama aws acl add` to allow it if you open code-server or Kiro Crew with --internet (CloudFront); " +
			"open/kirocrew over SSM (the default) and connect/tunnel/ssh do not use the allowlist",
	})
}

// cloudFrontInstanceNames returns the names of the instances that have
// a CloudFront distribution, sorted.
func (s *Service) cloudFrontInstanceNames(ctx context.Context) ([]string, error) {
	rows, err := s.ListInstances(ctx, false)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		if r.DistributionDomain != "" {
			names = append(names, r.Name)
		}
	}
	return names, nil
}

// aclEntryCoversIP reports whether the allowlist entry covers ip. Only
// the exact single-address form is matched directly; wider prefixes are
// matched by parsing the CIDR.
func aclEntryCoversIP(cidr, ip string) bool {
	if cidr == ip || cidr == ip+"/32" {
		return true
	}
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	addr := net.ParseIP(ip)
	return addr != nil && network.Contains(addr)
}

// simulateChunkSize bounds the ActionNames per SimulatePrincipalPolicy
// request; results within a request are still paginated via Marker.
const simulateChunkSize = 50

// diagnoseEgress reports how the private subnets reach the internet
// (ADR 0026): the mode init recorded, where the private default route
// points, and NATs left over from a switch.
func diagnoseEgress(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	inspect := opts.egress
	if inspect == nil {
		inspect = NewService(opts.Clients).InspectEgress
	}
	st, err := inspect(ctx)
	if err != nil {
		rep.add(DoctorCheck{
			Name:   "egress",
			Status: CheckWarn,
			Detail: fmt.Sprintf("could not check the egress: %v", err),
			Advice: "the check needs ssm:GetParameter, ec2:DescribeRouteTables, ec2:DescribeInstances and ec2:DescribeNatGateways",
		})
		return
	}
	rep.add(evaluateEgress(st))
}

// egressFinding is one observation behind the egress check.
type egressFinding struct {
	status CheckStatus
	detail string
	advice string
}

// evaluateEgress turns an EgressStatus into the doctor line. A route that
// leads nowhere is a fail (no instance reaches the internet); a NAT left
// over from a switch, or a mode that differs from the recorded one, is a
// warn (things work, but cost or attack surface remain).
func evaluateEgress(st *EgressStatus) DoctorCheck {
	if !st.Initialized {
		return DoctorCheck{
			Name:   "egress",
			Status: CheckWarn,
			Detail: "the private route table does not exist yet",
			Advice: "run `nakama aws init` to create the shared infrastructure",
		}
	}
	expected, recordNote := st.RecordedMode, "recorded"
	if expected == "" {
		expected, recordNote = NATModeInstance, "none recorded; treated as instance"
	}

	var (
		actual   string
		summary  string
		findings []egressFinding
	)
	switch st.RouteTarget {
	case EgressRouteNATGateway:
		actual = NATModeGateway
		gw, ok := findEgressNATGateway(st.NATGateways, st.RouteTargetID)
		switch {
		case !ok:
			findings = append(findings, egressFinding{CheckFail,
				fmt.Sprintf("the private route points at NAT gateway %s, which is gone or not nakama's", st.RouteTargetID),
				"run `nakama aws init` to create a NAT gateway and repoint the route"})
		case gw.State != "available":
			findings = append(findings, egressFinding{CheckFail,
				fmt.Sprintf("NAT gateway %s is %s%s, so the instances cannot reach the internet",
					gw.ID, gw.State, parenthesized(gw.FailureMessage)),
				"run `nakama aws init` to replace it"})
		default:
			summary = fmt.Sprintf("NAT gateway %s (available, egress IP %s)", gw.ID, gw.PublicIP)
		}
		for _, n := range st.NATInstances {
			findings = append(findings, egressFinding{CheckWarn,
				fmt.Sprintf("NAT instance %s (%s) still exists although the egress goes through the NAT gateway", n.ID, n.State),
				"run `nakama aws init` to terminate it"})
		}
	case EgressRouteNATInstance:
		actual = NATModeInstance
		n, ok := findEgressNATInstance(st.NATInstances, st.RouteTargetID)
		if !ok {
			findings = append(findings, egressFinding{CheckFail,
				fmt.Sprintf("the private route points at network interface %s, which no nakama NAT instance has", st.RouteTargetID),
				"run `nakama aws init` to create the NAT instance and repoint the route"})
		} else {
			summary = fmt.Sprintf("NAT instance %s (%s; it runs while a code-server instance runs)", n.ID, n.State)
		}
		for _, gw := range st.NATGateways {
			findings = append(findings, egressFinding{CheckWarn,
				fmt.Sprintf("NAT gateway %s (%s) still exists although the egress goes through the NAT instance; it is charged while it exists",
					gw.ID, gw.State),
				"run `nakama aws init` to delete it"})
		}
	case EgressRouteNone:
		findings = append(findings, egressFinding{CheckFail,
			"the private route table has no default route, so the instances cannot reach the internet",
			"run `nakama aws init` to add it"})
	default:
		findings = append(findings, egressFinding{CheckFail,
			fmt.Sprintf("the private default route points at %s, not at a nakama NAT", st.RouteTargetID),
			"run `nakama aws init` to repoint it"})
	}
	if actual != "" && actual != expected {
		findings = append(findings, egressFinding{CheckWarn,
			fmt.Sprintf("init is set to the %s mode, but the egress goes through the %s", expected, natModeNoun(actual)),
			fmt.Sprintf("run `nakama aws init` to finish the switch, or `nakama aws init --nat %s` to keep the %s",
				actual, natModeNoun(actual))})
	}

	c := DoctorCheck{Name: "egress", Status: CheckOK}
	parts := []string{fmt.Sprintf("%s mode (%s)", expected, recordNote)}
	if summary != "" {
		parts = append(parts, summary)
	}
	for _, f := range findings {
		parts = append(parts, f.detail)
		if checkSeverity(f.status) > checkSeverity(c.Status) {
			c.Status = f.status
			c.Advice = f.advice
		}
	}
	c.Detail = strings.Join(parts, "; ")
	return c
}

// natModeNoun names the NAT of a mode in a sentence.
func natModeNoun(mode string) string {
	if mode == NATModeGateway {
		return "NAT gateway"
	}
	return "NAT instance"
}

// checkSeverity orders statuses for picking the worst one.
func checkSeverity(s CheckStatus) int {
	switch s {
	case CheckFail:
		return 2
	case CheckWarn:
		return 1
	default:
		return 0
	}
}

// parenthesized returns " (s)", or "" for an empty s.
func parenthesized(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func findEgressNATGateway(gws []EgressNATGateway, id string) (EgressNATGateway, bool) {
	for _, g := range gws {
		if g.ID == id {
			return g, true
		}
	}
	return EgressNATGateway{}, false
}

func findEgressNATInstance(nats []EgressNATInstance, eniID string) (EgressNATInstance, bool) {
	for _, n := range nats {
		if n.ENIID == eniID {
			return n, true
		}
	}
	return EgressNATInstance{}, false
}

// diagnoseCapabilities reports one line per nakama feature stating
// whether the caller's IAM policies allow every action the feature
// needs. The feature-to-actions table lives in permissions.go (the
// single source of truth shared with the docs). Missing permissions are
// warn, not fail: a deliberately scoped principal is a valid setup, and
// fail stays reserved for a broken environment.
func diagnoseCapabilities(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	sourceARN, err := policySourceARNFromCaller(opts.Clients.CallerARN)
	if err != nil {
		rep.add(DoctorCheck{
			Name:   "capabilities",
			Status: CheckWarn,
			Detail: fmt.Sprintf("could not derive a policy source from the caller ARN: %v", err),
		})
		return
	}
	union := simulatedActionUnion()
	allowed := make(map[string]bool, len(union))
	for start := 0; start < len(union); start += simulateChunkSize {
		chunk := union[start:min(start+simulateChunkSize, len(union))]
		var marker *string
		for {
			out, err := opts.Clients.IAM.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{
				PolicySourceArn: aws.String(sourceARN),
				ActionNames:     chunk,
				ResourceArns:    []string{"*"},
				Marker:          marker,
			})
			if err != nil {
				rep.add(DoctorCheck{
					Name:   "capabilities",
					Status: CheckWarn,
					Detail: fmt.Sprintf("could not verify: %v", err),
					Advice: "the caller needs iam:SimulatePrincipalPolicy for this check only; the features themselves may still work",
				})
				return
			}
			for _, r := range out.EvaluationResults {
				if r.EvalDecision == iamtypes.PolicyEvaluationDecisionTypeAllowed {
					allowed[aws.ToString(r.EvalActionName)] = true
				}
			}
			if !out.IsTruncated {
				break
			}
			marker = out.Marker
		}
	}
	for _, row := range featurePermissionMatrix() {
		if !row.Simulate {
			continue
		}
		var missing []string
		for _, a := range row.Actions {
			if !allowed[a] {
				missing = append(missing, a)
			}
		}
		name := "capability: " + row.Feature
		if len(missing) == 0 {
			rep.add(DoctorCheck{
				Name:   name,
				Status: CheckOK,
				Detail: fmt.Sprintf("all %d actions allowed", len(row.Actions)),
			})
			continue
		}
		rep.add(DoctorCheck{
			Name:   name,
			Status: CheckWarn,
			Detail: "missing: " + strings.Join(missing, ", "),
			Advice: `grant the listed actions to use this feature (the simulation evaluates Resource "*"; resource-scoped allow policies can be reported as missing even though the real call succeeds)`,
		})
	}
}

// diagnoseTunnels reports locally recorded tunnels and dead forwards.
func diagnoseTunnels(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	list := opts.tunnelNames
	if list == nil {
		list = listTunnelStateNames
	}
	names, err := list()
	if err != nil {
		rep.add(DoctorCheck{Name: "tunnels", Status: CheckWarn, Detail: fmt.Sprintf("could not read the tunnel state directory: %v", err)})
		return
	}
	if len(names) == 0 {
		rep.add(DoctorCheck{Name: "tunnels", Status: CheckOK, Detail: "no tunnels recorded"})
		return
	}
	statusOf := opts.tunnelStatus
	if statusOf == nil {
		if opts.Clients == nil {
			rep.add(DoctorCheck{
				Name:   "tunnels",
				Status: CheckSkip,
				Detail: fmt.Sprintf("%d tunnel(s) recorded (%s); state not probed without credentials", len(names), strings.Join(names, ", ")),
			})
			return
		}
		statusOf = NewService(opts.Clients).GetTunnelStatus
	}

	var dead, unreachable []string
	for _, name := range names {
		st, err := statusOf(ctx, name)
		if err != nil || st == nil {
			unreachable = append(unreachable, name)
			continue
		}
		for _, f := range st.Forwards {
			if f.State == ForwardStateDead {
				dead = append(dead, fmt.Sprintf("%s (%s)", name, f.Spec))
			}
		}
	}
	switch {
	case len(dead) > 0:
		rep.add(DoctorCheck{
			Name:   "tunnels",
			Status: CheckWarn,
			Detail: "dead forwards: " + strings.Join(dead, ", "),
			Advice: "after `nakama aws login`, run `nakama aws tunnel restart <name>` to reconnect with the recorded forwards",
		})
	case len(unreachable) > 0:
		rep.add(DoctorCheck{
			Name:   "tunnels",
			Status: CheckWarn,
			Detail: "state recorded but the daemon does not answer: " + strings.Join(unreachable, ", "),
			Advice: "run `nakama aws tunnel status <name>` (it cleans up stale state) or start the tunnel again",
		})
	default:
		rep.add(DoctorCheck{
			Name:   "tunnels",
			Status: CheckOK,
			Detail: fmt.Sprintf("%d tunnel(s) healthy: %s", len(names), strings.Join(names, ", ")),
		})
	}
}

// diagnoseVersion reports whether a newer nakama release is out. It asks
// api.github.com, and is never a fail: a network that blocks GitHub or
// an old version is no reason to fail the diagnosis.
func diagnoseVersion(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	check := opts.latestRelease
	if check == nil {
		check = func(ctx context.Context) version.CheckResult {
			return version.Checker{}.Check(ctx, version.Version)
		}
	}
	r := check(ctx)
	c := DoctorCheck{Name: "version"}
	switch r.Status {
	case version.StatusUpToDate:
		c.Status = CheckOK
		c.Detail = r.Current + " is the latest release"
	case version.StatusAhead:
		c.Status = CheckOK
		c.Detail = fmt.Sprintf("%s is newer than the latest release (%s)", r.Current, r.Latest)
	case version.StatusOutdated:
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("%s is out of date; the latest release is %s", r.Current, r.Latest)
		c.Advice = "download " + r.Latest + " from " + r.URL
	case version.StatusDevelopment:
		c.Status = CheckSkip
		c.Detail = fmt.Sprintf("skipped: %s is a development build, not compared with the latest release (%s)", r.Current, r.Latest)
	default:
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("could not check for a newer release: %v", r.Err)
		c.Advice = "the check asks api.github.com; where that is blocked, compare `nakama version` with " + config.ReleasesURL
	}
	rep.add(c)
}

// listTunnelStateNames returns the instance names that have a tunnel
// state file, sorted. A missing directory is not an error.
func listTunnelStateNames() ([]string, error) {
	dir, err := tunnelStateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// plural returns the English plural suffix for "entry".
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
