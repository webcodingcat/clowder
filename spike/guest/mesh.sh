#!/usr/bin/env bash
# WireGuard with one interface per peer, the node address on clowder0, routes
# with the node address as source, and a routing table per peer for the
# return path without SNAT.
# Usage: mesh.sh key
#        mesh.sh up <self id> <peer id>,<peer private ip>,<peer public key>...
set -euo pipefail

readonly KEY_FILE=/etc/clowder-spike/wg.key
readonly PORT_BASE=51800
readonly MTU=1370

# node_address <host id>: Print the overlay address of a host.
node_address() {
  echo "10.77.0.$(($1 + 1))"
}

# print_public_key: Create the private key once and print the public key.
print_public_key() {
  mkdir -p "$(dirname "${KEY_FILE}")"
  # -s: an earlier run without wireguard-tools may have left an empty file
  [[ -s "${KEY_FILE}" ]] || (umask 077 && wg genkey >"${KEY_FILE}")
  wg pubkey <"${KEY_FILE}"
}

# up_peer <self id> <peer id> <peer private ip> <peer public key>: The
# interface clw-<peer>, its route and its routing table. The interface listens
# on 51800 + peer id; the peer's interface to us listens on 51800 + self id.
up_peer() {
  local self="$1" peer="$2" peer_ip="$3" peer_key="$4" link="clw-$2"
  ip link show "${link}" >/dev/null 2>&1 || ip link add "${link}" type wireguard
  wg set "${link}" private-key "${KEY_FILE}" listen-port $((PORT_BASE + peer)) \
    peer "${peer_key}" endpoint "${peer_ip}:$((PORT_BASE + self))" \
    allowed-ips 0.0.0.0/0 persistent-keepalive 25
  ip link set "${link}" mtu "${MTU}" up
  # the reverse path filter (also loose) drops everything that arrives on an
  # interface without an IPv4 address; the node address is enough
  ip addr replace "$(node_address "${self}")/32" dev "${link}" noprefixroute
  sysctl -q -w "net.ipv4.conf.${link//./\/}.rp_filter=2"
  ip route replace "$(node_address "${peer}")/32" dev "${link}" \
    src "$(node_address "${self}")"
  ip route replace default dev "${link}" table $((100 + peer))
  ip rule show | grep -q "lookup $((100 + peer))" \
    || ip rule add fwmark $((0x100 + peer))/0xfff lookup $((100 + peer)) priority 1000
}

# main: Do what the header of this script describes.
main() {
  case "$1" in
    key) print_public_key ;;
    up)
      local self="$2" spec peer peer_ip peer_key
      shift 2
      print_public_key >/dev/null
      ip link show clowder0 >/dev/null 2>&1 || ip link add clowder0 type dummy
      ip addr replace "$(node_address "${self}")/32" dev clowder0
      ip addr replace 169.254.77.53/32 dev clowder0
      ip link set clowder0 up
      for spec in "$@"; do
        IFS=, read -r peer peer_ip peer_key <<<"${spec}"
        up_peer "${self}" "${peer}" "${peer_ip}" "${peer_key}"
      done
      wg show all latest-handshakes
      ;;
    *)
      echo "usage: mesh.sh key | up <self id> <peer>..." >&2
      exit 2
      ;;
  esac
}

main "$@"
