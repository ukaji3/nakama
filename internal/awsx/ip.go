package awsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/proxy"
)

var errEmptyAMIParameter = errors.New("ssm parameter for ubuntu ami returned empty value")

// IPCheckRounds is how many times DetectPublicIPv4s asks each check
// site (Issue #104).
const IPCheckRounds = 3

// IPDiscoverer finds the caller's public IPv4 address.
//
// Implementations are expected to be deterministic with respect to
// the network configuration and to fail rather than guess.
type IPDiscoverer interface {
	// PublicIPv4 asks checkip.amazonaws.com once: a quick check for
	// "is this machine allowlisted" (open, doctor).
	PublicIPv4(ctx context.Context) (string, error)
	// DetectPublicIPv4s asks every check site IPCheckRounds times and
	// returns the distinct public IPv4 addresses seen, for the commands
	// that change the allowlist (acl add, acl delete all --keep-mine).
	// A network with redundant exits can show a different address per
	// connection, so each check uses a new connection. It fails only
	// when no check found an address.
	DetectPublicIPv4s(ctx context.Context) (*DetectedIPs, error)
}

// DetectedIPs is the outcome of DetectPublicIPv4s.
type DetectedIPs struct {
	// IPs are the distinct public IPv4 addresses, in ascending order.
	IPs []string
	// Checks is the number of checks made.
	Checks int
	// Failures are the checks that found no address, in site order.
	Failures []IPCheckFailure
}

// IPCheckFailure is one check that found no public IPv4 address.
type IPCheckFailure struct {
	Site string
	Err  error
}

// FailureSummary describes the failed checks, one line per site:
// "ifconfig.io: 2 of 3 checks failed: <the first error>".
func (d *DetectedIPs) FailureSummary() []string {
	if d == nil || len(d.Failures) == 0 {
		return nil
	}
	var (
		order []string
		count = map[string]int{}
		first = map[string]error{}
	)
	for _, f := range d.Failures {
		if count[f.Site] == 0 {
			order = append(order, f.Site)
			first[f.Site] = f.Err
		}
		count[f.Site]++
	}
	lines := make([]string, 0, len(order))
	for _, site := range order {
		lines = append(lines, fmt.Sprintf("%s: %d of %d checks failed: %v", site, count[site], IPCheckRounds, first[site]))
	}
	return lines
}

// ipCheckSite is one public "what is my IP" endpoint that answers with
// the address alone, as plain text.
type ipCheckSite struct {
	name string // shown in warnings
	url  string
}

// ipCheckSites are the sites DetectPublicIPv4s asks. ifconfig.io's /ip
// answers in plain text whatever the User-Agent ("/" sends HTML to
// some clients).
var ipCheckSites = []ipCheckSite{
	{name: "checkip.amazonaws.com", url: nakamaConfig.CheckIPURL},
	{name: "ifconfig.io", url: nakamaConfig.IfconfigIOURL},
}

type httpIPDiscoverer struct {
	// client serves PublicIPv4, unchanged since before Issue #104.
	client *http.Client
	// checkClient serves DetectPublicIPv4s: no connection reuse, and
	// IPv4 to the check sites.
	checkClient *http.Client
	sites       []ipCheckSite
}

// NewHTTPIPDiscoverer returns the production IPDiscoverer. Its clients
// resolve proxies via internal/proxy (env vars first, then OS
// settings) instead of the default http.ProxyFromEnvironment.
func NewHTTPIPDiscoverer() IPDiscoverer {
	return &httpIPDiscoverer{
		client:      &http.Client{Timeout: 5 * time.Second, Transport: proxy.NewTransport()},
		checkClient: &http.Client{Timeout: 5 * time.Second, Transport: newIPCheckTransport(proxy.NewTransport(), ipCheckSites, nil)},
		sites:       ipCheckSites,
	}
}

// dialFunc is net.Dialer.DialContext's signature.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// newIPCheckTransport returns a copy of tr adapted for
// DetectPublicIPv4s (tr itself is left alone):
//
//   - every request opens a new connection (no keep-alive, and HTTP/2
//     connections are single-use with it), so that a network that picks
//     its exit per connection can show each exit;
//   - a direct connection to a check site is made over IPv4, as
//     "curl -4" does: ifconfig.io also has IPv6 addresses and answers a
//     dual-stack client with its IPv6 address. A connection to a proxy
//     is left alone; which family the proxy uses is its own choice, and
//     an IPv6 answer is rejected later.
//
// dial is the underlying dialer (nil: the one http.DefaultTransport
// uses).
func newIPCheckTransport(tr *http.Transport, sites []ipCheckSite, dial dialFunc) *http.Transport {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	hosts := map[string]bool{}
	for _, s := range sites {
		if u, err := url.Parse(s.url); err == nil {
			hosts[u.Hostname()] = true
		}
	}
	c := tr.Clone()
	c.DisableKeepAlives = true
	c.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dial(ctx, ipCheckNetwork(network, addr, hosts), addr)
	}
	return c
}

// ipCheckNetwork is the network to dial addr on: "tcp4" for a check
// site, the requested one otherwise.
func ipCheckNetwork(network, addr string, hosts map[string]bool) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil && network == "tcp" && hosts[host] {
		return "tcp4"
	}
	return network
}

// PublicIPv4 returns the caller's public IPv4 address by issuing a
// short-timeout HTTP GET against config.CheckIPURL.
func (d *httpIPDiscoverer) PublicIPv4(ctx context.Context) (string, error) {
	return fetchCheckIP(ctx, d.client, d.sites[0].url)
}

// DetectPublicIPv4s asks every site IPCheckRounds times, all at once.
func (d *httpIPDiscoverer) DetectPublicIPv4s(ctx context.Context) (*DetectedIPs, error) {
	type answer struct {
		ip  netip.Addr
		err error
	}
	answers := make([]answer, IPCheckRounds*len(d.sites))
	var wg sync.WaitGroup
	for round := range IPCheckRounds {
		for i, site := range d.sites {
			wg.Go(func() {
				a := &answers[round*len(d.sites)+i]
				raw, err := fetchCheckIP(ctx, d.checkClient, site.url)
				if err == nil {
					a.ip, err = publicIPv4(raw)
				}
				a.err = err
			})
		}
	}
	wg.Wait()

	res := &DetectedIPs{Checks: len(answers)}
	seen := map[netip.Addr]bool{}
	var ips []netip.Addr
	// Failures in site order, then round order.
	for i, site := range d.sites {
		for round := range IPCheckRounds {
			a := answers[round*len(d.sites)+i]
			if a.err != nil {
				res.Failures = append(res.Failures, IPCheckFailure{Site: site.name, Err: a.err})
				continue
			}
			if !seen[a.ip] {
				seen[a.ip] = true
				ips = append(ips, a.ip)
			}
		}
	}
	if len(ips) == 0 {
		return res, fmt.Errorf("no check site answered with a public IPv4 address (%s)", strings.Join(res.FailureSummary(), "; "))
	}
	slices.SortFunc(ips, netip.Addr.Compare)
	for _, ip := range ips {
		res.IPs = append(res.IPs, ip.String())
	}
	return res, nil
}

// fetchCheckIP GETs target and returns the trimmed body.
func fetchCheckIP(ctx context.Context, client *http.Client, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the check site returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", errors.New("the check site returned an empty body")
	}
	return ip, nil
}

// sharedAddressSpace is RFC 6598's 100.64.0.0/10 (carrier-grade NAT):
// an address inside a carrier, never the one the WAF sees.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// publicIPv4 accepts an answer only if it is a public IPv4 address.
// checkip.amazonaws.com answers with the first X-Forwarded-For value
// when the request has one, so behind a TLS-intercepting proxy that
// adds the header it can name an internal address; the allowlist must
// not get that.
func publicIPv4(raw string) (netip.Addr, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("answered %q, which is not an IP address", raw)
	}
	a = a.Unmap()
	switch {
	case !a.Is4():
		return netip.Addr{}, fmt.Errorf("answered the IPv6 address %s; the allowlist takes IPv4 only", a)
	case !a.IsGlobalUnicast() || a.IsPrivate() || sharedAddressSpace.Contains(a):
		return netip.Addr{}, fmt.Errorf("answered %s, which is not a public address (a proxy may have added an X-Forwarded-For header)", a)
	}
	return a, nil
}
