#!/usr/bin/env bash
# One DRBD 9 resource as the plan configures it: quorum majority with
# suspend-io, auto-promote off, max-peers 4, port 10000 + minor, node-id =
# host ID. The resource file is the same on every host.
# Usage: drbd.sh res <name> <minor> <host>:<id>:<disk or none>...
#        drbd.sh lv <vg> <name> <size>      thin LV in <vg>/thin
#        drbd.sh create-md <name>           new metadata on this host's disk
set -euo pipefail

# write_res <name> <minor> <host>:<id>:<disk>...: /etc/drbd.d/<name>.res.
write_res() {
  local name="$1" minor="$2" spec host id disk hosts=()
  shift 2
  {
    echo "resource ${name} {"
    echo "  options { quorum majority; on-no-quorum suspend-io; auto-promote no; }"
    echo "  net { protocol C; after-sb-0pri discard-zero-changes;"
    echo "        after-sb-1pri discard-secondary; after-sb-2pri disconnect; }"
    echo "  disk { c-max-rate 50M; c-plan-ahead 20; }"
    echo "  device minor ${minor};"
    echo "  meta-disk internal;"
    for spec in "$@"; do
      IFS=: read -r host id disk <<<"${spec}"
      hosts+=("${host}")
      echo "  on ${host} { node-id ${id}; address 10.77.0.$((id + 1)):$((10000 + minor)); disk ${disk}; }"
    done
    echo "  connection-mesh { hosts ${hosts[*]}; }"
    echo "}"
  } >"/etc/drbd.d/${name}.res"
  drbdadm sh-nop
}

main() {
  case "$1" in
    res) write_res "${@:2}" ;;
    lv) lvs "$2/$3" >/dev/null 2>&1 || lvcreate -q -V "$4" -T "$2/thin" -n "$3" ;;
    create-md) echo yes | drbdadm create-md --force --max-peers=4 "$2" ;;
    *)
      echo "usage: drbd.sh res|lv|create-md ..." >&2
      exit 2
      ;;
  esac
}

main "$@"
