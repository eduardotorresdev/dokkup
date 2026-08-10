#!/bin/sh
#
# Convenience installer for dokkup.
#
# It downloads the release binary, verifies its checksum and -- when cosign is
# available -- its signature, installs it, and hands over to `dokkup install`.
#
# Piping a script from the internet into a root shell deserves suspicion, so
# this one is short enough to read first:
#
#   curl -fsSL https://raw.githubusercontent.com/eduardotorresdev/dokkup/main/install.sh | less
#
# The manual path in the README does the same thing in four commands, and is the
# better choice if you would rather not trust this file.

set -eu

REPO=eduardotorresdev/dokkup
INSTALL_PATH=${INSTALL_PATH:-/usr/local/bin/dokkup}
VERSION=${VERSION:-latest}

die() {
    echo "dokkup install: $*" >&2
    exit 1
}

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

need curl
need sha256sum
need install

[ "$(id -u)" -eq 0 ] || die "run as root (this installs a system service)"

case "$(uname -s)" in
    Linux) ;;
    *) die "dokkup installs on Linux hosts; this is $(uname -s)" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
esac

command -v dokku >/dev/null 2>&1 \
    || die "dokku not found on this host; dokkup manages an existing Dokku installation"

BINARY="dokkup_linux_${ARCH}"
if [ "${VERSION}" = latest ]; then
    BASE="https://github.com/${REPO}/releases/latest/download"
else
    BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

TMP=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '${TMP}'" EXIT INT TERM

echo "Downloading ${BINARY} (${VERSION})..."
curl -fsSL "${BASE}/${BINARY}" -o "${TMP}/${BINARY}"
curl -fsSL "${BASE}/SHA256SUMS" -o "${TMP}/SHA256SUMS"

echo "Verifying checksum..."
(cd "${TMP}" && sha256sum --check --ignore-missing SHA256SUMS) \
    || die "checksum verification failed -- do not run this binary"

if command -v cosign >/dev/null 2>&1; then
    echo "Verifying signature..."
    curl -fsSL "${BASE}/${BINARY}.cosign.bundle" -o "${TMP}/${BINARY}.cosign.bundle"
    cosign verify-blob "${TMP}/${BINARY}" \
        --bundle "${TMP}/${BINARY}.cosign.bundle" \
        --certificate-identity-regexp "https://github\.com/${REPO}/" \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com \
        || die "signature verification failed -- do not run this binary"
else
    echo "cosign not installed; skipping signature verification."
    echo "The checksum was verified. To verify provenance too, install cosign and"
    echo "follow the manual steps in the README."
fi

echo "Installing to ${INSTALL_PATH}..."
install -m 0755 "${TMP}/${BINARY}" "${INSTALL_PATH}"

echo
echo "Installed. Continuing with 'dokkup install'."
echo
exec "${INSTALL_PATH}" install "$@"
