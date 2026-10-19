# Installs or updates the AWS CLI v2 for the ubuntu user (Issue #101).
#
# Fragment contract: no shebang and no `set` of its own. It only defines
# the function below; the including script (cloud-init user-data or a
# Run Command script, both running as root) calls it. The function
# returns non-zero when the AWS CLI could not be installed, so a caller
# under `set -e` stops there, as it did when `snap install` failed.
#
# Why not the snap: a snap starts through the setuid snap-confine, which
# cannot run inside an agent sandbox built on an unprivileged user
# namespace, where setuid has no effect. An AWS CLI from AWS's own
# installer runs there.
#
# AWS's install script (the recommended method in the AWS CLI user
# guide) picks the x86_64 or aarch64 zip from `uname -m`, verifies its
# PGP signature, and installs into $XDG_DATA_HOME/aws-cli with the `aws`
# link in $XDG_BIN_HOME. Run again, it updates an older install and does
# nothing when the latest version is already there.
nakama_install_awscli() {
  # ~/.local/bin is not on the PATH of non-login shells (the browser IDE's
  # terminal). Other nakama installs into ~/.local/bin add the same line,
  # so whichever runs first adds it once.
  local path_line='export PATH="$HOME/.local/bin:$PATH"'
  if ! grep -qF "${path_line}" /home/ubuntu/.bashrc 2>/dev/null; then
    printf '\n# Added by nakama: tools such as the AWS CLI install into ~/.local/bin.\n%s\n' \
      "${path_line}" >> /home/ubuntu/.bashrc
    chown ubuntu:ubuntu /home/ubuntu/.bashrc
  fi

  # The install script verifies the signature only when gpg is present,
  # and otherwise just warns; nakama does not install unverified.
  if ! command -v gpg >/dev/null 2>&1; then
    echo "nakama: gpg is required to verify the AWS CLI download; install gnupg" >&2
    return 1
  fi

  echo "nakama: installing the AWS CLI v2 into /home/ubuntu/.local/aws-cli"
  # The script is downloaded to a file before it runs: piped into bash, a
  # download cut short would run a partial script.
  sudo -u ubuntu -H bash -c '
    set -euo pipefail
    tmp="$(mktemp -d)"
    trap "rm -rf \"$tmp\"" EXIT
    curl -fsSL https://awscli.amazonaws.com/v2/install.sh -o "$tmp/install.sh"
    XDG_DATA_HOME="$HOME/.local" XDG_BIN_HOME="$HOME/.local/bin" bash "$tmp/install.sh" --quiet
    "$HOME/.local/bin/aws" --version
  '
}
