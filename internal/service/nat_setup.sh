#!/bin/bash
# nakama NAT instance setup (Amazon Linux 2023, t4g.nano). Issue #87.
#
# Runs as the instance's user-data on first boot, and again through SSM
# Run Command when `nakama aws init` finds the forwarding rules missing.
# It is idempotent: every step checks before it changes anything.
#
# The instance has 412 MiB of RAM plus a zram swap of the same size,
# which zram backs with that same RAM. dnf with the full AL2023
# repository metadata needs more than that: `dnf install
# iptables-services` was OOM-killed with the swap exhausted, and a full
# `dnf upgrade` peaks around 560 MiB. So dnf runs only with a temporary
# swap file on disk, installs only what forwarding needs, and the run
# fails loudly instead of leaving the instance without NAT.
set -euo pipefail

log() { echo "nakama-nat: $*"; }

# --- Forwarding (no package needed; first, so it never depends on dnf) ---
cat >/etc/sysctl.d/99-nakama-nat.conf <<'EOF'
net.ipv4.ip_forward = 1
EOF
sysctl -q -p /etc/sysctl.d/99-nakama-nat.conf

# --- Temporary swap on disk while dnf runs -------------------------------
SWAPFILE=/var/tmp/nakama-nat.swap
cleanup_swap() {
  if swapon --show=NAME --noheadings 2>/dev/null | grep -qx "$SWAPFILE"; then
    swapoff "$SWAPFILE" || true
  fi
  rm -f "$SWAPFILE"
}
trap cleanup_swap EXIT
if ! swapon --show=NAME --noheadings 2>/dev/null | grep -qx "$SWAPFILE"; then
  rm -f "$SWAPFILE"
  # dd rather than fallocate: swapon rejects files with holes on some
  # file systems, and 1 GiB takes a few seconds on gp3.
  dd if=/dev/zero of="$SWAPFILE" bs=1M count=1024 status=none
  chmod 0600 "$SWAPFILE"
  mkswap "$SWAPFILE" >/dev/null
  # Lower priority than zram: the disk is used only once RAM-backed swap
  # is full.
  swapon -p 10 "$SWAPFILE"
fi

# dnf_retry runs dnf up to three times: a transient mirror or network
# error must not leave the instance half set up.
dnf_retry() {
  local n
  for n in 1 2 3; do
    if dnf -y --setopt=install_weak_deps=False "$@"; then
      return 0
    fi
    log "dnf $* failed (attempt $n/3)"
    sleep $((n * 5))
  done
  return 1
}

# --- Security updates (required) -----------------------------------------
# AL2023 locks dnf to the AMI's release: a plain `dnf upgrade` installs
# nothing. --releasever=latest applies the current release, and with it
# the security fixes published since the AMI was built.
dnf_retry --releasever=latest upgrade

# --- iptables (nf_tables backend) ----------------------------------------
# iptables-nft alone provides the iptables command. iptables-services is
# not needed: the rules are applied by nakama-nat.service on every boot
# rather than restored from a saved file.
if ! command -v iptables >/dev/null 2>&1; then
  dnf_retry install iptables-nft
fi
command -v iptables >/dev/null 2>&1 || { log "iptables is still missing"; exit 1; }
dnf clean all >/dev/null 2>&1 || true

# --- Rules, re-applied on every boot -------------------------------------
cat >/usr/local/sbin/nakama-nat-setup <<'EOF'
#!/bin/bash
set -euo pipefail
IFACE="$(ip route show default 0.0.0.0/0 | awk '{print $5; exit}')"
if [ -z "${IFACE}" ]; then
  IFACE="$(ip -o link show | awk -F': ' '$2 != "lo" {print $2; exit}')"
fi
iptables -t nat -C POSTROUTING -o "${IFACE}" -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -o "${IFACE}" -j MASQUERADE
iptables -C FORWARD -i "${IFACE}" -j ACCEPT 2>/dev/null || iptables -A FORWARD -i "${IFACE}" -j ACCEPT
iptables -C FORWARD -o "${IFACE}" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -A FORWARD -o "${IFACE}" -m state --state RELATED,ESTABLISHED -j ACCEPT
EOF
chmod 0755 /usr/local/sbin/nakama-nat-setup

cat >/etc/systemd/system/nakama-nat.service <<'EOF'
[Unit]
Description=nakama NAT instance forwarding rules
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/nakama-nat-setup
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable nakama-nat.service
# restart, not start: a unit that failed before (the Issue #87 state)
# must run again now that iptables exists.
systemctl restart nakama-nat.service

# --- Verify ---------------------------------------------------------------
[ "$(sysctl -n net.ipv4.ip_forward)" = 1 ] || { log "ip_forward is off"; exit 1; }
systemctl is-active --quiet nakama-nat.service || { log "nakama-nat.service is not active"; exit 1; }
iptables -t nat -S POSTROUTING | grep -q -- '-j MASQUERADE' || { log "MASQUERADE rule is missing"; exit 1; }
log "ready"
