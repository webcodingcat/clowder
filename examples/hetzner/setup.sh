#!/usr/bin/env bash
# Prepare a Hetzner host for clowder and make it the first host of a cluster:
# packages (DRBD 9 from LINBIT, clowderd from clowder's APT repository), the
# operator base that clowderd check expects, a thin pool per storage class, the
# Floating IP, then clowderd check and clowderd init.
#
# clowder-setup.service runs it at every boot until it has finished (the
# marker), so it may reboot in between; every step can run again. Its settings
# come from /etc/clowder-setup.env, the bootstrap key from a root-only file;
# cloud-init writes both (examples/hetzner).
# Usage: setup.sh
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

readonly SETTINGS=/etc/clowder-setup.env
readonly KEY_FILE=/etc/clowder/bootstrap-key
readonly STATE_DIR=/var/lib/clowder-setup
readonly MARKER="${STATE_DIR}/done"
readonly REBOOTS="${STATE_DIR}/reboots"
readonly MAX_REBOOTS=2
readonly METADATA=http://169.254.169.254/hetzner/v1/metadata
readonly LINBIT_KEY_URL=https://packages.linbit.com/package-signing-pubkey.asc
readonly LINBIT_FINGERPRINT=4E5385546726D13CB649872CFC05A31DB826FE48
# LINBIT publishes DRBD 9 for everyone only in its Proxmox repository; the one
# for Proxmox VE 9 is built for Debian 13. Plain HTTP is enough: apt checks the
# signature with the pinned key.
readonly LINBIT_SOURCE="http://packages.linbit.com/public proxmox-9 drbd-9"
readonly CLOWDER_KEY_URL=https://webcodingcat.github.io/clowder-apt/clowder.asc
readonly CLOWDER_FINGERPRINT=90936C85402C45ADCC58A8A2139D3911698DC23E
readonly CLOWDER_SOURCE="https://webcodingcat.github.io/clowder-apt trixie main"
# Docker's networks; the module keeps the private network clear of them
readonly DOCKER_POOLS=172.20.0.0/14
readonly NETPLAN_FLOATING_IP=/etc/netplan/60-clowder-floating-ip.yaml
# apt-daily and unattended-upgrades may hold dpkg's lock at the first boot
readonly APT=(apt-get -qq -o DPkg::Lock::Timeout=600)

# log <message>: Print a line to the journal.
log() {
  echo "clowder-setup: $*"
}

# wait_for <seconds> <what> <command...>: Run a command until it succeeds, at
# most for the given seconds.
wait_for() {
  local seconds="$1" what="$2"
  shift 2
  local deadline=$((SECONDS + seconds))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      log "gave up waiting for ${what} after ${seconds} s"
      return 1
    fi
    sleep 2
  done
}

# has_address <address>: Tell whether an interface holds the address.
has_address() {
  ip -4 -o addr show | grep -q " $1/"
}

# volume_device <volume id>: Print the device of a Hetzner volume.
volume_device() {
  echo "/dev/disk/by-id/scsi-0HC_Volume_$1"
}

# wait_for_hardware: Wait for the private address and the volumes, which
# Hetzner attaches after the server has booted.
wait_for_hardware() {
  local class_volume
  wait_for 300 "the private network" has_address "$(private_address)"
  for class_volume in ${VOLUMES}; do
    wait_for 300 "volume ${class_volume}" test -b "$(volume_device "${class_volume#*=}")"
  done
}

# private_address: Print the host's address in the private network.
private_address() {
  curl -fsS "${METADATA}/private-networks" | awk '/- ip:/ {print $3; exit}'
}

# install_headers: Install the headers of the running kernel for the DRBD
# module. When the mirror no longer has them (the image's kernel is old),
# upgrade and reboot into the current kernel, at most MAX_REBOOTS times.
install_headers() {
  local reboots=0 headers
  headers="linux-headers-$(uname -r)"
  # the image ships empty index files for trixie-security, which hides the
  # headers of the running kernel; fetch the lists anew
  rm -rf /var/lib/apt/lists/*
  "${APT[@]}" update
  # a package the mirror has must install; a failure then is no reason to reboot
  if apt-cache show "${headers}" >/dev/null 2>&1; then
    "${APT[@]}" install -y --no-install-recommends "${headers}"
    return
  fi
  [[ -f "${REBOOTS}" ]] && reboots="$(cat "${REBOOTS}")"
  if ((reboots >= MAX_REBOOTS)); then
    log "no headers for kernel $(uname -r) after ${reboots} reboots"
    return 1
  fi
  echo $((reboots + 1)) >"${REBOOTS}"
  log "no headers for kernel $(uname -r): upgrading and rebooting"
  # keep changed configuration files, without asking
  "${APT[@]}" -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold full-upgrade -y
  systemctl --no-block reboot
  exit 0
}

# configure_base: What clowderd check expects before the packages come in:
# Docker's daemon.json before Docker first starts, the sysctls, the watchdog,
# and unattended-upgrades leaving the kernel, Docker, DRBD and clowder alone.
configure_base() {
  mkdir -p /etc/docker
  cat >/etc/docker/daemon.json <<EOF
{
  "live-restore": true,
  "userland-proxy": false,
  "default-address-pools": [{"base": "${DOCKER_POOLS}", "size": 24}],
  "log-driver": "json-file",
  "log-opts": {"max-size": "10m", "max-file": "3"}
}
EOF
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
  printf '[Manager]\nRuntimeWatchdogSec=60\n' >/etc/systemd/system.conf.d/watchdog.conf
  systemctl daemon-reexec
  cat >/etc/apt/apt.conf.d/52clowder-unattended <<'EOF'
// the kernel, Docker, DRBD and clowder are updated only after clowder host drain
Unattended-Upgrade::Package-Blacklist { "linux-"; "docker"; "drbd"; "clowder"; };
EOF
}

# trust_key <url> <fingerprint> <keyring>: Fetch a signing key and keep only
# the key with the pinned fingerprint, as apt's keyring for one source.
trust_key() {
  local url="$1" fingerprint="$2" keyring="$3" home
  home="$(mktemp -d)"
  curl -fsSL "${url}" | gpg --homedir "${home}" --quiet --import
  if ! gpg --homedir "${home}" --list-keys --with-colons "${fingerprint}" | grep -q "^fpr:.*${fingerprint}"; then
    log "${url} does not hold the key ${fingerprint}"
    rm -rf "${home}"
    return 1
  fi
  gpg --homedir "${home}" --export "${fingerprint}" >"${keyring}"
  rm -rf "${home}"
}

# install_packages: LINBIT's DRBD 9 and clowderd with what it depends on, from
# clowder's APT repository or, with the test hook, from a checked .deb.
install_packages() {
  local package=clowderd
  "${APT[@]}" install -y --no-install-recommends gnupg curl
  trust_key "${LINBIT_KEY_URL}" "${LINBIT_FINGERPRINT}" /usr/share/keyrings/linbit.gpg
  echo "deb [signed-by=/usr/share/keyrings/linbit.gpg] ${LINBIT_SOURCE}" >/etc/apt/sources.list.d/linbit.list
  if [[ -n "${PACKAGE_URL}" ]]; then
    package=/var/cache/clowderd.deb
    curl -fsSL "${PACKAGE_URL}" -o "${package}"
    echo "${PACKAGE_SHA256}  ${package}" | sha256sum --check --quiet
  else
    trust_key "${CLOWDER_KEY_URL}" "${CLOWDER_FINGERPRINT}" /usr/share/keyrings/clowder.gpg
    echo "deb [signed-by=/usr/share/keyrings/clowder.gpg] ${CLOWDER_SOURCE}" >/etc/apt/sources.list.d/clowder.list
    [[ -n "${CLOWDERD_VERSION}" ]] && package="clowderd=${CLOWDERD_VERSION}"
  fi
  "${APT[@]}" update
  "${APT[@]}" install -y --no-install-recommends "${package}"
}

# configure_storage: LVM leaves DRBD's devices alone, DRBD resources start
# only through clowderd, and each storage class gets its thin pool.
configure_storage() {
  local class_volume
  if ! grep -q 'r|^/dev/drbd' /etc/lvm/lvm.conf; then
    sed -i '/^devices {/a\	global_filter = [ "r|^/dev/drbd.*|" ]' /etc/lvm/lvm.conf
  fi
  systemctl disable --now drbd.service 2>/dev/null || true
  # drbdadm reports every new metadata to LINBIT's usage survey by default
  sed -i 's/^\tusage-count yes;/\tusage-count no;/' /etc/drbd.d/global_common.conf
  # the edits match the packages' files as they are; clowderd check would
  # fail later, but this says what is wrong
  if ! grep -q 'r|^/dev/drbd' /etc/lvm/lvm.conf \
    || ! grep -q '^\s*usage-count no;' /etc/drbd.d/global_common.conf; then
    log "lvm.conf or DRBD's global_common.conf did not take the edit; their layout changed"
    return 1
  fi
  for class_volume in ${VOLUMES}; do
    create_pool "vg-${class_volume%%=*}" "$(volume_device "${class_volume#*=}")"
  done
}

# create_pool <vg> <device>: A volume group with one thin pool on a device
# (90 % of it; the rest is the metadata and the reserve).
create_pool() {
  local vg="$1" device="$2"
  vgs "${vg}" >/dev/null 2>&1 && return 0
  pvcreate -q "${device}"
  vgcreate -q "${vg}" "${device}"
  lvcreate -q -l 90%VG -T "${vg}/thin"
}

# configure_floating_ip: The Floating IP on the public interface, in a file of
# its own that cloud-init does not rewrite at boot.
configure_floating_ip() {
  local interface
  interface="$(ip -4 route show default | awk '{print $5; exit}')"
  cat >"${NETPLAN_FLOATING_IP}" <<EOF
# the cluster's Floating IP (clowder); clowder moves it between hosts
network:
  version: 2
  ethernets:
    ${interface}:
      addresses:
        - "${FLOATING_IP}/32"
EOF
  chmod 600 "${NETPLAN_FLOATING_IP}"
  netplan apply
}

# time_synchronized: Tell whether systemd-timesyncd has set the clock.
time_synchronized() {
  [[ "$(timedatectl show --property=NTPSynchronized --value)" == yes ]]
}

# init_cluster: clowderd check, then clowderd init, once.
init_cluster() {
  local public private class_volume pools=()
  public="$(curl -fsS "${METADATA}/public-ipv4")"
  private="$(private_address)"
  for class_volume in ${VOLUMES}; do
    pools+=(--pool "vg-${class_volume%%=*}/thin")
  done
  clowderd check "${pools[@]}" --floating-ip "${FLOATING_IP}"
  if [[ -f /var/lib/clowder/host.json ]]; then
    log "this host is in a cluster already"
    return 0
  fi
  if [[ -d /var/lib/clowder/raft ]]; then
    log "an earlier clowderd init stopped halfway; to start anew, remove /var/lib/clowder and reboot"
    return 1
  fi
  clowderd init --name "${NAME}" --underlay "${private}" --public-address "${public}" \
    --server-ref "$(curl -fsS "${METADATA}/instance-id")" --bootstrap-key-file "${KEY_FILE}"
}

# main: Do what the header of this script describes.
main() {
  # shellcheck source=/dev/null
  source "${SETTINGS}"
  mkdir -p "${STATE_DIR}"
  wait_for_hardware
  install_headers
  configure_base
  install_packages
  configure_storage
  configure_floating_ip
  wait_for 600 "the time sync" time_synchronized
  init_cluster
  # the key is in the cluster's state now (sealed) and in the user's files
  rm -f "${KEY_FILE}"
  touch "${MARKER}"
  log "done"
}

main "$@"
