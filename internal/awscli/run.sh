# Runs the AWS CLI for nakama's own calls from root scripts (Issue #101):
# the code-server password and version in the user-data, the version
# record of `nakama aws update`, the password swap after a restore.
#
# Fragment contract: no shebang and no `set` of its own; it only defines
# the function.
#
# The official install belongs to the ubuntu user, and anything running
# as ubuntu (a sandboxed agent included) can rewrite that tree, so it is
# never executed as root: it runs as
# ubuntu, with an empty environment, a HOME that does not exist and no
# AWS config or credentials files, so none of ubuntu's AWS settings
# apply and the call uses the instance role. A disk without the official
# install (an instance created before Issue #101, or restored from a
# backup of one) still has the snap, which is used instead.
nakama_aws() {
  local official=/home/ubuntu/.local/aws-cli/v2/current/bin/aws
  if [ -x "${official}" ]; then
    sudo -u ubuntu env -i PATH=/usr/bin:/bin HOME=/nonexistent \
      AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_PAGER= \
      "${official}" "$@"
  elif [ -x /snap/bin/aws ]; then
    /snap/bin/aws "$@"
  else
    echo "nakama: the AWS CLI is not installed" >&2
    return 127
  fi
}
