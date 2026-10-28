#!/usr/bin/env bash
# The nft table inet clowder and the DOCKER-USER rules for published ports,
# as the plan describes them (3.2, 3.4, 3.5). The same rules go on every host.
# Usage: ingress.sh <self id> <floating ip> <proto>:<port>:<target host id>...
set -euo pipefail

readonly BRIDGES='{ "docker0", "cbr-*", "br-*" }'

# node_address <host id>: Print the overlay address of a host.
node_address() {
  echo "10.77.0.$(($1 + 1))"
}

# print_ruleset <self id> <floating ip> <published>...: The whole table, so
# nft -f replaces it atomically.
print_ruleset() {
  local self="$1" fip="$2" spec proto port target peer
  shift 2
  echo "table inet clowder"
  echo "delete table inet clowder"
  echo "table inet clowder {"
  echo "  chain mark_in {"
  echo "    type filter hook prerouting priority mangle; policy accept;"
  for peer in 0 1 2; do
    ((peer == self)) && continue
    echo "    ct state new iifname \"clw-${peer}\" ct mark set ct mark and 0xfffff000 or $((0x100 + peer))"
  done
  for spec in "$@"; do
    IFS=: read -r proto port target <<<"${spec}"
    ((target == self)) && continue
    echo "    ct state new ip daddr ${fip} ${proto} dport ${port} ct mark set ct mark and 0xfffff000 or 0x200"
  done
  echo "    iifname ${BRIDGES} meta mark set ct mark and 0xfff"
  echo "  }"
  echo "  chain dnat_in {"
  echo "    type nat hook prerouting priority dstnat - 10; policy accept;"
  for spec in "$@"; do
    IFS=: read -r proto port target <<<"${spec}"
    ((target == self)) && continue
    echo "    ip daddr ${fip} ${proto} dport ${port} dnat ip to $(node_address "${target}"):${port}"
  done
  echo "  }"
  echo "  chain snat_out {"
  echo "    type nat hook postrouting priority srcnat - 10; policy accept;"
  echo "    iifname ${BRIDGES} ct status dnat ip daddr 10.77.0.0/24 snat ip to $(node_address "${self}")"
  echo "  }"
  echo "  chain guard_in {"
  echo "    type filter hook input priority filter; policy accept;"
  echo "    iifname ${BRIDGES} ct state established,related accept"
  echo "    iifname ${BRIDGES} ip daddr 169.254.77.53 udp dport 53 accept"
  echo "    iifname ${BRIDGES} ip daddr ${fip} tcp dport { 80, 443 } accept"
  echo "    iifname ${BRIDGES} drop"
  echo "  }"
  echo "  chain guard_fwd {"
  echo "    type filter hook forward priority filter - 10; policy accept;"
  echo "    iifname ${BRIDGES} ip daddr 169.254.169.254 drop"
  echo "    iifname ${BRIDGES} ip daddr 10.77.0.0/24 ct status dnat accept"
  echo "    iifname ${BRIDGES} ip daddr 10.77.0.0/24 drop"
  echo "  }"
  echo "}"
}

# docker_user_rules: Accept forwarded connections that clowder redirected and
# their answers; Docker's FORWARD policy would drop them.
docker_user_rules() {
  iptables -C DOCKER-USER -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null \
    || iptables -I DOCKER-USER 1 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  iptables -C DOCKER-USER -m conntrack --ctstate DNAT -m connmark --mark 0x200/0xfff -j ACCEPT 2>/dev/null \
    || iptables -I DOCKER-USER 2 -m conntrack --ctstate DNAT -m connmark --mark 0x200/0xfff -j ACCEPT
}

main() {
  local ruleset
  ruleset="$(print_ruleset "$@")"
  nft -c -f - <<<"${ruleset}"
  nft -f - <<<"${ruleset}"
  docker_user_rules
}

main "$@"
