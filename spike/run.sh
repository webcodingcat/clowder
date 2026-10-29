#!/usr/bin/env bash
# Phase 0 spike: try the risky paths of the design on three HLab hosts.
# Usage: HLAB=/path/to/hlab/bin/hlab spike/run.sh <step>...
#   up        start the lab, create the hosts (OpenTofu), install them, mesh
#   net       DNAT without SNAT, client IP, Floating IP move, conntrack, invariant
#   drbd      quorum, suspend-io, fencing, resync, class move, snapshot, recover
#   boot      restart "no" and the mount guard across a reboot
#   watchdog  softdog reboots a host whose PID 1 hangs
#   down      destroy the hosts and stop the lab
# Results go to spike/results/ (summary.txt, one log per step, the rulesets).
set -euo pipefail
export MSYS_NO_PATHCONV=1

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly HERE
DOCKER_HERE="$(cd "${HERE}" && (pwd -W 2>/dev/null || pwd) )"
readonly DOCKER_HERE
readonly LAB_CONFIG="${HERE}/lab.yaml"
readonly CONTAINER=hlab-clowder
readonly TOFU_IMAGE=ghcr.io/opentofu/opentofu:1.13.2
readonly ECHO_IMAGE=alpine/socat
readonly RESULTS="${HERE}/results"
readonly NODES=(node0 node1 node2)

# lab <arguments>: Run HLab's wrapper with the spike's lab configuration.
# Globals:
#   HLAB (read)
lab() {
  "${HLAB:?set HLAB to the path of hlab/bin/hlab}" -c "${LAB_CONFIG}" "$@"
}

# tofu <arguments>: Run OpenTofu in the lab's network on spike/infra.
# Globals:
#   HLAB_NETWORK, HCLOUD_ENDPOINT, HCLOUD_TOKEN, TF_VAR_ssh_public_key (read)
tofu() {
  docker run --rm -i --network "${HLAB_NETWORK}" \
    -e HCLOUD_ENDPOINT -e HCLOUD_TOKEN -e TF_IN_AUTOMATION=1 \
    -e TF_VAR_ssh_public_key -v "${DOCKER_HERE}/infra:/work" -w /work \
    "${TOFU_IMAGE}" "$@"
}

# load_env: The lab's endpoints and the lab's SSH key for OpenTofu.
load_env() {
  eval "$(docker exec "${CONTAINER}" hlab env)"
  TF_VAR_ssh_public_key="$(docker exec "${CONTAINER}" cat /hlab/id_ed25519.pub)"
  export HLAB_NETWORK HCLOUD_ENDPOINT HCLOUD_TOKEN TF_VAR_ssh_public_key
  load_outputs
}

# on <host> <command>: Run a shell command on a host as root.
on() {
  local host="$1"
  shift
  docker exec -i "${CONTAINER}" hlab ssh "${host}" "$*"
}

# guest <host> <script> [arguments]: Run one of guest/*.sh on a host.
guest() {
  local host="$1" script="$2"
  shift 2
  docker exec -i "${CONTAINER}" hlab ssh "${host}" bash -s -- "$@" \
    <"${HERE}/guest/${script}"
}

# client <command>: Run a command as the Internet client alice.
client() {
  docker exec -i "${CONTAINER}" hlab client exec alice "$@"
}

# api <method> <path> [json]: Call the Cloud API from inside the lab; print
# the answer.
api() {
  local method="$1" path="$2" body="${3:-}" data=()
  [[ -n "${body}" ]] && data=(-d "${body}")
  docker exec "${CONTAINER}" curl -sf -X "${method}" \
    -H "Authorization: Bearer ${HCLOUD_TOKEN}" -H "Content-Type: application/json" \
    "${data[@]}" "${HCLOUD_ENDPOINT}${path}"
}

# lab_jq <arguments>: jq inside the lab container (the host may lack it).
lab_jq() {
  docker exec -i "${CONTAINER}" jq "$@"
}

# api_action <path> [json]: POST an action and wait until it succeeded.
api_action() {
  local action_id status
  action_id="$(api POST "$1" "${2:-}" | lab_jq -r .action.id)"
  for _ in $(seq 1 120); do
    status="$(api GET "/actions/${action_id}" | lab_jq -r .action.status)"
    [[ "${status}" == success ]] && return 0
    [[ "${status}" == error ]] && return 1
    sleep 1
  done
  return 1
}

# load_outputs: Read the OpenTofu outputs once (each tofu run takes seconds).
# Globals:
#   OUTPUTS (set)
load_outputs() {
  OUTPUTS="$(tofu output -json 2>/dev/null || echo "{}")"
}

# output <jq filter>: Read from the OpenTofu outputs.
# Globals:
#   OUTPUTS (read)
output() {
  lab_jq -r "$1" <<<"${OUTPUTS}"
}

# server_id <host>: Print a host's server ID.
server_id() {
  output ".nodes.value.$1.id"
}

# check <name> <command...>: Run a check, record OK or FAIL in the summary.
check() {
  local name="$1"
  shift
  if "$@"; then
    echo "OK   ${name}" | tee -a "${RESULTS}/summary.txt"
  else
    echo "FAIL ${name}" | tee -a "${RESULTS}/summary.txt"
  fi
}

# wait_ssh <host>: Wait until a host answers over SSH.
wait_ssh() {
  for _ in $(seq 1 120); do
    on "$1" true >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# mesh_up <host>: Bring up clowder0 and the WireGuard interfaces of a host.
mesh_up() {
  local host="$1" self peer specs=() key
  self="${host#node}"
  for peer in 0 1 2; do
    ((peer == self)) && continue
    key="$(guest "node${peer}" mesh.sh key)"
    specs+=("${peer},10.0.1.$((10 + peer)),${key}")
  done
  guest "${host}" mesh.sh up "${self}" "${specs[@]}"
}

# ingress_up <host>: The nft table and DOCKER-USER rules; the test ports are
# published on host 2.
ingress_up() {
  local host="$1" fip
  fip="$(output .floating_ip.value.address)"
  guest "${host}" ingress.sh "${host#node}" "${fip}" udp:9987:2 tcp:9988:2
}

# step_up: Start the lab, create the hosts, install them, build the mesh.
step_up() {
  local host fip
  lab up
  load_env
  tofu init -input=false >/dev/null
  tofu apply -input=false -auto-approve
  load_outputs
  fip="$(output .floating_ip.value.address)"
  for host in "${NODES[@]}"; do
    wait_ssh "${host}"
    on "${host}" cloud-init status --wait >/dev/null || true
  done
  for host in "${NODES[@]}"; do
    guest "${host}" base.sh "$(output ".nodes.value.${host}.bulk")" \
      "$(output ".nodes.value.${host}.fast")" "${fip}" &
  done
  wait
  for host in "${NODES[@]}"; do
    mesh_up "${host}"
  done
  for host in "${NODES[@]}"; do
    check "mesh ${host} reaches its peers" \
      on "${host}" "for a in 10.77.0.1 10.77.0.2 10.77.0.3; do ping -c1 -W2 \$a >/dev/null || exit 1; done"
  done
}

# start_echo <host> <port> <proto> <name>: A container answering with its
# host name and the source address it sees, published on the Floating IP
# and the node address.
start_echo() {
  local host="$1" port="$2" proto="$3" name="$4" fip address listen
  fip="$(output .floating_ip.value.address)"
  address="10.77.0.$((${host#node} + 1))"
  listen=TCP-LISTEN
  [[ "${proto}" == udp ]] && listen=UDP-RECVFROM
  on "${host}" "docker rm -f ${name} >/dev/null 2>&1; docker run -d --restart no \
    --name ${name} --hostname ${host}-echo -p ${fip}:${port}:${port}/${proto} \
    -p ${address}:${port}:${port}/${proto} ${ECHO_IMAGE} \
    ${listen}:${port},fork,reuseaddr 'SYSTEM:echo \$(hostname) \$SOCAT_PEERADDR'" >/dev/null
}

# ask_echo <proto> <port>: Send one request from alice to the Floating IP and
# print the answer ("<host>-echo <address the container saw>").
ask_echo() {
  local fip
  fip="$(output .floating_ip.value.address)"
  client python3 - "$1" "${fip}" "$2" <<'EOF'
import socket, sys
proto, host, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
kind = socket.SOCK_DGRAM if proto == "udp" else socket.SOCK_STREAM
with socket.socket(socket.AF_INET, kind) as s:
    s.settimeout(3)
    s.connect((host, port))
    s.send(b"x\n")
    print(s.recv(200).decode().strip())
EOF
}

# alice_address: Print alice's public address.
alice_address() {
  client ip -4 -o addr show scope global | awk '{split($4, a, "/"); print a[1]; exit}'
}

# expect_echo <proto> <port> <host>: The answer comes from <host>'s container
# and shows alice's own address.
expect_echo() {
  local answer
  answer="$(ask_echo "$1" "$2" 2>&1 || true)"
  echo "  $1/$2 -> ${answer}"
  [[ "${answer}" == "$3-echo $(alice_address)" ]]
}

# conntrack_follow: A UDP flow from one source port keeps its conntrack entry
# on the ingress host; after the target moves, only conntrack -D lets it
# follow. Prints the answers in order.
conntrack_follow() {
  local fip ingress="$1"
  fip="$(output .floating_ip.value.address)"
  client python3 - "${fip}" >"${RESULTS}/conntrack.log" <<'EOF' &
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(0.4)
for _ in range(60):
    s.sendto(b"x", (sys.argv[1], 9987))
    try:
        print(s.recv(200).decode().strip(), flush=True)
    except socket.timeout:
        print("-", flush=True)
    time.sleep(0.5)
EOF
  sleep 8
  # move the target to host 1 (rules on the ingress host only, for the test)
  guest "${ingress}" ingress.sh "${ingress#node}" "${fip}" udp:9987:1 tcp:9988:2
  sleep 8
  on "${ingress}" "conntrack -D -p udp --orig-dst ${fip} --dport 9987 >/dev/null 2>&1 || true"
  wait
  cat "${RESULTS}/conntrack.log"
  # before the delete: still node2 although the rule points to node1; after: node1
  tail -n 10 "${RESULTS}/conntrack.log" | grep -q '^node1-echo'
}

# no_fip_source <host> <seconds>: No packet with the Floating IP as source
# leaves <host> while alice talks to the echo services. tcpdump runs detached
# on the host; its count is read afterwards.
no_fip_source() {
  local host="$1" fip count
  fip="$(output .floating_ip.value.address)"
  on "${host}" "pub=\$(ip -4 route show default | awk '{print \$5; exit}'); \
    (timeout $2 tcpdump -nni \$pub -e -c 1 src ${fip} 2>/dev/null >/tmp/fip-packets) \
    >/dev/null 2>&1 </dev/null &"
  sleep 2
  for _ in 1 2 3; do
    ask_echo udp 9987 >/dev/null 2>&1 || true
    sleep 1
  done
  sleep "$2"
  on "${host}" cat /tmp/fip-packets
  # grep -c .: tcpdump may leave an empty line when timeout stops it
  count="$(on "${host}" "grep -c . /tmp/fip-packets || true")"
  echo "  packets with the Floating IP as source: ${count}"
  [[ "${count}" -eq 0 ]]
}

# step_net: Published ports over WireGuard without SNAT.
step_net() {
  local host fip
  load_env
  fip="$(output .floating_ip.value.address)"
  docker exec "${CONTAINER}" hlab client add alice >/dev/null 2>&1 || true
  for host in "${NODES[@]}"; do
    ingress_up "${host}"
  done
  # an earlier run may have left the Floating IP on node1
  api_action "/floating_ips/$(output .floating_ip.value.id)/actions/assign" \
    "{\"server\": $(server_id node0)}"
  start_echo node2 9987 udp echo-udp
  start_echo node2 9988 tcp echo-tcp
  start_echo node1 9987 udp echo-udp
  check "udp via node0 to node2 keeps the client address" expect_echo udp 9987 node2
  check "tcp via node0 to node2 keeps the client address" expect_echo tcp 9988 node2
  check "no packet with the Floating IP as source leaves node2" no_fip_source node2 8
  api_action "/floating_ips/$(output .floating_ip.value.id)/actions/assign" \
    "{\"server\": $(server_id node1)}"
  sleep 3
  check "udp via node1 (after the move) to node2" expect_echo udp 9987 node2
  check "tcp via node1 (after the move) to node2" expect_echo tcp 9988 node2
  check "conntrack -D lets a running UDP flow follow a moved target" \
    conntrack_follow node1
  ingress_up node1
  for host in "${NODES[@]}"; do
    on "${host}" "nft list table inet clowder; echo; iptables -S DOCKER-USER; \
      echo; ip rule; ip route show table all | grep -E 'clw|clowder'" \
      >"${RESULTS}/ruleset-${host}.txt"
  done
}

# drbd_on <host> <arguments>: drbd.sh on a host.
drbd_on() {
  local host="$1"
  shift
  guest "${host}" drbd.sh "$@"
}

# write_res_everywhere <node1 disk>: The resource r0 on all hosts; node2 is
# the diskless tiebreaker.
write_res_everywhere() {
  local host
  for host in "${NODES[@]}"; do
    drbd_on "${host}" res r0 0 node0:0:/dev/vg-bulk/r0 "node1:1:$1" node2:2:none
  done
}

# wait_uptodate <host> <seconds>: Wait until all disks of r0 are UpToDate as
# <host> sees them.
wait_uptodate() {
  for _ in $(seq 1 "$2"); do
    on "$1" "drbdadm status r0" | grep -qE 'Inconsistent|Outdated|SyncSource|SyncTarget|Connecting' \
      || return 0
    sleep 1
  done
  return 1
}

# write_blocks <host> <seconds>: A write to /mnt/r0 does not finish within
# <seconds> (suspended I/O).
write_blocks() {
  # A write on suspended I/O hangs in the kernel (state D) and no timeout can
  # end it: start it detached and look whether it is still running.
  on "$1" "(dd if=/dev/zero of=/mnt/r0/blocked bs=4k count=1 conv=fsync status=none; \
    echo done >/tmp/write-done) >/dev/null 2>&1 </dev/null & rm -f /tmp/write-done"
  sleep "$2"
  on "$1" "[ ! -e /tmp/write-done ] && ps -eo stat,cmd | grep -q '^D.*dd if=/dev/zero of=/mnt/r0/blocked'"
}

# sums_match <host>: The test file on <host> still has its checksum.
sums_match() {
  on "$1" "cd /mnt/r0 && sha256sum -c --quiet /root/r0.sha256"
}

# power <host> <off|on>: Power a host off or on through the Cloud API.
power() {
  api_action "/servers/$(server_id "$1")/actions/power$2"
}

# rtt_during <host> <target> <command>: Max ping RTT over WireGuard while a
# command runs on <host>; prints it.
rtt_during() {
  local host="$1" target="$2"
  shift 2
  on "${host}" "ping -i 0.2 -w 30 ${target} > /tmp/rtt.log & $*; kill %1 2>/dev/null; \
    awk -F'time=' '/time=/{split(\$2,a,\" \"); if (a[1]>m) m=a[1]} END {print \"max rtt ms:\", m}' /tmp/rtt.log"
}

# step_drbd: Quorum, fencing, resync, class move, snapshot, recover.
step_drbd() {
  local host
  load_env
  # a clean start: power-cycle hosts with hung I/O from an earlier run, then
  # remove r0 everywhere
  docker exec "${CONTAINER}" hlab fault clear
  for host in "${NODES[@]}"; do
    if on "${host}" "drbdsetup status r0 2>/dev/null | grep -q suspended" \
      || ! on "${host}" "umount -l /mnt/r0 2>/dev/null; timeout 30 drbdadm down r0 2>/dev/null; \
        ! drbdsetup status r0 >/dev/null 2>&1"; then
      power "${host}" off
      power "${host}" on
      wait_ssh "${host}"
    fi
    on "${host}" "lvremove -q -y vg-bulk/r0 vg-fast/r0 2>/dev/null; rm -f /etc/drbd.d/r0.res; true"
  done
  # hosts that were rebooted (here or at the end of an earlier run) lost the
  # mesh and the ingress rules; both are idempotent
  for host in "${NODES[@]}"; do
    mesh_up "${host}"
    ingress_up "${host}"
  done
  drbd_on node0 lv vg-bulk r0 4G
  drbd_on node1 lv vg-bulk r0 4G
  write_res_everywhere /dev/vg-bulk/r0
  drbd_on node0 create-md r0
  drbd_on node1 create-md r0
  for host in "${NODES[@]}"; do
    on "${host}" "drbdadm up r0"
  done
  # initial full sync of 4 GiB, rate-limited, while measuring the RTT
  on node0 "drbdadm primary --force r0"
  echo "initial sync:" | tee -a "${RESULTS}/measurements.txt"
  rtt_during node0 10.77.0.2 "start=\$(date +%s); sleep 2; \
    while drbdadm status r0 | grep -qE 'Sync|Inconsistent'; do sleep 1; done; \
    echo seconds: \$((\$(date +%s) - start))" | tee -a "${RESULTS}/measurements.txt"
  on node0 "mkfs.ext4 -q /dev/drbd0 && mkdir -p /mnt/r0 && mount /dev/drbd0 /mnt/r0 && \
    head -c 200M /dev/urandom > /mnt/r0/data && sync && \
    (cd /mnt/r0 && sha256sum data > /root/r0.sha256)"
  on node0 "cat /root/r0.sha256" | on node1 "cat > /root/r0.sha256"
  echo "write load (1 GiB, protocol C):" | tee -a "${RESULTS}/measurements.txt"
  rtt_during node0 10.77.0.2 "dd if=/dev/zero of=/mnt/r0/load bs=1M count=1024 oflag=direct status=none; rm /mnt/r0/load" \
    | tee -a "${RESULTS}/measurements.txt"
  check "r0 UpToDate on all disks" wait_uptodate node0 600

  # node0 cut off on the private network: suspended I/O, then fenced
  docker exec "${CONTAINER}" hlab fault cut node0 node1 --net private
  docker exec "${CONTAINER}" hlab fault cut node0 node2 --net private
  sleep 15
  check "node0 without quorum: writes are suspended" write_blocks node0 10
  check "fence node0 (power off through the API)" power node0 off
  check "node1 promotes with node2 as tiebreaker" on node1 "drbdadm primary r0"
  check "node1 mounts and the data is intact" on node1 \
    "mkdir -p /mnt/r0 && mount /dev/drbd0 /mnt/r0 && cd /mnt/r0 && sha256sum -c --quiet /root/r0.sha256"
  docker exec "${CONTAINER}" hlab fault clear
  power node0 on
  wait_ssh node0
  check "after the boot node0 has no DRBD resource up" on node0 "! drbdsetup status r0 >/dev/null 2>&1"
  mesh_up node0
  ingress_up node0
  on node0 "drbdadm up r0"
  check "node0 resyncs as secondary" wait_uptodate node1 300

  # class move online: node1 (primary) detaches, attaches a disk in vg-fast
  drbd_on node1 lv vg-fast r0 4G
  on node1 "drbdadm detach r0"
  check "node1 writes while diskless" on node1 \
    "dd if=/dev/urandom of=/mnt/r0/diskless bs=1M count=20 conv=fsync status=none"
  write_res_everywhere /dev/vg-fast/r0
  drbd_on node1 create-md r0
  on node1 "drbdadm attach r0"
  check "node1 resyncs onto vg-fast" wait_uptodate node1 600
  check "data intact after the class move" sums_match node1

  # backup snapshot: freeze on the primary (node1), snapshot on node0
  on node1 "fsfreeze -f /mnt/r0"
  on node0 "lvcreate -q -s -n r0snap vg-bulk/r0"
  on node1 "fsfreeze -u /mnt/r0"
  check "snapshot on the secondary mounts read-only with intact data" on node0 \
    "lvchange -q -ay -K vg-bulk/r0snap && mkdir -p /mnt/snap && \
     mount -t ext4 -o ro,noload /dev/vg-bulk/r0snap /mnt/snap && cd /mnt/snap && \
     sha256sum -c --quiet /root/r0.sha256; rc=\$?; cd /; umount /mnt/snap; \
     lvremove -q -y vg-bulk/r0snap; exit \$rc"

  # recover: node0 and node2 gone for good, node1 alone continues. Both go at
  # once: when peers drop one after the other, DRBD may keep quorum on the
  # last UpToDate node (seen in an earlier run), which is safe but not the
  # case this tests.
  power node0 off &
  power node2 off &
  wait
  sleep 10
  check "node1 alone: writes are suspended" write_blocks node1 10
  check "recover on node1: quorum off, forget the peers, resume" on node1 \
    "set -e; drbdadm del-peer r0:node0; drbdadm del-peer r0:node2; \
     drbdadm resource-options r0 --quorum=off; drbdadm resume-io r0 || true; \
     drbdsetup forget-peer r0 0; drbdsetup forget-peer r0 2; \
     timeout 30 dd if=/dev/urandom of=/mnt/r0/after bs=1M count=5 conv=fsync status=none"
  check "data intact after recover" sums_match node1
  power node0 on
  power node2 on
  wait_ssh node0
  wait_ssh node2
  on node1 "drbdsetup status r0 --verbose" >"${RESULTS}/drbd-after-recover.txt"
}

# step_boot: restart "no" and the mount guard across a reboot (node2).
step_boot() {
  load_env
  # a clean start: what an earlier run left behind
  on node2 "docker rm -f guarded docker-managed >/dev/null 2>&1; \
    docker volume rm guardvol >/dev/null 2>&1; umount /var/lib/clowder-spike/vol 2>/dev/null; \
    lvremove -q -y vg-bulk/guard 2>/dev/null; true"
  on node2 "mkdir -p /var/lib/clowder-spike/vol && \
    docker volume create -d local -o type=none -o o=bind \
      -o device=/var/lib/clowder-spike/vol/data guardvol >/dev/null"
  check "a container cannot start while its volume is not mounted" on node2 \
    "! docker run --rm -v guardvol:/data alpine true 2>/dev/null"
  guest node2 drbd.sh lv vg-bulk guard 1G
  on node2 "mkfs.ext4 -q /dev/vg-bulk/guard && mount /dev/vg-bulk/guard /var/lib/clowder-spike/vol && \
    mkdir -p /var/lib/clowder-spike/vol/data && \
    docker run -d --restart no --name guarded -v guardvol:/data alpine sleep infinity >/dev/null && \
    docker run -d --restart unless-stopped --name docker-managed alpine sleep infinity >/dev/null"
  api_action "/servers/$(server_id node2)/actions/reset"
  wait_ssh node2
  check "after the reboot Docker did not start the restart-no container" on node2 \
    "[ \"\$(docker inspect -f '{{.State.Running}}' guarded)\" = false ]"
  on node2 "echo \"docker-managed running: \$(docker inspect -f '{{.State.Running}}' docker-managed)\"" \
    | tee -a "${RESULTS}/measurements.txt"
  check "the unmounted volume cannot be used after the reboot" on node2 \
    "! docker start guarded 2>/dev/null && [ ! -e /var/lib/clowder-spike/vol/data ]"
}

# step_watchdog: The watchdog that systemd pets reboots node2 once nobody
# pets it. PID 1 cannot be stopped (the kernel ignores SIGSTOP for init), so
# systemd hands the device over (runtime setting, gone after the reboot) and a
# shell arms it and closes it without the magic character. Drop-ins apply in
# name order across /etc and /run, so the runtime one is called zz-*.
step_watchdog() {
  local before
  load_env
  check "systemd pets the watchdog" on node2 \
    "systemctl show -p RuntimeWatchdogUSec | grep -q '=1min'"
  on node2 "cat /sys/class/watchdog/watchdog0/identity" \
    | sed 's/^/watchdog device: /' | tee -a "${RESULTS}/measurements.txt"
  before="$(on node2 cat /proc/sys/kernel/random/boot_id)"
  on node2 "mkdir -p /run/systemd/system.conf.d && \
    printf '[Manager]\nRuntimeWatchdogSec=0\n' >/run/systemd/system.conf.d/zz-spike.conf && \
    systemctl daemon-reexec && sleep 2 && echo 1 >/dev/watchdog0"
  check "the watchdog reboots node2 when nobody pets it" rebooted_within node2 "${before}" 300
  check "after the reboot systemd pets the watchdog again" on node2 \
    "systemctl show -p RuntimeWatchdogUSec | grep -q '=1min'"
}

# rebooted_within <host> <old boot id> <seconds>: The host booted anew within
# <seconds>; prints how long it took. sshd keeps answering while PID 1 hangs,
# so the boot ID is polled.
rebooted_within() {
  local host="$1" before="$2" start now
  start="$(date +%s)"
  while now="$(date +%s)" && ((now - start < $3)); do
    if [[ "$(on "${host}" cat /proc/sys/kernel/random/boot_id 2>/dev/null)" =~ ^[0-9a-f-]+$ ]] \
      && [[ "$(on "${host}" cat /proc/sys/kernel/random/boot_id)" != "${before}" ]]; then
      echo "rebooted after about $((now - start)) s" | tee -a "${RESULTS}/measurements.txt"
      return 0
    fi
    sleep 5
  done
  return 1
}

# step_down: Destroy the hosts and stop the lab.
step_down() {
  load_env
  tofu destroy -input=false -auto-approve >/dev/null
  lab down
}

# main: Do what the header of this script describes.
main() {
  local step
  (($# > 0)) || set -- up net drbd boot watchdog
  mkdir -p "${RESULTS}"
  for step in "$@"; do
    echo "=== ${step}" | tee -a "${RESULTS}/summary.txt"
    "step_${step}" 2>&1 | tee "${RESULTS}/${step}.log"
  done
}

main "$@"
