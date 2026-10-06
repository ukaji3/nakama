# Installs Kiro CLI and Kiro Crew for the ubuntu user.
#
# Shared by the cloud-init user-data (where it runs by default) and by
# the `kiro` provisioning preset (where it runs on demand, e.g. for
# `nakama aws setup rerun`). Both contexts execute as root, so every
# step drops to the ubuntu user explicitly.
#
# Fragment contract: no shebang, no `set` of its own, and it must not
# abort the enclosing script. The outer `if` captures the failure and
# the caller reports it; the instance stays usable either way.

# The official installer puts binaries in ~/.local/bin but does not
# touch any shell rc file: it only prints the PATH line to add. Add it
# here so interactive logins (and `bash -lc` below) find kiro-cli.
nakama_kiro_path_line='export PATH="$HOME/.local/bin:$PATH"'
if ! grep -qF "${nakama_kiro_path_line}" /home/ubuntu/.bashrc 2>/dev/null; then
  printf '\n# Added by nakama: kiro-cli and Kiro Crew install into ~/.local/bin.\n%s\n' \
    "${nakama_kiro_path_line}" >> /home/ubuntu/.bashrc
  chown ubuntu:ubuntu /home/ubuntu/.bashrc
fi

if sudo -u ubuntu -H bash -lc '
    set -euo pipefail
    export PATH="$HOME/.local/bin:$PATH"

    echo "nakama: installing Kiro CLI"
    # pipefail matters here: without it a failed download would feed an
    # empty script to bash, which exits 0 and installs nothing.
    curl -fsSL https://cli.kiro.dev/install | bash

    echo "nakama: installing Kiro Crew"
    # `kiro-cli crew` installs Kiro Crew when it is missing and then runs
    # the Kiro Crew CLI with the remaining arguments ("Arguments forwarded
    # to the Kiro Crew CLI"). `-y` answers the install prompt; after `--`,
    # `--version` goes to kirocrew, which just prints its version, so the
    # step installs (or finds) Kiro Crew and exits 0. An earlier version
    # passed `install` there, which kirocrew rejects ("invalid choice"),
    # so the gateway service was never installed (Issue #79).
    "$HOME/.local/bin/kiro-cli" crew -y -- --version

    # `kirocrew service install` ships an AppArmor profile that carries
    # an unprivileged userns grant, and it refuses to install while
    # ~/.kiro is group- or world-writable: another local user could
    # then swap the executable at ~/.kiro/crew-venv/bin/kirocrew and
    # inherit that grant. The ubuntu user runs with umask 0002 (its
    # private group), so the tree is created 0775 and has to be
    # tightened before the service step.
    echo "nakama: tightening ~/.kiro for the AppArmor profile"
    chmod -R go-w "$HOME/.kiro"

    echo "nakama: installing the Kiro Crew gateway service"
    kirocrew service install
  '; then
  echo "nakama: Kiro CLI and Kiro Crew ready"
else
  echo "nakama: WARNING Kiro CLI / Kiro Crew setup failed; the instance is still usable." >&2
  echo "nakama: WARNING retry with \`nakama aws setup rerun <name> --preset kiro\`." >&2
fi
