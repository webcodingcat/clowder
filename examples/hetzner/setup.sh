#!/usr/bin/env bash
# Prepare a Hetzner host for clowder and make it a host of a cluster: packages
# (DRBD 9 from LINBIT, clowderd from clowder's APT repository), the operator
# base that clowderd check expects, a thin pool per storage class, the
# Floating IP, then clowderd check and clowderd init (the first host) or
# clowderd join (the others).
#
# clowder-setup.service runs it at every boot until it has finished (the
# marker), so it may reboot in between; every step can run again, and what a
# prepared image holds already is skipped. Its settings come from
# /etc/clowder-setup.env (the first host's with the cluster's name), the join
# token (the others) from a root-only file; cloud-init writes them
# (examples/hetzner).
#
# With PREPARE_ONLY=yes in the settings it prepares an image instead: the
# kernel's headers, the operator base and what clowderd depends on (DRBD's
# module, Docker), without clowderd and without a cluster; then it leaves the
# marker PREPARED. clowder's live tests make their hosts' image so.
# Usage: setup.sh
set -euo pipefail
# a failure in $(...) fails the assignment too (fetch_clowderd)
shopt -s inherit_errexit
export DEBIAN_FRONTEND=noninteractive

readonly SETTINGS=/etc/clowder-setup.env
readonly TOKEN_FILE=/etc/clowder/join-token
readonly STATE_DIR=/var/lib/clowder-setup
readonly MARKER="${STATE_DIR}/done"
readonly PREPARED="${STATE_DIR}/prepared"
readonly PACKAGE_FILE=/var/cache/clowderd.deb
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

# metadata <path> [<pattern>]: Print a value of Hetzner's metadata service,
# read once and kept from then on: once clowderd runs, its nft rules close the
# service to the host (a joining host's user data holds its token), and the setup
# runs again at a boot until its marker exists. An answer that is empty or
# does not match the pattern (an extended regex) fails and is not kept: until
# Hetzner has attached the private network, private-networks is [], and a
# kept [] would hide the address at every later boot.
metadata() {
  local path="$1" pattern="${2:-.}" kept="${STATE_DIR}/metadata/$1" answer
  if [[ ! -s "${kept}" ]]; then
    # the time limit lets wait_for's deadline hold while the service hangs
    answer="$(curl -fsS --max-time 10 "${METADATA}/${path}")"
    if ! grep -qE -- "${pattern}" <<<"${answer}"; then
      # on stderr: callers take the output as the value
      log "the metadata service has no ${path} yet" >&2
      return 1
    fi
    mkdir -p "${STATE_DIR}/metadata"
    printf '%s\n' "${answer}" >"${kept}.new"
    mv "${kept}.new" "${kept}"
  fi
  cat "${kept}"
}

# private_address: Print the host's address in the private network.
private_address() {
  metadata private-networks '- ip: ' | awk '/- ip: / {print $3; exit}'
}

# has_private_address: Tell whether the metadata service names the host's
# private address yet and an interface holds it.
has_private_address() {
  local address
  address="$(private_address)" && has_address "${address}"
}

# wait_for_hardware: Wait for the private address and the volumes, which
# Hetzner attaches after the server has booted.
wait_for_hardware() {
  local class_volume
  wait_for 300 "the private network" has_private_address
  for class_volume in ${VOLUMES}; do
    wait_for 300 "volume ${class_volume}" test -b "$(volume_device "${class_volume#*=}")"
  done
}

# install_headers: Install the headers of the running kernel for the DRBD
# module. When the mirror no longer has them (the image's kernel is old),
# upgrade and reboot into the current kernel, at most MAX_REBOOTS times. A
# prepared image has them already, with DRBD's module built for them.
install_headers() {
  local reboots=0 headers
  headers="linux-headers-$(uname -r)"
  if dpkg-query -W -f '${db:Status-Status}' "${headers}" 2>/dev/null | grep -qx installed; then
    return
  fi
  # the image ships empty index files for trixie-security, which hides the
  # headers of the running kernel; fetch the lists anew. Only the index
  # files go: apt's lock and partial/ stay, so another apt-get update at boot
  # (HLab's guest agent, apt-daily) finishes before ours instead of breaking
  find /var/lib/apt/lists -maxdepth 1 -type f ! -name lock -delete
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
// the kernel, Docker, DRBD and clowder are updated by hand, in a maintenance
// window: their updates restart the stacks or the host
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

# add_sources: LINBIT's repository for DRBD 9 and, without the test hook,
# clowder's; then the package lists.
add_sources() {
  # the lists may be from a run that stopped halfway, or broken by another
  # apt-get update at boot; install_headers fetches them only once
  "${APT[@]}" update
  "${APT[@]}" install -y --no-install-recommends gnupg curl
  trust_key "${LINBIT_KEY_URL}" "${LINBIT_FINGERPRINT}" /usr/share/keyrings/linbit.gpg
  echo "deb [signed-by=/usr/share/keyrings/linbit.gpg] ${LINBIT_SOURCE}" >/etc/apt/sources.list.d/linbit.list
  if [[ -z "${PACKAGE_URL}" ]]; then
    trust_key "${CLOWDER_KEY_URL}" "${CLOWDER_FINGERPRINT}" /usr/share/keyrings/clowder.gpg
    echo "deb [signed-by=/usr/share/keyrings/clowder.gpg] ${CLOWDER_SOURCE}" >/etc/apt/sources.list.d/clowder.list
  fi
  "${APT[@]}" update
}

# fetch_clowderd: With the test hook, download the .deb and check it; print
# what apt installs as clowderd: that file, else the pinned version from
# clowder's repository.
fetch_clowderd() {
  if [[ -n "${PACKAGE_URL}" ]]; then
    curl -fsSL "${PACKAGE_URL}" -o "${PACKAGE_FILE}"
    echo "${PACKAGE_SHA256}  ${PACKAGE_FILE}" | sha256sum --check --quiet
    echo "${PACKAGE_FILE}"
  elif [[ -n "${CLOWDERD_VERSION}" ]]; then
    echo "clowderd=${CLOWDERD_VERSION}"
  else
    echo clowderd
  fi
}

# install_packages: DRBD 9 and clowderd with what it depends on; what a
# prepared image holds is there already.
install_packages() {
  local package
  add_sources
  package="$(fetch_clowderd)"
  "${APT[@]}" install -y --no-install-recommends "${package}"
}

# install_dependencies: What clowderd depends on, without clowderd, for a
# prepared image: its package changes with every build, its dependencies
# (DRBD's module above all) rarely. Of each alternative the first, without
# its version: the newest the mirror has. Should that be too old, the
# install of clowderd on the host says so.
install_dependencies() {
  local package depends dependencies
  add_sources
  package="$(fetch_clowderd)"
  if [[ -f "${package}" ]]; then
    depends="$(dpkg-deb --field "${package}" Depends)"
  else
    depends="$(apt-cache show --no-all-versions "${package}" | awk -F ': ' '/^Depends:/ {print $2; exit}')"
  fi
  mapfile -t dependencies < <(tr ',' '\n' <<<"${depends}" | sed -E 's/\|.*//; s/\(.*\)//; s/[[:space:]]//g' | grep -v '^$')
  if ((${#dependencies[@]} == 0)); then
    log "found no dependencies of ${package}"
    return 1
  fi
  "${APT[@]}" install -y --no-install-recommends "${dependencies[@]}"
  rm -f "${PACKAGE_FILE}"
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

# join_cluster <host flags...>: clowderd join through the first host, tried
# again for a while: it may still boot, or the leader be busy with another
# host. clowderd says in the journal why a try failed.
join_cluster() {
  local deadline=$((SECONDS + 900))
  # the module writes an empty file for a host without a token in join_tokens
  if [[ -z "$(tr -d '[:space:]' <"${TOKEN_FILE}")" ]]; then
    log "${TOKEN_FILE} holds no join token; put one there (clowder host join-token) and reboot"
    return 1
  fi
  until clowderd join "$@" --via "${JOIN_VIA}" --token-file "${TOKEN_FILE}"; do
    if ((SECONDS >= deadline)); then
      log "gave up joining through ${JOIN_VIA}; if the cluster lists this host as joining," \
        "clowder host remove it, then put a new token into ${TOKEN_FILE} and reboot"
      return 1
    fi
    sleep 10
  done
}

# enter_cluster: clowderd check, then clowderd init with the cluster's name
# or clowderd join with the join token, once.
enter_cluster() {
  local class_volume pools=() host=() underlay public_address server_id
  for class_volume in ${VOLUMES}; do
    pools+=(--pool "vg-${class_volume%%=*}/thin")
  done
  clowderd check "${pools[@]}" --floating-ip "${FLOATING_IP}"
  if [[ -f /var/lib/clowder/host.json ]]; then
    log "this host is in a cluster already"
    return 0
  fi
  if [[ -d /var/lib/clowder/raft ]]; then
    log "an earlier clowderd init or join stopped halfway; to start anew, remove /var/lib/clowder and reboot"
    return 1
  fi
  # one value per assignment: a failed $(...) in an array's assignment does
  # not stop the script
  underlay="$(private_address)"
  public_address="$(metadata public-ipv4)"
  server_id="$(metadata instance-id)"
  # the host's name in the cluster is its hostname, the server's name
  host=(--underlay "${underlay}" --public-address "${public_address}" --server-id "${server_id}")
  if [[ -f "${TOKEN_FILE}" ]]; then
    join_cluster "${host[@]}"
  else
    clowderd init "${host[@]}" --cluster-name "${CLUSTER_NAME}"
  fi
}

# main: Do what the header of this script describes.
main() {
  # shellcheck source=/dev/null
  source "${SETTINGS}"
  mkdir -p "${STATE_DIR}"
  if [[ "${PREPARE_ONLY:-}" == yes ]]; then
    install_headers
    configure_base
    install_dependencies
    touch "${PREPARED}"
    log "prepared"
    return
  fi
  wait_for_hardware
  install_headers
  configure_base
  install_packages
  configure_storage
  configure_floating_ip
  wait_for 600 "the time sync" time_synchronized
  enter_cluster
  # the token is used up
  rm -f "${TOKEN_FILE}"
  touch "${MARKER}"
  log "done"
}

main "$@"
