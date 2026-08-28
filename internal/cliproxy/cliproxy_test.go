package cliproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinnedATSFingerprints are the SHA-256 fingerprints of the Amazon
// Trust Services roots, cross-checked at import time against both
// https://www.amazontrust.com/repository/ and the Ubuntu
// ca-certificates package.
var pinnedATSFingerprints = map[string]string{
	"8ecde6884f3d87b1125ba31ac3fcb13d7016de7f57cc904fe1cb97c6ae98196e": "Amazon Root CA 1",
	"1ba5b2aa8c65401a82960118f80bec4f62304d83cec4713a19c39c011ea46db4": "Amazon Root CA 2",
	"18ce6cfe7bf14e60b2e347b8dfe868cb31d02ebb3ada271569f50343b46db3a4": "Amazon Root CA 3",
	"e35d28419ed02025cfa69038cd623962458da5c695fbdea3c22b0bfb25897092": "Amazon Root CA 4",
	"568d6905a2c88708a4b3025190edcfedb1974a606a13c6e5290fcb2ae63edab5": "Starfield Services Root Certificate Authority - G2",
}

func TestATSRootsArePinned(t *testing.T) {
	roots, err := atsRoots()
	if err != nil {
		t.Fatalf("atsRoots: %v", err)
	}
	if len(roots) != len(pinnedATSFingerprints) {
		t.Fatalf("embedded roots = %d, want %d", len(roots), len(pinnedATSFingerprints))
	}
	for _, r := range roots {
		name, ok := pinnedATSFingerprints[fingerprint(r)]
		if !ok {
			t.Errorf("unexpected embedded root %q (fp %s)", r.Subject.String(), fingerprint(r))
			continue
		}
		if !strings.Contains(r.Subject.String(), name) {
			t.Errorf("subject %q does not contain pinned name %q", r.Subject.String(), name)
		}
	}
}

// testCA is a locally generated CA standing in for a corporate
// TLS-interception CA, with a leaf for the probe target.
type testCA struct {
	caCert  *x509.Certificate
	pool    *x509.CertPool
	tlsCert tls.Certificate
}

func newTestCA(t *testing.T, host string) *testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Example Corp Interception CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &testCA{
		caCert: caCert,
		pool:   pool,
		tlsCert: tls.Certificate{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
		},
	}
}

// startTLSTarget serves HTTPS with the test CA's leaf, standing in for
// an AWS endpoint seen through an intercepting proxy.
func startTLSTarget(t *testing.T, ca *testCA) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // any HTTP status is fine for the probe
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.tlsCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// startConnectProxy runs a minimal HTTP CONNECT proxy. When
// authSchemes is non-empty every CONNECT is rejected with 407 and
// those Proxy-Authenticate values.
func startConnectProxy(t *testing.T, authSchemes []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if len(authSchemes) > 0 {
			for _, s := range authSchemes {
				w.Header().Add("Proxy-Authenticate", s)
			}
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestDetector wires a Detector at the test target through the test
// proxy, trusting the test CA as the "OS store".
func newTestDetector(t *testing.T, target *httptest.Server, proxyURL string, ca *testCA) *Detector {
	t.Helper()
	var pu *url.URL
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			t.Fatal(err)
		}
		pu = u
	}
	return &Detector{
		proxyFunc:    func(*http.Request) (*url.URL, error) { return pu, nil },
		endpoint:     target.URL + "/",
		timeout:      5 * time.Second,
		rootCAs:      ca.pool,
		isPublicRoot: isATSRoot,
		bundleDir:    t.TempDir(),
	}
}

func TestDetectThroughInterceptingProxy(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	target := startTLSTarget(t, ca)
	proxySrv := startConnectProxy(t, nil)
	d := newTestDetector(t, target, proxySrv.URL, ca)

	det, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if det.ProxyURL != proxySrv.URL {
		t.Errorf("ProxyURL = %q, want %q", det.ProxyURL, proxySrv.URL)
	}
	if !det.Intercepted {
		t.Fatal("Intercepted = false, want true (test CA is not an ATS root)")
	}
	if !strings.Contains(det.RootSubject, "Example Corp Interception CA") {
		t.Errorf("RootSubject = %q", det.RootSubject)
	}
	if det.CheckErr != nil {
		t.Errorf("CheckErr = %v, want nil (bundle must make the endpoint verifiable)", det.CheckErr)
	}

	pemBytes, err := os.ReadFile(det.BundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	var subjects []string
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("bundle certificate: %v", err)
		}
		subjects = append(subjects, cert.Subject.String())
	}
	// Corporate CA + the five ATS roots.
	if len(subjects) != 6 {
		t.Fatalf("bundle has %d certificates, want 6: %v", len(subjects), subjects)
	}
	joined := strings.Join(subjects, "\n")
	if !strings.Contains(joined, "Example Corp Interception CA") {
		t.Error("bundle must contain the corporate CA")
	}
	if !strings.Contains(joined, "Amazon Root CA 1") {
		t.Error("bundle must contain the ATS roots for interception-bypassed endpoints")
	}
	if strings.Contains(joined, "CN=127.0.0.1") {
		t.Error("bundle must not contain the leaf certificate")
	}
	if filepath.Base(det.BundlePath) != bundleFileName {
		t.Errorf("bundle file = %q", det.BundlePath)
	}
}

func TestDetectDirectConnectionIntercepted(t *testing.T) {
	// Transparent interception: no explicit proxy, still a corporate
	// root on the wire.
	ca := newTestCA(t, "127.0.0.1")
	target := startTLSTarget(t, ca)
	d := newTestDetector(t, target, "", ca)

	det, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if det.ProxyURL != "" {
		t.Errorf("ProxyURL = %q, want empty", det.ProxyURL)
	}
	if !det.Intercepted || det.BundlePath == "" {
		t.Errorf("det = %+v, want interception with a bundle", det)
	}
}

func TestDetectNoInterception(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	target := startTLSTarget(t, ca)
	d := newTestDetector(t, target, "", ca)
	// Classify the (test) root as public, as if the chain ended at an
	// ATS root: no bundle must be written and none referenced.
	d.isPublicRoot = func(*x509.Certificate) (bool, error) { return true, nil }

	det, err := d.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if det.Intercepted || det.BundlePath != "" {
		t.Errorf("det = %+v, want no interception and no bundle", det)
	}
	if det.CheckErr != nil {
		t.Errorf("CheckErr = %v", det.CheckErr)
	}
	if _, err := os.Stat(filepath.Join(d.bundleDir, bundleFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("no bundle file must be written without interception")
	}
}

func TestDetectProxyRequiresNTLM(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	target := startTLSTarget(t, ca)
	proxySrv := startConnectProxy(t, []string{"Negotiate", "NTLM"})
	d := newTestDetector(t, target, proxySrv.URL, ca)

	_, err := d.Detect(context.Background())
	var authErr *ProxyAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("want *ProxyAuthError, got %v", err)
	}
	if authErr.SupportsBasic() {
		t.Error("SupportsBasic = true for NTLM/Negotiate-only proxy")
	}
	if len(authErr.Schemes) != 2 {
		t.Errorf("Schemes = %v", authErr.Schemes)
	}
}

func TestDetectProxyRequiresBasic(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	target := startTLSTarget(t, ca)
	proxySrv := startConnectProxy(t, []string{`Basic realm="corp"`})
	d := newTestDetector(t, target, proxySrv.URL, ca)

	_, err := d.Detect(context.Background())
	var authErr *ProxyAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("want *ProxyAuthError, got %v", err)
	}
	if !authErr.SupportsBasic() {
		t.Error("SupportsBasic = false, want true")
	}
}

func TestIsATSRootRejectsOtherCAs(t *testing.T) {
	ca := newTestCA(t, "127.0.0.1")
	ok, err := isATSRoot(ca.caCert)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a locally generated CA must not classify as an ATS root")
	}
	roots, err := atsRoots()
	if err != nil {
		t.Fatal(err)
	}
	ok, err = isATSRoot(roots[0])
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("an embedded ATS root must classify as such")
	}
}

func TestEnvAppliesDetection(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"http_proxy=http://old.example:1",   // replaced, case-insensitively
		"HTTPS_PROXY=http://old.example:2",  // replaced
		"NO_PROXY=corp.example,localhost",   // merged, order preserved, deduped
		"AWS_CA_BUNDLE=C:\\old\\bundle.pem", // replaced
	}
	det := &Detection{
		ProxyURL:   "http://proxy.corp.example:8080",
		BundlePath: "/home/u/.config/nakama/aws-ca-bundle.pem",
	}
	env := Env(base, det)

	find := func(key string) (string, int) {
		val, n := "", 0
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if strings.EqualFold(k, key) {
				val = v
				n++
			}
		}
		return val, n
	}
	if v, n := find("HTTP_PROXY"); n != 1 || v != det.ProxyURL {
		t.Errorf("HTTP_PROXY = %q (count %d)", v, n)
	}
	if v, n := find("HTTPS_PROXY"); n != 1 || v != det.ProxyURL {
		t.Errorf("HTTPS_PROXY = %q (count %d)", v, n)
	}
	if v, n := find("NO_PROXY"); n != 1 || v != "corp.example,localhost,127.0.0.1,::1" {
		t.Errorf("NO_PROXY = %q (count %d)", v, n)
	}
	if v, n := find("AWS_CA_BUNDLE"); n != 1 || v != det.BundlePath {
		t.Errorf("AWS_CA_BUNDLE = %q (count %d)", v, n)
	}
	if v, _ := find("NAKAMA_AWS_CLI_SHELL"); v != "1" {
		t.Errorf("NAKAMA_AWS_CLI_SHELL = %q", v)
	}
	if v, _ := find("PATH"); v != "/usr/bin" {
		t.Errorf("PATH = %q, must be preserved", v)
	}
}

func TestEnvDirectNoBundle(t *testing.T) {
	env := Env([]string{"PATH=/usr/bin", "NO_PROXY=keep.example"}, &Detection{})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "HTTP_PROXY=") || strings.Contains(joined, "HTTPS_PROXY=") {
		t.Errorf("no proxy vars must be set for a direct connection: %v", env)
	}
	if strings.Contains(joined, "AWS_CA_BUNDLE=") {
		t.Errorf("no bundle var must be set without interception: %v", env)
	}
	if !strings.Contains(joined, "NO_PROXY=keep.example") {
		t.Errorf("existing NO_PROXY must be preserved: %v", env)
	}
	if !strings.Contains(joined, "NAKAMA_AWS_CLI_SHELL=1") {
		t.Errorf("marker must always be set: %v", env)
	}
}
