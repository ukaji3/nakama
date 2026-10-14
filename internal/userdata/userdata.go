// Package userdata renders cloud-init User Data scripts for nakama
// EC2 instances. The actual shell-script template lives next to this
// file as code-server.tmpl.sh and is embedded at build time.
package userdata

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/ukaji3/nakama/internal/kiro"
)

//go:embed code-server.tmpl.sh
var codeServerTemplate string

//go:embed bare.tmpl.sh
var bareTemplate string

//go:embed restore.tmpl.sh
var restoreTemplate string

// BareParams is the data passed to the bare cloud-init template
// (`nakama aws create --bare`, Issue #52).
type BareParams struct {
	// InstanceName is the nakama instance name.
	InstanceName string
	// Region is the AWS region.
	Region string
	// InstallKiro requests Kiro CLI and Kiro Crew (the default; turned
	// off by `create --no-kiro`, Issue #68).
	InstallKiro bool
}

// RenderBare returns the rendered cloud-init User Data shell script for
// a bare instance: no code-server, no password, just a reachable
// development box.
func RenderBare(p BareParams) (string, error) {
	t, err := template.New("bare").Parse(bareTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, bareTemplateData{
		BareParams: p,
		KiroScript: kiroScriptFor(p.InstallKiro),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// bareTemplateData is BareParams plus the fields the template needs but
// callers should not have to supply.
type bareTemplateData struct {
	BareParams
	// KiroScript is the shared Kiro install fragment, empty when the
	// instance was created with --no-kiro.
	KiroScript string
}

// kiroScriptFor returns the shared install fragment when requested.
// Keeping the decision here means both templates stay identical in
// behaviour and neither embeds its own copy of the steps.
func kiroScriptFor(install bool) string {
	if !install {
		return ""
	}
	return kiro.InstallScript()
}

// pinnedVersionPattern is the accepted shape of an explicit
// code-server version (plain semver, e.g. "4.96.4"). The value is
// interpolated into a double-quoted shell string inside the rendered
// User Data, so anything else is rejected up front.
var pinnedVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// ValidatePinnedVersion rejects --code-server-version values that are
// not plain X.Y.Z semver. Empty means "latest" and is allowed.
func ValidatePinnedVersion(v string) error {
	if v == "" {
		return nil
	}
	if !pinnedVersionPattern.MatchString(v) {
		return fmt.Errorf("invalid --code-server-version %q: expected X.Y.Z (e.g. 4.96.4)", v)
	}
	return nil
}

// CodeServerParams is the data passed to the code-server cloud-init
// template.
type CodeServerParams struct {
	// InstanceName is the nakama instance name (used to resolve the
	// SSM Parameter Store paths).
	InstanceName string
	// Region is the AWS region (used for `aws ssm` calls inside
	// the EC2 instance).
	Region string
	// Port is the TCP port on which code-server should listen.
	Port int
	// SSMPasswordParameter is the absolute SSM parameter name that
	// holds the code-server password (SecureString).
	SSMPasswordParameter string
	// SSMVersionParameter is the SSM parameter to which the running
	// code-server version is written back after install.
	SSMVersionParameter string
	// PinnedVersion is the optional explicit code-server version to
	// install. Empty means latest.
	PinnedVersion string
	// InstallKiro requests Kiro CLI and Kiro Crew (the default; turned
	// off by `create --no-kiro`, Issue #68).
	InstallKiro bool
}

// codeServerTemplateData is CodeServerParams plus the derived fields
// the template needs.
type codeServerTemplateData struct {
	CodeServerParams
	// KiroScript is the shared Kiro install fragment, empty when the
	// instance was created with --no-kiro.
	KiroScript string
}

// RenderCodeServer returns the rendered cloud-init User Data shell
// script for a code-server instance.
func RenderCodeServer(p CodeServerParams) (string, error) {
	if err := ValidatePinnedVersion(p.PinnedVersion); err != nil {
		return "", err
	}
	t, err := template.New("code-server").Parse(codeServerTemplate)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, codeServerTemplateData{
		CodeServerParams: p,
		KiroScript:       kiroScriptFor(p.InstallKiro),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RestoreScriptPath is where the restore user data places the
// first-boot script of a code-server instance.
const RestoreScriptPath = "/var/lib/nakama/restore-first-boot.sh"

// RestoreParams is the data for the user data of an instance that
// `nakama aws restore` rebuilds from a backup after the original was
// deleted (Issue #96).
type RestoreParams struct {
	InstanceName string
	Region       string
	// Bare is true for an instance without code-server: the user data
	// then only keeps the SSH host keys.
	Bare bool
	// SSMPasswordParameter holds the new code-server password, and
	// SSMVersionParameter receives the running version. Both are
	// required unless Bare.
	SSMPasswordParameter string
	SSMVersionParameter  string
}

// restoreCloudConfig is the subset of cloud-config the restore user data
// uses. It is marshalled rather than templated so that the embedded
// script is always quoted correctly.
type restoreCloudConfig struct {
	SSHDeleteKeys bool               `yaml:"ssh_deletekeys"`
	WriteFiles    []restoreWriteFile `yaml:"write_files,omitempty"`
	RunCmd        [][]string         `yaml:"runcmd,omitempty"`
}

type restoreWriteFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// restoreHeader explains the cloud-config to anyone who reads the user
// data of the instance.
const restoreHeader = `#cloud-config
# nakama user data for an instance restored from a backup (Issue #96).
#
# The root volume comes from the backup and already holds the user's
# system, so nothing is installed. cloud-init sees a new instance ID and
# would delete and regenerate the SSH host keys; ssh_deletekeys: false
# keeps the backup's keys, so known_hosts entries made by
# "nakama aws ssh setup" stay valid.
`

// RenderRestore returns the cloud-config user data for an instance
// restored from a backup. The restored volume has already run its own
// create-time user data, so this replaces it entirely: it keeps the SSH
// host keys and, for a code-server instance, swaps in the new password.
func RenderRestore(p RestoreParams) (string, error) {
	cfg := restoreCloudConfig{SSHDeleteKeys: false}
	if !p.Bare {
		if p.SSMPasswordParameter == "" || p.SSMVersionParameter == "" {
			return "", errors.New("restore user data: the SSM password and version parameters are required for a code-server instance")
		}
		t, err := template.New("restore").Parse(restoreTemplate)
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, p); err != nil {
			return "", err
		}
		cfg.WriteFiles = []restoreWriteFile{{
			Path:        RestoreScriptPath,
			Owner:       "root:root",
			Permissions: "0700",
			Content:     buf.String(),
		}}
		cfg.RunCmd = [][]string{{"bash", RestoreScriptPath}}
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("restore user data: %w", err)
	}
	return restoreHeader + string(body), nil
}
