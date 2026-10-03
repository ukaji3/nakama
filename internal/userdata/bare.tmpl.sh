#!/bin/bash
# nakama User Data script for a bare instance on Ubuntu 26.04
# Rendered from internal/userdata/bare.tmpl.sh
#
# A bare instance carries no browser IDE, no CloudFront distribution and
# no password: it is reached with `nakama aws connect`, `nakama aws ssh`
# and `nakama aws tunnel` (Issue #52). This script therefore only makes
# the instance a usable development box.
#
# Template variables: InstanceName, Region (Go text/template).

set -euxo pipefail
exec > >(tee -a /var/log/nakama-userdata.log) 2>&1

# cloud-init on Ubuntu 26.04 invokes user-data without exporting HOME,
# but apt-driven triggers below read $HOME.
export HOME="${HOME:-/root}"

INSTANCE_NAME="{{.InstanceName}}"
REGION="{{.Region}}"
TARGET_USER="ubuntu"

echo "nakama: bare instance ${INSTANCE_NAME} in ${REGION}"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# The AWS CLI is not required by nakama itself (which speaks the AWS
# APIs natively), but a development box is expected to have it, and the
# provisioning steps of `create --setup` may use it. Same source as the
# regular image: the classic snap, after snapd seeding completes.
apt-get purge -y -qq awscli || true
snap wait system seed.loaded
snap install aws-cli --classic
ln -sf /snap/bin/aws /usr/local/bin/aws

# Baseline tooling a developer expects to find on a fresh box.
apt-get install -y -qq git curl unzip unar jq build-essential

# Session Manager (and therefore `connect` / `tunnel` / `ssh`) needs the
# SSM agent. Ubuntu images ship it as a snap; make sure it is enabled so
# the instance is reachable even if the image changes defaults.
snap start amazon-ssm-agent 2>/dev/null || systemctl enable --now snap.amazon-ssm-agent.amazon-ssm-agent.service 2>/dev/null || true

# `nakama aws ssh` authorizes keys for the ubuntu user, so sshd must be
# running (it listens on the instance only: no inbound path exists from
# outside the VPC).
systemctl enable --now ssh || true

chown -R "${TARGET_USER}:${TARGET_USER}" "/home/${TARGET_USER}" || true

{{if .InstallKiro}}# Kiro CLI and Kiro Crew (Issue #68). Rendered from
# internal/kiro/install.sh, shared with the `kiro` provisioning preset.
# Placed after the chown above so the tightened ~/.kiro permissions the
# AppArmor profile requires are not reset, and best-effort: a failure
# here leaves a usable box.
{{.KiroScript}}
{{end}}echo "nakama: bare instance ready"
