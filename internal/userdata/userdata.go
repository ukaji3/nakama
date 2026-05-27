// Package userdata renders cloud-init User Data scripts for nakama
// EC2 instances. The actual shell-script template lives next to this
// file as code-server.tmpl.sh and is embedded at build time.
package userdata

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed code-server.tmpl.sh
var codeServerTemplate string

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
