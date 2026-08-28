// Package cliproxy detects the proxy and TLS-interception environment
// the AWS CLI would face on this machine and renders it into the aws
// CLI's official configuration surface: HTTP(S)_PROXY / NO_PROXY
// environment variables and an AWS_CA_BUNDLE PEM file (ADR 0018).
//
// The AWS CLI neither reads the OS proxy configuration (WinHTTP / PAC)
// nor the OS certificate store, so on corporate Windows machines
// behind a TLS-intercepting proxy it fails with CERTIFICATE_VERIFY_
// FAILED out of the box. nakama already resolves both (ADR 0015);
// this package bridges that knowledge to the aws CLI without touching
// the user's ~/.aws/config or any persistent OS state.
package cliproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ukaji3/nakama/internal/proxy"
)

// defaultProbeEndpoint is the AWS endpoint used to observe the TLS
// chain a corporate proxy would present to the aws CLI. STS in the
// project's fixed region: reachable without credentials (the TLS
// handshake and an unauthenticated HTTP response are all we need).
const defaultProbeEndpoint = "https://sts.ap-northeast-1.amazonaws.com/"

// bundleFileName is the AWS_CA_BUNDLE file written under the nakama
// config directory. It contains only public certificates.
const bundleFileName = "aws-ca-bundle.pem"

// Detector probes the network environment. The zero value is not
// usable; construct with NewDetector. Fields are unexported and
// overridden by same-package tests.
type Detector struct {
	// proxyFunc resolves the proxy for a request (ADR 0015 resolver in
	// production: environment first, then WinHTTP/PAC on Windows).
	proxyFunc func(*http.Request) (*url.URL, error)
	// endpoint is the AWS URL to probe.
	endpoint string
	// timeout bounds each network operation.
	timeout time.Duration
	// rootCAs overrides the trust store used to verify the probed
	// chain. nil means the platform verifier (the OS certificate
	// store on Windows), which is the point of the design: only a
	// chain the OS itself trusts (e.g. a GPO-distributed corporate CA)
	// is ever exported, so an actual attacker's chain — which the OS
	// would reject — can never end up in the bundle.
	rootCAs *x509.CertPool
	// isPublicRoot classifies the verified chain's root. The default
	// compares against the embedded Amazon Trust Services roots: AWS
	// endpoints chain to those, so any other OS-trusted root means a
	// TLS-intercepting proxy re-signed the connection.
	isPublicRoot func(*x509.Certificate) (bool, error)
	// bundleDir is where the AWS_CA_BUNDLE file is written.
	bundleDir string
}

// NewDetector returns a Detector with production defaults.
func NewDetector() *Detector {
	return &Detector{
		proxyFunc:    proxy.Func(),
		endpoint:     defaultProbeEndpoint,
		timeout:      15 * time.Second,
		isPublicRoot: isATSRoot,
	}
}

// Detection is the result of Detect.
type Detection struct {
	// ProxyURL is the resolved proxy ("" = direct connection).
	ProxyURL string
	// ProxyFromEnv reports whether HTTP(S)_PROXY environment variables
	// already decided the proxy (as opposed to WinHTTP/PAC discovery).
	ProxyFromEnv bool
	// Intercepted reports that the verified chain's root is not a
	// public AWS root, i.e. a TLS-intercepting proxy re-signs traffic.
	Intercepted bool
	// RootSubject is the subject of the verified chain's root, for
	// display ("who re-signed the connection").
	RootSubject string
	// BundlePath is the written AWS_CA_BUNDLE file ("" when no
	// interception was detected: the aws CLI's default bundle works).
	BundlePath string
	// CheckErr is the result of the final end-to-end verification
	// using exactly the settings the subshell will export (nil = OK).
	CheckErr error
}

// ProxyAuthError reports that the proxy demands authentication for
// CONNECT, which nakama cannot supply. The aws CLI supports only HTTP
// Basic (credentials embedded in the proxy URL); NTLM/Negotiate-only
// proxies need a local authenticating relay (e.g. Cntlm or px).
type ProxyAuthError struct {
	Proxy   string
	Schemes []string
}

func (e *ProxyAuthError) Error() string {
	return fmt.Sprintf("proxy %s requires authentication (%s)", e.Proxy, strings.Join(e.Schemes, ", "))
}

// SupportsBasic reports whether the proxy offered HTTP Basic, the only
// scheme the aws CLI can use (as user:password@ in the proxy URL).
func (e *ProxyAuthError) SupportsBasic() bool {
	for _, s := range e.Schemes {
		if strings.EqualFold(s, "Basic") {
			return true
		}
	}
	return false
}

// Detect resolves the proxy, probes the AWS endpoint through it,
// classifies TLS interception, writes the CA bundle when needed, and
// end-to-end verifies the resulting configuration.
func (d *Detector) Detect(ctx context.Context) (*Detection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("parse probe endpoint: %w", err)
	}
	proxyURL, err := d.proxyFunc(req)
	if err != nil {
		return nil, fmt.Errorf("resolve proxy: %w", err)
	}
	det := &Detection{}
	if proxyURL != nil {
		det.ProxyURL = proxyURL.String()
		det.ProxyFromEnv = os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" ||
			os.Getenv("HTTP_PROXY") != "" || os.Getenv("http_proxy") != ""
	}

	state, err := d.probeTLS(ctx, proxyURL)
	if err != nil {
		// Diagnose the most common hard failure precisely: a proxy
		// that rejects unauthenticated CONNECT.
		if proxyURL != nil {
			if authErr := d.probeProxyAuth(ctx, proxyURL, req.URL); authErr != nil {
				return nil, authErr
			}
		}
		return nil, fmt.Errorf("probe %s: %w", d.endpoint, err)
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return nil, errors.New("probe connection returned no verified certificate chain")
	}
	chain := state.VerifiedChains[0]
	root := chain[len(chain)-1]
	det.RootSubject = root.Subject.String()

	public, err := d.isPublicRoot(root)
	if err != nil {
		return nil, err
	}
	if !public {
		det.Intercepted = true
		bundle, err := buildBundle(chain)
		if err != nil {
			return nil, err
		}
		path, err := d.writeBundle(bundle)
		if err != nil {
			return nil, err
		}
		det.BundlePath = path
	}

	det.CheckErr = d.verifyConfiguration(ctx, proxyURL, det.BundlePath)
	return det, nil
}

// probeTLS connects to the endpoint (optionally through the proxy)
// with the ambient trust store and returns the TLS connection state.
func (d *Detector) probeTLS(ctx context.Context, proxyURL *url.URL) (*tls.ConnectionState, error) {
	return d.connectionState(ctx, proxyURL, d.rootCAs)
}

// verifyConfiguration repeats the probe with exactly the trust the aws
// CLI will be given: the written bundle when interception was
// detected, the ambient store otherwise.
func (d *Detector) verifyConfiguration(ctx context.Context, proxyURL *url.URL, bundlePath string) error {
	pool := d.rootCAs
	if bundlePath != "" {
		pemBytes, err := os.ReadFile(bundlePath) // #nosec G304 -- path was just written by us under the user config dir
		if err != nil {
			return err
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return errors.New("written CA bundle contains no usable certificates")
		}
	}
	_, err := d.connectionState(ctx, proxyURL, pool)
	return err
}

// connectionState performs one HTTPS request and returns the TLS
// state. A non-2xx HTTP status is fine: the TLS handshake is what is
// being tested.
func (d *Detector) connectionState(ctx context.Context, proxyURL *url.URL, pool *x509.CertPool) (*tls.ConnectionState, error) {
	transport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return proxyURL, nil },
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: d.timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.TLS == nil {
		return nil, errors.New("probe endpoint did not negotiate TLS")
	}
	return resp.TLS, nil
}

// probeProxyAuth sends a bare CONNECT to the proxy and reports a
// *ProxyAuthError when it demands authentication. It returns nil when
// the proxy lets an unauthenticated CONNECT through (the probe failure
// then has another cause).
func (d *Detector) probeProxyAuth(ctx context.Context, proxyURL *url.URL, target *url.URL) *ProxyAuthError {
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		host = net.JoinHostPort(proxyURL.Hostname(), "8080")
	}
	dialer := &net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(d.timeout))
	targetHost := target.Host
	if target.Port() == "" {
		targetHost = net.JoinHostPort(target.Hostname(), "443")
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetHost, targetHost); err != nil {
		return nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		return nil
	}
	var schemes []string
	for _, v := range resp.Header.Values("Proxy-Authenticate") {
		if s, _, _ := strings.Cut(strings.TrimSpace(v), " "); s != "" {
			schemes = append(schemes, s)
		}
	}
	if len(schemes) == 0 {
		schemes = []string{"unknown"}
	}
	return &ProxyAuthError{Proxy: proxyURL.String(), Schemes: schemes}
}

// buildBundle renders the CA certificates of the verified chain plus
// the embedded Amazon Trust Services roots as one PEM bundle.
//
// The chain's CA certificates (everything above the leaf) let the aws
// CLI verify connections re-signed by the corporate proxy. The ATS
// roots keep endpoints on the proxy's interception-bypass list
// verifiable too: AWS_CA_BUNDLE *replaces* the CLI's default bundle,
// so a corporate-only bundle would break any AWS endpoint that is not
// intercepted.
func buildBundle(chain []*x509.Certificate) ([]byte, error) {
	cas := chain[1:]
	if len(cas) == 0 {
		// Degenerate but possible: the proxy presents a single
		// self-signed certificate. It then is its own root.
		cas = chain
	}
	ats, err := atsRoots()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# AWS_CA_BUNDLE generated by `nakama aws cli shell`.\n")
	b.WriteString("# Corporate proxy CA chain (verified against the OS certificate store)\n")
	b.WriteString("# plus the Amazon Trust Services roots. Public certificates only.\n")
	seen := map[string]bool{}
	for _, c := range append(append([]*x509.Certificate{}, cas...), ats...) {
		fp := fingerprint(c)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		b.WriteString("\n# " + c.Subject.String() + "\n")
		if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}); err != nil {
			return nil, err
		}
	}
	return []byte(b.String()), nil
}

// writeBundle persists the bundle under the nakama config directory
// (same convention as internal/service's tunnel state and
// internal/profilestore: XDG_CONFIG_HOME, else ~/.config).
func (d *Detector) writeBundle(pemBytes []byte) (string, error) {
	dir := d.bundleDir
	if dir == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			dir = filepath.Join(x, "nakama")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home directory for the CA bundle: %w", err)
			}
			dir = filepath.Join(home, ".config", "nakama")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // #nosec G703 -- dir derives from the user's own XDG_CONFIG_HOME/home, the intended destination (same trust as the tunnel state dir)
		return "", fmt.Errorf("create nakama config dir: %w", err)
	}
	path := filepath.Join(dir, bundleFileName)
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil { // #nosec G703 -- fixed file name under the user's own config dir; contains public certificates only
		return "", fmt.Errorf("write CA bundle: %w", err)
	}
	return path, nil
}

// managedEnvKeys are the variables Env owns: pre-existing entries are
// replaced, not duplicated (Windows environment names are
// case-insensitive, so matching is too).
var managedEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "AWS_CA_BUNDLE", "NAKAMA_AWS_CLI_SHELL"}

// Env returns base with the detection result applied: the resolved
// proxy exported the way the aws CLI reads it, the CA bundle when
// interception was detected, and a marker so nested invocations can be
// recognized. base is not modified.
func Env(base []string, det *Detection) []string {
	existingNoProxy := ""
	out := make([]string, 0, len(base)+len(managedEnvKeys))
	for _, kv := range base {
		key, value, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, "NO_PROXY") && existingNoProxy == "" {
			existingNoProxy = value
		}
		managed := false
		for _, m := range managedEnvKeys {
			if strings.EqualFold(key, m) {
				managed = true
				break
			}
		}
		if !managed {
			out = append(out, kv)
		}
	}
	if det.ProxyURL != "" {
		out = append(out,
			"HTTP_PROXY="+det.ProxyURL,
			"HTTPS_PROXY="+det.ProxyURL,
			"NO_PROXY="+mergeNoProxy(existingNoProxy),
		)
	} else if existingNoProxy != "" {
		out = append(out, "NO_PROXY="+existingNoProxy)
	}
	if det.BundlePath != "" {
		out = append(out, "AWS_CA_BUNDLE="+det.BundlePath)
	}
	out = append(out, "NAKAMA_AWS_CLI_SHELL=1")
	return out
}

// mergeNoProxy appends the loopback defaults to an existing NO_PROXY
// value, deduplicating entries and preserving their order.
func mergeNoProxy(existing string) string {
	seen := map[string]bool{}
	var entries []string
	for _, e := range strings.Split(existing+",localhost,127.0.0.1,::1", ",") {
		e = strings.TrimSpace(e)
		if e == "" || seen[strings.ToLower(e)] {
			continue
		}
		seen[strings.ToLower(e)] = true
		entries = append(entries, e)
	}
	return strings.Join(entries, ",")
}
