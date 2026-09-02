// Package userdata renders cloud-init User Data scripts for nakama
// EC2 instances. The actual shell-script template lives next to this
// file as code-server.tmpl.sh and is embedded at build time.
package userdata

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"text/template"
)

//go:embed code-server.tmpl.sh
var codeServerTemplate string

//go:embed bare.tmpl.sh
var bareTemplate string

// BareParams is the data passed to the bare cloud-init template
// (`nakama aws create --bare`, Issue #52).
type BareParams struct {
	// InstanceName is the nakama instance name.
	InstanceName string
	// Region is the AWS region.
	Region string
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
	if err := t.Execute(&buf, p); err != nil {
		return "", err
	}
	return buf.String(), nil
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
	if err := t.Execute(&buf, p); err != nil {
		return "", err
	}
	return buf.String(), nil
}
