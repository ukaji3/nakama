#!/bin/bash
# nakama User Data script for code-server on Ubuntu 26.04
# Rendered from internal/userdata/code-server.tmpl.sh
#
# Template variables (Go text/template syntax):
#   {{`{{.InstanceName}}`}}, {{`{{.Region}}`}}, {{`{{.Port}}`}},
#   {{`{{.SSMPasswordParameter}}`}}, {{`{{.SSMVersionParameter}}`}},
#   {{`{{.PinnedVersion}}`}}

set -euxo pipefail
exec > >(tee -a /var/log/nakama-userdata.log) 2>&1

INSTANCE_NAME="{{.InstanceName}}"
REGION="{{.Region}}"
PORT="{{.Port}}"
PASSWORD_PARAM="{{.SSMPasswordParameter}}"
VERSION_PARAM="{{.SSMVersionParameter}}"
PINNED_VERSION="{{.PinnedVersion}}"
TARGET_USER="ubuntu"
HOME_DIR="/home/${TARGET_USER}"

# 1. Wait for cloud-init networking, apt and AWS CLI
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y curl ca-certificates jq awscli

# 2. Pull the password from SSM Parameter Store (SecureString).
PASSWORD="$(aws ssm get-parameter \
  --region "${REGION}" \
  --name "${PASSWORD_PARAM}" \
  --with-decryption \
  --query Parameter.Value \
  --output text)"

if [ -z "${PASSWORD}" ]; then
  echo "FATAL: empty password retrieved from ${PASSWORD_PARAM}" >&2
  exit 1
fi

# 3. Install code-server via the official install script.
INSTALL_FLAGS=()
if [ -n "${PINNED_VERSION}" ]; then
  INSTALL_FLAGS+=(--version "${PINNED_VERSION}")
fi
curl -fsSL https://code-server.dev/install.sh \
  | sh -s -- "${INSTALL_FLAGS[@]}"

# 4. Drop the runtime config (auth=password, plain HTTP — TLS terminates
#    at CloudFront).
mkdir -p "${HOME_DIR}/.config/code-server"
cat > "${HOME_DIR}/.config/code-server/config.yaml" <<EOF
bind-addr: 0.0.0.0:${PORT}
auth: password
password: ${PASSWORD}
cert: false
EOF
chown -R "${TARGET_USER}:${TARGET_USER}" "${HOME_DIR}/.config"

# 5. Enable and start the systemd unit (template form: code-server@<user>).
systemctl daemon-reload
systemctl enable --now "code-server@${TARGET_USER}"

# 6. Record the installed version back to SSM (best effort).
if VERSION_LINE="$(sudo -u "${TARGET_USER}" code-server --version 2>/dev/null | head -n1)"; then
  RUNNING_VERSION="$(echo "${VERSION_LINE}" | awk '{print $1}')"
  if [ -n "${RUNNING_VERSION}" ]; then
    aws ssm put-parameter \
      --region "${REGION}" \
      --name "${VERSION_PARAM}" \
      --type String \
      --overwrite \
      --value "${RUNNING_VERSION}" || true
  fi
fi

echo "nakama User Data finished for instance: ${INSTANCE_NAME}"
