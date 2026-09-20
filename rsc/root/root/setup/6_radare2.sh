#!/bin/bash
# Install the latest radare2 release from GitHub.
set -euo pipefail
echo "- $0"

ARCH="$(dpkg --print-architecture)"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

API_URL="https://api.github.com/repos/radareorg/radare2/releases/latest"
AUTH_HEADER=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

DEB_URL="$(
  curl -fsSL "${AUTH_HEADER[@]}" -H 'Accept: application/vnd.github+json' "${API_URL}" |
    jq -r --arg arch "${ARCH}" '.assets[]? | select(.name | test("radare2_[0-9]+\\.[0-9]+\\.[0-9]+_" + $arch + "\\.deb$")) | .browser_download_url' |
    head -n1
)"
if [[ -z "${DEB_URL}" ]]; then
  echo "Failed to determine radare2 download URL for arch ${ARCH}" >&2
  exit 1
fi

curl -fsSL "${DEB_URL}" -o "${TMPDIR}/radare2.deb"
dpkg -i "${TMPDIR}/radare2.deb"
