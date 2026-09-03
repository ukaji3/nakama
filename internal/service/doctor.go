package service

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ukaji3/nakama/internal/profilestore"
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
	// tunnelNames overrides the tunnel state listing in tests.
	tunnelNames func() ([]string, error)
	// tunnelStatus overrides the per-tunnel status probe in tests.
	tunnelStatus func(ctx context.Context, name string) (*TunnelStatus, error)
}

// Diagnose runs the read-only environment diagnosis behind
// `nakama aws doctor`: proxy resolution and TLS interception (reusing
// the cliproxy detector from ADR 0018), credential resolution, WAF
// allowlist membership for this machine's public IP, one capability
// line per feature derived from featurePermissionMatrix (permissions.go),
// and the local tunnel state. It performs no mutation of any kind and
// never returns credentials, tokens or passwords in the report.
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
		for _, name := range []string{"waf-allowlist", "capabilities"} {
			rep.add(DoctorCheck{Name: name, Status: CheckSkip, Detail: "skipped: no AWS credentials"})
		}
		diagnoseTunnels(ctx, opts, rep)
		return rep
	}

	diagnoseCredentials(opts, rep)
	diagnoseWAF(ctx, opts, rep)
	diagnoseCapabilities(ctx, opts, rep)
	diagnoseTunnels(ctx, opts, rep)
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
// code-server through the WAF allowlist.
func diagnoseWAF(ctx context.Context, opts DoctorOptions, rep *DoctorReport) {
	listACL := opts.aclEntries
	if listACL == nil {
		listACL = NewService(opts.Clients).ListACL
	}
	publicIP := opts.publicIP
	if publicIP == nil {
		publicIP = opts.Clients.IPDiscoverer.PublicIPv4
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
				len(entries), plural(len(entries)), err),
		})
		return
	}
	for _, e := range entries {
		if aclEntryCoversIP(e.CIDR, ip) {
			rep.add(DoctorCheck{
				Name:   "waf-allowlist",
				Status: CheckOK,
				Detail: fmt.Sprintf("this machine (%s) is allowed by %s", ip, e.CIDR),
			})
			return
		}
	}
	rep.add(DoctorCheck{
		Name:   "waf-allowlist",
		Status: CheckWarn,
		Detail: fmt.Sprintf("this machine (%s) is not in the allowlist (%d entr%s)", ip, len(entries), plural(len(entries))),
		Advice: "run `nakama aws acl add` to allow it (code-server is unreachable otherwise; connect/tunnel/ssh are unaffected)",
	})
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
