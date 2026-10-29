#!/bin/sh
# Check the renderers' golden files with the tools the hosts run them with:
# nft for the ruleset, drbdadm for the resource files. Runs inside a Debian
# container from the repository root (bin/dev test-tools); nft -c needs
# CAP_NET_ADMIN.
# Usage: test/tools/check-golden-files.sh
set -eu

# install_tools: Install nftables and drbd-utils quietly.
install_tools() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -qq update >/dev/null
  apt-get -qq install -y --no-install-recommends nftables drbd-utils >/dev/null
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
}

main "$@"
