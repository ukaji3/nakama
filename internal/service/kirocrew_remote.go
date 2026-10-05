package service

// Instance-side half of the Kiro Crew web access (Issue #71, ADR 0020).
//
// The browser reaches Kiro Crew only through code-server: a dedicated
// CloudFront distribution adds `X-Forwarded-Host: <proxy port>.<proxy
// domain>`, code-server's host-based proxy (proxy-domain) authenticates
// the request with its password cookie and forwards it untouched to a
// loopback-only nginx, and nginx normalises Host/Origin before handing
// it to the gateway. This file renders the nginx site and the scripts
// that install and remove it; everything is a pure function so the
// exact bytes sent to the instance are unit-tested.

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// kirocrewGatewayPort is the Kiro Crew gateway's loopback port
	// (KIROCREW_PORT default).
	kirocrewGatewayPort = 5476
	// kirocrewProxyPort is the nginx listener code-server forwards to.
	// It must differ from the gateway port: code-server's host-based
	// proxy targets this port, and nginx then talks to the gateway.
	kirocrewProxyPort = 15476
	// kirocrewProxyDomain is code-server's proxy-domain. It is never
	// resolved by DNS: the CloudFront distribution injects it as
	// X-Forwarded-Host, which code-server's getHost() prefers over Host.
	kirocrewProxyDomain = "kirocrew.nakama.internal"

	kirocrewNginxConfPath  = "/etc/nginx/conf.d/nakama-kirocrew.conf"
	kirocrewCodeServerConf = "/home/ubuntu/.config/code-server/config.yaml"

	// Markers the remote scripts print for the CLI to parse.
	kirocrewMarkerOK    = "NAKAMA_KIROCREW_OK"
	kirocrewMarkerError = "NAKAMA_KIROCREW_ERROR"
)

// kirocrewForwardedHost is the X-Forwarded-Host value the crew
// distribution sends; code-server maps it to kirocrewProxyPort.
func kirocrewForwardedHost() string {
	return fmt.Sprintf("%d.%s", kirocrewProxyPort, kirocrewProxyDomain)
}

// cloudFrontDomainPattern accepts only the default *.cloudfront.net
// names CloudFront assigns. The domain is interpolated into nginx,
// YAML and shell, so anything wider would be an injection vector.
var cloudFrontDomainPattern = regexp.MustCompile(`^d[a-z0-9]{6,32}\.cloudfront\.net$`)

func validateCloudFrontDomain(domain string) error {
	if !cloudFrontDomainPattern.MatchString(domain) {
		return fmt.Errorf("unexpected CloudFront domain %q", domain)
	}
	return nil
}

// kirocrewNginxConfig renders the loopback reverse proxy in front of
// the Kiro Crew gateway.
//
// Security notes, each pinned by a test:
//   - It listens on 127.0.0.1 only; code-server is the sole client.
//   - Origin is checked BEFORE it is rewritten. Rewriting Origin to the
//     gateway's own loopback origin is what lets Kiro Crew's CSRF and
//     WebSocket checks pass, so the check has to happen here or it
//     would not happen at all. Only the crew origin (or no Origin, as
//     on top-level navigations) gets through.
//   - X-Forwarded-For is deliberately left as received (CloudFront
//     sets it). Kiro Crew then keeps classifying these requests as
//     proxied, which keeps its config-write and secret-reveal endpoints
//     closed to the browser — the intended posture for a dashboard
//     reachable from outside the host.
//   - X-Forwarded-Proto: https is set because Kiro Crew trusts it only
//     from a loopback peer, which nginx is; the session cookies then
//     carry Secure, which mobile browsers need for the wss:// upgrade.
func kirocrewNginxConfig(crewDomain string) (string, error) {
	if err := validateCloudFrontDomain(crewDomain); err != nil {
		return "", err
	}
	gw := fmt.Sprintf("localhost:%d", kirocrewGatewayPort)
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by `+"`nakama aws kirocrew enable`"+`; removed by `+"`kirocrew disable`"+`.
# Loopback-only proxy between code-server's host-based proxy and the
# Kiro Crew gateway (nakama ADR 0020).

map $http_origin $nakama_kirocrew_origin_ok {
    ""                     1;
    "https://%[1]s"   1;
    default                0;
}

# Rewritten Origin: empty stays empty (nginx then omits the header).
map $http_origin $nakama_kirocrew_origin {
    ""      "";
    default "http://%[2]s";
}

map $http_upgrade $nakama_kirocrew_connection {
    default upgrade;
    ""      close;
}

server {
    listen 127.0.0.1:%[3]d;
    server_name _;
    # Keep rewritten redirects relative. With the default (on), nginx
    # expands "/" into an absolute URL built from its own listener,
    # handing the browser http://127.0.0.1:%[3]d/... (seen in testing).
    absolute_redirect off;

    location / {
        if ($nakama_kirocrew_origin_ok = 0) {
            return 403;
        }

        proxy_pass http://127.0.0.1:%[4]d;
        proxy_http_version 1.1;

        proxy_set_header Host %[2]s;
        proxy_set_header Origin $nakama_kirocrew_origin;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-Host %[1]s;
        # X-Forwarded-For is intentionally not overridden: see ADR 0020.

        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $nakama_kirocrew_connection;

        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering off;
        client_max_body_size 100m;

        proxy_redirect http://localhost:%[4]d/ /;
        proxy_redirect http://127.0.0.1:%[4]d/ /;
        proxy_redirect http://kirocrew.localhost:%[4]d/ /;
    }
}
`, crewDomain, gw, kirocrewProxyPort, kirocrewGatewayPort)
	return b.String(), nil
}

// kirocrewConfigEditor is the Python program both scripts use to edit
// code-server's config.yaml. It owns a marker-delimited block so the
// user's own settings are never touched, refuses to proceed when the
// user already configured proxy-domain or trusted-origins outside the
// block (two owners for one key would fight), keeps the file mode and
// owner, and never prints the file (it holds the password).
//
// Usage: python3 - <path> set <crew-domain> | remove
const kirocrewConfigEditor = `
import os, sys

path, action = sys.argv[1], sys.argv[2]
begin = "# BEGIN nakama kirocrew (managed by ` + "`nakama aws kirocrew enable`" + `)"
end = "# END nakama kirocrew"

with open(path, encoding="utf-8") as fh:
    lines = fh.read().splitlines()

kept, inside = [], False
for line in lines:
    if line.strip() == begin:
        inside = True
        continue
    if inside:
        if line.strip() == end:
            inside = False
        continue
    kept.append(line)
if inside:
    sys.exit("unterminated nakama kirocrew block in " + path)

for line in kept:
    key = line.split(":", 1)[0].strip()
    if key in ("proxy-domain", "trusted-origins") and not line.startswith((" ", "\t", "#")):
        if action == "set":
            sys.exit(key + " is already set in " + path + " outside the nakama block; refusing to override it")

if action == "set":
    domain = sys.argv[3]
    while kept and kept[-1] == "":
        kept.pop()
    kept += [begin, "proxy-domain: KIROCREW_PROXY_DOMAIN", "trusted-origins:", "  - " + domain, end]
elif action != "remove":
    sys.exit("unknown action " + action)

st = os.stat(path)
tmp = path + ".nakama.tmp"
with open(tmp, "w", encoding="utf-8") as fh:
    fh.write("\n".join(kept) + "\n")
os.chmod(tmp, st.st_mode & 0o7777)
os.chown(tmp, st.st_uid, st.st_gid)
os.replace(tmp, path)
`

func kirocrewEditorProgram() string {
	return strings.ReplaceAll(kirocrewConfigEditor, "KIROCREW_PROXY_DOMAIN", kirocrewProxyDomain)
}

// kirocrewEnableScript installs the nginx site and switches on
// code-server's host-based proxy for the crew distribution. It is
// idempotent. It never enables xtrace and never prints config.yaml,
// which holds the code-server password.
func kirocrewEnableScript(crewDomain string) (string, error) {
	nginx, err := kirocrewNginxConfig(crewDomain)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	fmt.Fprintf(&b, `fail() { echo "%s $*"; exit 1; }
`, kirocrewMarkerError)
	fmt.Fprintf(&b, `
# 1. Preconditions: the gateway must be installed and answering.
sudo -u ubuntu -H bash -lc 'export PATH="$HOME/.local/bin:$PATH"; command -v kirocrew' >/dev/null 2>&1 \
  || fail "kirocrew is not installed for the ubuntu user"
curl -fsS --max-time 10 http://127.0.0.1:%d/api/health >/dev/null 2>&1 \
  || fail "the Kiro Crew gateway is not answering on 127.0.0.1:%d"
[ -f %s ] || fail "code-server config not found"
command -v python3 >/dev/null 2>&1 || fail "python3 is required"
`, kirocrewGatewayPort, kirocrewGatewayPort, kirocrewCodeServerConf)

	b.WriteString(`
# 2. nginx, without the stock site (it would listen on 0.0.0.0:80).
if ! command -v nginx >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y -qq nginx >/dev/null \
    || fail "apt-get install nginx failed"
fi
rm -f /etc/nginx/sites-enabled/default
`)
	fmt.Fprintf(&b, `
# 3. The loopback proxy site.
cat > %s <<'NAKAMA_KIROCREW_NGINX'
%sNAKAMA_KIROCREW_NGINX
nginx -t >/dev/null 2>&1 || fail "nginx rejected the generated configuration"
systemctl enable nginx >/dev/null 2>&1
systemctl reload-or-restart nginx || fail "nginx failed to start"
`, kirocrewNginxConfPath, nginx)

	fmt.Fprintf(&b, `
# 4. code-server: host-based proxy for the crew distribution only.
if ! python3 - %s set %s <<'NAKAMA_KIROCREW_EDIT'
%s
NAKAMA_KIROCREW_EDIT
then
  fail "could not update the code-server config (see the message above)"
fi
systemctl restart code-server@ubuntu || fail "code-server failed to restart"
echo "%s"
`, kirocrewCodeServerConf, crewDomain, kirocrewEditorProgram(), kirocrewMarkerOK)
	return b.String(), nil
}

// kirocrewDisableScript removes what kirocrewEnableScript added. It is
// idempotent; the nginx package itself is left installed.
func kirocrewDisableScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	fmt.Fprintf(&b, `fail() { echo "%s $*"; exit 1; }
`, kirocrewMarkerError)
	fmt.Fprintf(&b, `
rm -f %s
if command -v nginx >/dev/null 2>&1; then
  if [ -z "$(ls -A /etc/nginx/conf.d /etc/nginx/sites-enabled 2>/dev/null)" ]; then
    systemctl disable --now nginx >/dev/null 2>&1 || true
  else
    nginx -t >/dev/null 2>&1 && systemctl reload nginx || true
  fi
fi
if [ -f %s ] && grep -qF "# BEGIN nakama kirocrew" %s; then
  if ! python3 - %s remove <<'NAKAMA_KIROCREW_EDIT'
%s
NAKAMA_KIROCREW_EDIT
  then
    fail "could not update the code-server config"
  fi
  systemctl restart code-server@ubuntu || fail "code-server failed to restart"
fi
echo "%s"
`, kirocrewNginxConfPath, kirocrewCodeServerConf, kirocrewCodeServerConf,
		kirocrewCodeServerConf, kirocrewEditorProgram(), kirocrewMarkerOK)
	return b.String()
}

// parseKirocrewScriptResult maps the script's markers to an error. The
// stdout is returned by runRemoteScript even when the command failed,
// so a NAKAMA_KIROCREW_ERROR line explains a non-zero exit.
func parseKirocrewScriptResult(stdout string, runErr error) error {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if reason, ok := strings.CutPrefix(line, kirocrewMarkerError+" "); ok {
			return fmt.Errorf("instance-side setup failed: %s", reason)
		}
	}
	if runErr != nil {
		return fmt.Errorf("instance-side setup failed: %w", runErr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == kirocrewMarkerOK {
			return nil
		}
	}
	return fmt.Errorf("instance-side setup did not report completion")
}
