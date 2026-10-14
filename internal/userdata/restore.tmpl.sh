#!/bin/bash
# nakama: first boot of a code-server instance restored from a backup
# (Issue #96). Rendered from internal/userdata/restore.tmpl.sh and run by
# cloud-init (runcmd), which runs it once per instance ID.
#
# The instance boots it twice: first on the fresh Ubuntu volume it is
# launched with (no code-server there, so this exits at once; restore
# then discards that volume), and again on the volume restored from the
# backup, the first time the user starts the instance.
#
# The restored volume already holds code-server, its settings and the
# Kiro tools as they were at backup time, so nothing is reinstalled. Only
# the password changes: the old one went away with the deleted instance's
# SSM parameter, and restore stored a new one.
#
# Never enable xtrace here: the script handles the password (CWE-532).
set -euo pipefail
exec >>/var/log/nakama-restore.log 2>&1

INSTANCE_NAME="{{.InstanceName}}"
REGION="{{.Region}}"
PASSWORD_PARAM="{{.SSMPasswordParameter}}"
VERSION_PARAM="{{.SSMVersionParameter}}"
TARGET_USER="ubuntu"
CONFIG="/home/${TARGET_USER}/.config/code-server/config.yaml"

echo "nakama restore: first boot of ${INSTANCE_NAME} at $(date -u +%FT%TZ)"
if [ ! -f "${CONFIG}" ]; then
  echo "nakama restore: ${CONFIG} not found; nothing to do"
  exit 0
fi

AWS=/snap/bin/aws
[ -x "${AWS}" ] || AWS=aws

# `nakama aws start` starts the NAT instance first, but SSM can still be
# a moment away; retry for up to five minutes.
PASSWORD=""
for _ in $(seq 1 30); do
  if PASSWORD="$("${AWS}" ssm get-parameter \
      --region "${REGION}" \
      --name "${PASSWORD_PARAM}" \
      --with-decryption \
      --query Parameter.Value \
      --output text)" && [ -n "${PASSWORD}" ]; then
    break
  fi
  PASSWORD=""
  sleep 10
done
if [ -z "${PASSWORD}" ]; then
  echo "nakama restore: FATAL: could not read ${PASSWORD_PARAM}; code-server keeps the password of the backup" >&2
  exit 1
fi

# Replace only the password line and keep the rest of the user's config.
TMP="$(mktemp "${CONFIG}.XXXXXX")"
grep -v '^password:' "${CONFIG}" > "${TMP}" || true
printf 'password: %s\n' "${PASSWORD}" >> "${TMP}"
unset PASSWORD
chmod 600 "${TMP}"
chown "${TARGET_USER}:${TARGET_USER}" "${TMP}"
mv -f "${TMP}" "${CONFIG}"
systemctl restart "code-server@${TARGET_USER}"
echo "nakama restore: code-server password replaced"

# The version parameter went away with the delete; record it again
# (best effort, as the create user data does).
if VERSION_LINE="$(sudo -u "${TARGET_USER}" code-server --version 2>/dev/null | head -n1)"; then
  RUNNING_VERSION="$(echo "${VERSION_LINE}" | awk '{print $1}')"
  if [ -n "${RUNNING_VERSION}" ]; then
    "${AWS}" ssm put-parameter \
      --region "${REGION}" \
      --name "${VERSION_PARAM}" \
      --type String \
      --overwrite \
      --value "${RUNNING_VERSION}" >/dev/null || true
  fi
fi
echo "nakama restore: finished"
