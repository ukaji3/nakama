package awsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	nakamaConfig "github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/proxy"
)

var errEmptyAMIParameter = errors.New("ssm parameter for ubuntu ami returned empty value")

// IPDiscoverer returns the caller's public IPv4 address.
//
// Implementations are expected to be deterministic with respect to
// the network configuration and to fail rather than guess.
type IPDiscoverer interface {
	PublicIPv4(ctx context.Context) (string, error)
}

type httpIPDiscoverer struct {
	client *http.Client
	url    string
}

// NewHTTPIPDiscoverer returns the production IPDiscoverer that calls
// the AWS-published checkip endpoint (config.CheckIPURL). The client
// resolves proxies via internal/proxy (env vars first, then OS
// settings) instead of the default http.ProxyFromEnvironment.
func NewHTTPIPDiscoverer() IPDiscoverer {
	return &httpIPDiscoverer{
		client: &http.Client{Timeout: 5 * time.Second, Transport: proxy.NewTransport()},
		url:    nakamaConfig.CheckIPURL,
	}
}

// PublicIPv4 returns the caller's public IPv4 address by issuing a
// short-timeout HTTP GET against config.CheckIPURL.
func (d *httpIPDiscoverer) PublicIPv4(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return "", err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checkip endpoint returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", errors.New("checkip endpoint returned empty body")
	}
	return ip, nil
}
