#!/usr/bin/env bash
# Add the Debian packages in dist/ to a checkout of the APT repository and
# sign it. Runs in debian:trixie-slim (release workflow, or by hand).
# Usage: publish.sh <apt repository checkout> <deb>...
# Environment:
#   APT_SIGNING_KEY  ASCII-armoured private key without passphrase
set -euo pipefail

readonly CODENAME=trixie
readonly COMPONENT=main
readonly ARCHITECTURE=amd64
readonly PUBLIC_KEY_FILE=clowder.asc

# install_tools: apt-ftparchive and GnuPG.
install_tools() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends apt-utils gnupg >/dev/null
}

# import_key: Import the signing key into GNUPGHOME; print its fingerprint.
import_key() {
  gpg --batch --quiet --import <<<"${APT_SIGNING_KEY:?APT_SIGNING_KEY is not set}"
  gpg --batch --with-colons --list-secret-keys | awk -F: '/^fpr:/ {print $10; exit}'
}

# add_to_pool <repository> <deb>: Put a package where Debian's layout has it
# (pool/main/c/clowderd/; no lib* packages here, so the first letter will do).
# A version is published once: a file that is there already stays, as hosts
# may have its checksum.
add_to_pool() {
  local repository="$1" deb="$2" name directory
  name="$(dpkg-deb --field "${deb}" Package)"
  directory="${repository}/pool/${COMPONENT}/${name:0:1}/${name}"
  if [[ -e "${directory}/$(basename "${deb}")" ]]; then
    echo "publish.sh: $(basename "${deb}") is published already" >&2
    return 1
  fi
  mkdir -p "${directory}"
  cp "${deb}" "${directory}/"
}

# write_indexes <repository>: The package index lists every version in the
# pool, so a host pinned to an older release still installs it.
write_indexes() {
  local repository="$1" binary="dists/${CODENAME}/${COMPONENT}/binary-${ARCHITECTURE}" release
  mkdir -p "${repository}/${binary}"
  # run from the repository: the index names the files relative to it
  (cd "${repository}" && apt-ftparchive --arch "${ARCHITECTURE}" packages "pool/${COMPONENT}") >"${repository}/${binary}/Packages"
  gzip -9 --no-name --keep --force "${repository}/${binary}/Packages"
  # the Release of the last run must not list itself
  rm -f "${repository}/dists/${CODENAME}/"{Release,InRelease,Release.gpg}
  # written beside, not into, the directory it lists
  release="$(mktemp)"
  apt-ftparchive \
    -o "APT::FTPArchive::Release::Origin=clowder" \
    -o "APT::FTPArchive::Release::Label=clowder" \
    -o "APT::FTPArchive::Release::Suite=${CODENAME}" \
    -o "APT::FTPArchive::Release::Codename=${CODENAME}" \
    -o "APT::FTPArchive::Release::Architectures=${ARCHITECTURE}" \
    -o "APT::FTPArchive::Release::Components=${COMPONENT}" \
    -o "APT::FTPArchive::Release::Description=clowder packages for Debian 13" \
    release "${repository}/dists/${CODENAME}" >"${release}"
  mv "${release}" "${repository}/dists/${CODENAME}/Release"
}

# sign <repository> <fingerprint>: InRelease and Release.gpg, then check
# that the signature holds.
sign() {
  local release="$1/dists/${CODENAME}" fingerprint="$2"
  gpg --batch --yes --local-user "${fingerprint}" --clearsign \
    --output "${release}/InRelease" "${release}/Release"
  gpg --batch --yes --local-user "${fingerprint}" --armor --detach-sign \
    --output "${release}/Release.gpg" "${release}/Release"
  gpg --batch --quiet --verify "${release}/InRelease"
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
  for deb in "$@"; do
    add_to_pool "${repository}" "${deb}"
  done
  write_indexes "${repository}"
  sign "${repository}" "${fingerprint}"
  gpg --batch --armor --export "${fingerprint}" >"${repository}/${PUBLIC_KEY_FILE}"
  echo "signed with ${fingerprint}"
}

main "$@"
