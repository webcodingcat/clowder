#!/bin/sh
# Check the renderers' golden files with the tools the hosts run them with:
# nft for the ruleset, drbdadm for the resource files, docker-compose for the
# rendered stacks and Traefik's Compose file. Runs inside a Debian container from the repository root
# (bin/dev test-tools); nft -c needs CAP_NET_ADMIN.
# Usage: test/tools/check-golden-files.sh
set -eu

# install_tools: Install nftables, drbd-utils and docker-compose quietly.
install_tools() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -qq update >/dev/null
  apt-get -qq install -y --no-install-recommends nftables drbd-utils docker-compose >/dev/null
}

# check_compose: Check every rendered stack with the docker-compose of the
# hosts, together with its secret variables as the agent passes both, and
# that it fills in no variable: the stacks' values are filled in on the
# user's machine already (a USER set here must not show up), and a $ in a
# secret is meant as it stands. Then Traefik's Compose file.
check_compose() {
  for rendered in internal/compose/testdata/*/rendered.golden.yaml; do
    secrets="$(dirname "${rendered}")/secrets.golden.yaml"
    if ! output="$(USER=filled-in-on-the-host docker-compose -f "${rendered}" -f "${secrets}" config 2>&1)"; then
      echo "${output}"
      return 1
    fi
    if echo "${output}" | grep -q filled-in-on-the-host; then
      echo "docker-compose filled in a variable of ${rendered}"
      return 1
    fi
    echo "docker-compose: ${rendered} OK"
  done
  traefik=internal/ingress/testdata/compose.golden.yaml
  if ! output="$(docker-compose -f "${traefik}" config 2>&1)"; then
    echo "${output}"
    return 1
  fi
  echo "docker-compose: ${traefik} OK"
}

# check_nft: Check every golden ruleset.
check_nft() {
  for ruleset in internal/network/testdata/*.golden.nft; do
    nft -c -f "${ruleset}"
    echo "nft: ${ruleset} OK"
  done
}

# check_drbd: Check every golden resource file, each in a configuration of
# its own as /etc/drbd.conf would include it. The container's hostname must
# be one of the files' hosts (node0).
check_drbd() {
  for resource in internal/storage/testdata/*.golden.res; do
    printf 'global { usage-count no; }\ninclude "%s";\n' "$(pwd)/${resource}" >/tmp/drbd.conf
    if ! output="$(drbdadm -c /tmp/drbd.conf sh-nop 2>&1)"; then
      echo "${output}"
      return 1
    fi
    echo "drbdadm: ${resource} OK"
  done
}

# main: Do what the header of this script describes.
main() {
  install_tools
  check_nft
  check_drbd
  check_compose
}

main "$@"
