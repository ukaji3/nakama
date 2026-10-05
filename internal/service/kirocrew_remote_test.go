package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testCrewDomain = "d1234567abcdef.cloudfront.net"

func TestValidateCloudFrontDomain(t *testing.T) {
	for _, ok := range []string{testCrewDomain, "d2mbs1emwfkuok.cloudfront.net"} {
		if err := validateCloudFrontDomain(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	// Everything that could break out of nginx/YAML/shell quoting, or
	// point the proxy at a host we do not own, must be refused.
	for _, bad := range []string{
		"", "example.com", "d123.cloudfront.net.evil.com", "D1234567ABCDEF.cloudfront.net",
		"d1234567abcdef.cloudfront.net;", "d1234567abcdef.cloudfront.net\nx", `d1234567"abc.cloudfront.net`,
		"d1234567abcdef.cloudfront.net/", "evil.cloudfront.net", "d1234567abcdef.cloudfront.net 1",
	} {
		if err := validateCloudFrontDomain(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if _, err := kirocrewNginxConfig("evil.example"); err == nil {
		t.Error("kirocrewNginxConfig must validate its input")
	}
	if _, err := kirocrewEnableScript("evil.example"); err == nil {
		t.Error("kirocrewEnableScript must validate its input")
	}
}

func TestKirocrewNginxConfigSecurityProperties(t *testing.T) {
	conf, err := kirocrewNginxConfig(testCrewDomain)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"listen 127.0.0.1:15476;",
		`"https://` + testCrewDomain + `"   1;`,
		"default                0;",
		"return 403;",
		"proxy_pass http://127.0.0.1:5476;",
		"proxy_set_header Host localhost:5476;",
		"proxy_set_header Origin $nakama_kirocrew_origin;",
		"proxy_set_header X-Forwarded-Proto https;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $nakama_kirocrew_connection;",
		"absolute_redirect off;",
		"proxy_redirect http://localhost:5476/ /;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx config lacks %q", want)
		}
	}
	// Exactly one listener, and it is loopback.
	if n := strings.Count(conf, "listen "); n != 1 {
		t.Errorf("nginx config has %d listen directives, want 1", n)
	}
	// X-Forwarded-For must be left as received so Kiro Crew keeps the
	// request classified as proxied (config writes stay closed).
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "proxy_set_header X-Forwarded-For") || strings.HasPrefix(trimmed, "proxy_set_header X-Real-IP") {
			t.Errorf("nginx must not rewrite client-address headers: %q", trimmed)
		}
	}
	// The Origin check has to run before the rewrite, i.e. inside the
	// location ahead of proxy_pass.
	if strings.Index(conf, "return 403;") > strings.Index(conf, "proxy_pass") {
		t.Error("the Origin check must precede proxy_pass")
	}
}

func TestKirocrewScriptsAreSafeBash(t *testing.T) {
	enable, err := kirocrewEnableScript(testCrewDomain)
	if err != nil {
		t.Fatal(err)
	}
	disable := kirocrewDisableScript()
	for name, s := range map[string]string{"enable": enable, "disable": disable} {
		if !strings.HasPrefix(s, "#!/bin/bash\n") {
			t.Errorf("%s: must start with a bash shebang (Run Command executes the file with sh)", name)
		}
		if strings.Contains(s, "set -x") || strings.Contains(s, "set -euxo") {
			t.Errorf("%s: xtrace would print config.yaml contents (the password) into the command history", name)
		}
		if strings.Contains(s, "cat "+kirocrewCodeServerConf) {
			t.Errorf("%s: must never print config.yaml", name)
		}
		if !strings.Contains(s, kirocrewMarkerOK) {
			t.Errorf("%s: must report completion with %s", name, kirocrewMarkerOK)
		}
		if _, err := exec.LookPath("bash"); err == nil {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: bash -n: %v\n%s", name, err, out)
			}
		}
	}
	for _, want := range []string{
		"rm -f /etc/nginx/sites-enabled/default", // no 0.0.0.0:80 listener
		"DPkg::Lock::Timeout=300",
		"<<'NAKAMA_KIROCREW_NGINX'", // quoted: no outer expansion of $variables
		"nginx -t",
		"systemctl restart code-server@ubuntu",
		"/api/health",
	} {
		if !strings.Contains(enable, want) {
			t.Errorf("enable script lacks %q", want)
		}
	}
}

// runEditor executes the real config editor (python3) against a file.
func runEditor(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"-", path}, args...)...)
	cmd.Stdin = strings.NewReader(kirocrewEditorProgram())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestKirocrewConfigEditor(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	const original = "bind-addr: 0.0.0.0:8080\nauth: password\npassword: s3cr3t-value\ncert: false\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runEditor(t, path, "set", testCrewDomain)
	if err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("the editor printed the password: %q", out)
	}
	got, _ := os.ReadFile(path)
	want := original +
		"# BEGIN nakama kirocrew (managed by `nakama aws kirocrew enable`)\n" +
		"proxy-domain: kirocrew.nakama.internal\n" +
		"trusted-origins:\n" +
		"  - " + testCrewDomain + "\n" +
		"# END nakama kirocrew\n"
	if string(got) != want {
		t.Errorf("after set:\n%s\nwant:\n%s", got, want)
	}
	// Windows has no POSIX mode bits to preserve; the editor only ever
	// runs on the Ubuntu instance, where this check is the one that
	// matters.
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 (the file holds the password)", st.Mode().Perm())
	}

	// Idempotent: a second set with another domain replaces the block.
	if out, err := runEditor(t, path, "set", "d7654321fedcba.cloudfront.net"); err != nil {
		t.Fatalf("second set: %v\n%s", err, out)
	}
	got, _ = os.ReadFile(path)
	if strings.Count(string(got), "# BEGIN nakama kirocrew") != 1 || strings.Contains(string(got), testCrewDomain) {
		t.Errorf("set must replace, not append:\n%s", got)
	}

	// remove restores the original byte for byte.
	if out, err := runEditor(t, path, "remove"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	got, _ = os.ReadFile(path)
	if string(got) != original {
		t.Errorf("after remove:\n%q\nwant:\n%q", got, original)
	}
	if out, err := runEditor(t, path, "remove"); err != nil {
		t.Errorf("remove must be idempotent: %v\n%s", err, out)
	}
}

func TestKirocrewConfigEditorRefusesForeignSettings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not available")
	}
	for _, foreign := range []string{
		"proxy-domain: example.com\n",
		"trusted-origins:\n  - example.com\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		content := "bind-addr: 0.0.0.0:8080\n" + foreign
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runEditor(t, path, "set", testCrewDomain)
		if err == nil {
			t.Errorf("set must refuse when the user configured %q themselves", foreign)
		}
		if !strings.Contains(out, "refusing") {
			t.Errorf("the refusal should explain itself, got %q", out)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("a refused set must leave the file untouched:\n%s", got)
		}
	}
}

func TestParseKirocrewScriptResult(t *testing.T) {
	if err := parseKirocrewScriptResult("noise\n"+kirocrewMarkerOK+"\n", nil); err != nil {
		t.Errorf("OK marker must succeed: %v", err)
	}
	err := parseKirocrewScriptResult(kirocrewMarkerError+" kirocrew is not installed for the ubuntu user\n", errors.New("exit 1"))
	if err == nil || !strings.Contains(err.Error(), "kirocrew is not installed") {
		t.Errorf("the remote reason must surface, got %v", err)
	}
	if err := parseKirocrewScriptResult("", errors.New("exit 1")); err == nil {
		t.Error("a failed command without a marker must still fail")
	}
	if err := parseKirocrewScriptResult("partial output\n", nil); err == nil {
		t.Error("a missing completion marker must fail (the script did not run to the end)")
	}
}
