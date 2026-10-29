#!/usr/bin/env bash
# Add the Debian packages in dist/ to a checkout of the APT repository and
# sign it. Runs in debian:trixie-slim (release workflow, or by hand).
# Usage: publish.sh <apt repository checkout> <deb>...
# Environment:
#   APT_SIGNING_KEY  ASCII-armoured private key without passphrase
set -euo pipefail

readonly CODENAME=trixie
readonly PUBLIC_KEY_FILE=clowder.asc

# install_tools: reprepro and GnuPG.
install_tools() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends reprepro gnupg >/dev/null
}

# import_key: Import the signing key into GNUPGHOME; print its fingerprint.
import_key() {
  gpg --batch --quiet --import <<<"${APT_SIGNING_KEY:?APT_SIGNING_KEY is not set}"
  gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr:/ {print $10; exit}'
}

# write_config <repository> <fingerprint>: reprepro's configuration. conf/ and
# db/ stay in the repository: the next release adds to them.
write_config() {
  local repository="$1" fingerprint="$2"
  mkdir -p "${repository}/conf"
  cat >"${repository}/conf/distributions" <<EOF
Origin: clowder
Label: clowder
Codename: ${CODENAME}
Architectures: amd64
Components: main
Description: clowder packages for Debian 13
SignWith: ${fingerprint}
EOF
}

# main: Do what the header of this script describes.
main() {
  local repository="$1" fingerprint deb
  shift
  install_tools
  # a fresh, throw-away keyring: the key lives only for this run
  export GNUPGHOME
  GNUPGHOME="$(mktemp -d)"
  fingerprint="$(import_key)"
  write_config "${repository}" "${fingerprint}"
  for deb in "$@"; do
    reprepro -b "${repository}" includedeb "${CODENAME}" "${deb}"
  done
  gpg --batch --armor --export "${fingerprint}" >"${repository}/${PUBLIC_KEY_FILE}"
  reprepro -b "${repository}" check "${CODENAME}"
  echo "signed with ${fingerprint}"
}

main "$@"
