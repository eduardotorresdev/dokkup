#!/usr/bin/env bash
#
# Installs Dokku on first boot of the development container.
#
# Runs once: the stamp file makes a restart cheap. If the install fails the stamp
# is not written, so the next boot tries again rather than leaving a container
# that looks ready and is not.

set -euo pipefail

STAMP=/var/lib/dokku-bootstrap.done
LOG=/var/log/dokku-bootstrap.log

if [[ -f "${STAMP}" ]]; then
    echo "Dokku already installed ($(dokku version 2>/dev/null || echo unknown))"
    exit 0
fi

# shellcheck disable=SC1091
source /etc/dokku-bootstrap.env

echo "Waiting for Docker..."
for _ in $(seq 1 90); do
    if docker info >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

if ! docker info >/dev/null 2>&1; then
    echo "Docker did not become ready; refusing to install Dokku" >&2
    exit 1
fi

echo "Installing Dokku ${DOKKU_TAG} (this takes a few minutes, output in ${LOG})"
cd /root
wget -qNP . https://raw.githubusercontent.com/dokku/dokku/master/bootstrap.sh

DOKKU_TAG="${DOKKU_TAG}" DOKKU_HOSTNAME="${DOKKU_HOSTNAME}" \
    bash bootstrap.sh >"${LOG}" 2>&1

# Prove it actually works before claiming success. A bootstrap that exits zero
# but leaves an unusable Dokku is worse than one that fails loudly.
dokku version
dokku apps:list >/dev/null

date -u +%FT%TZ >"${STAMP}"
echo "Dokku ready: $(dokku version)"
