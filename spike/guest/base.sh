#!/usr/bin/env bash
# Prepare a spike host: packages (DRBD 9 from LINBIT's proxmox-9 dist), the
# operator base from the plan, and the two LVM thin pools.
# Usage: base.sh <bulk volume id> <fast volume id> <floating ip>
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

readonly LINBIT_KEY_URL=https://packages.linbit.com/package-signing-pubkey.asc
readonly LINBIT_REPO="http://packages.linbit.com/public proxmox-9 drbd-9"

# install_packages: Debian packages plus drbd-dkms and drbd-utils from LINBIT;
# coccinelle lets the DKMS build run without LINBIT's online patch service.
install_packages() {
  # the image ships empty index files for trixie-security, which hides the
  # headers of the running kernel; fetch the lists anew
  rm -rf /var/lib/apt/lists/*
  apt-get update -qq
  # curl is in the image; apt reads an ASCII-armoured key directly (.asc)
  curl -fsSL "${LINBIT_KEY_URL}" -o /usr/share/keyrings/linbit.asc
  echo "deb [signed-by=/usr/share/keyrings/linbit.asc] ${LINBIT_REPO}" \
    >/etc/apt/sources.list.d/linbit.list
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends \
    "linux-headers-$(uname -r)" linux-headers-amd64 gcc libc6-dev make coccinelle dkms \
    drbd-dkms drbd-utils docker.io docker-cli docker-compose wireguard-tools nftables \
    iptables conntrack lvm2 thin-provisioning-tools e2fsprogs tcpdump socat \
    python3 jq >/dev/null
}

# configure_base <floating ip>: sysctls, watchdog, Docker, LVM filter, DRBD
# autostart off, the Floating IP on the public interface.
configure_base() {
  local floating_ip="$1" public_if
  cat >/etc/sysctl.d/90-clowder.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv4.conf.all.rp_filter = 2
net.ipv4.conf.default.rp_filter = 2
net.ipv4.conf.all.src_valid_mark = 1
EOF
  sysctl -q --system
  echo softdog >/etc/modules-load.d/softdog.conf
  modprobe softdog
  mkdir -p /etc/systemd/system.conf.d
  printf '[Manager]\nRuntimeWatchdogSec=60\n' \
    >/etc/systemd/system.conf.d/watchdog.conf
  systemctl daemon-reexec
  mkdir -p /etc/docker
  cat >/etc/docker/daemon.json <<'EOF'
{
  "live-restore": true,
  "userland-proxy": false,
  "default-address-pools": [{"base": "172.20.0.0/14", "size": 24}],
  "log-driver": "json-file",
  "log-opts": {"max-size": "10m", "max-file": "3"}
}
EOF
  systemctl restart docker
  if ! grep -q 'r|^/dev/drbd' /etc/lvm/lvm.conf; then
    sed -i '/^devices {/a\	global_filter = [ "r|^/dev/drbd.*|" ]' /etc/lvm/lvm.conf
  fi
  systemctl disable --now drbd.service 2>/dev/null || true
  # drbdadm reports every new metadata to LINBIT's usage survey by default
  sed -i 's/^\tusage-count yes;/\tusage-count no;/' /etc/drbd.d/global_common.conf
  public_if="$(ip -4 route show default | awk '{print $5; exit}')"
  ip addr replace "${floating_ip}/32" dev "${public_if}"
}

# create_pool <vg> <volume id>: A volume group with one thin pool on a
# Hetzner volume (90 % of it; the rest is the metadata and the reserve).
create_pool() {
  local vg="$1" volume_id="$2" disk
  disk="/dev/disk/by-id/scsi-0HC_Volume_${volume_id}"
  vgs "${vg}" >/dev/null 2>&1 && return 0
  pvcreate -q "${disk}"
  vgcreate -q "${vg}" "${disk}"
  lvcreate -q -l 90%VG -T "${vg}/thin"
}

main() {
  local bulk_id="$1" fast_id="$2" floating_ip="$3"
  install_packages
  configure_base "${floating_ip}"
  create_pool vg-bulk "${bulk_id}"
  create_pool vg-fast "${fast_id}"
  modprobe drbd
  echo "drbd: $(modinfo -F version drbd), drbd-utils: $(drbdadm --version \
    | awk -F= '/DRBDADM_VERSION=/{print $2}'), kernel: $(uname -r)"
}

main "$@"
