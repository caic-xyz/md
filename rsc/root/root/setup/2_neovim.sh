#!/bin/bash
# Install the latest stable Neovim build and wire common aliases.
set -euo pipefail
echo "- $0"

ARCH="$(uname -m)"
case "${ARCH}" in
aarch64 | arm64) ARCH="arm64" ;;
x86_64 | amd64) ARCH="x86_64" ;;
*)
  echo "Unsupported architecture: ${ARCH}" >&2
  exit 1
  ;;
esac

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

API_URL="https://api.github.com/repos/neovim/neovim/releases/latest"
AUTH_HEADER=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

NVIM_URL="$(
  curl -fsSL "${AUTH_HEADER[@]}" -H 'Accept: application/vnd.github+json' "${API_URL}" |
    jq -r --arg arch "${ARCH}" '.assets[]? | select(.name | test("nvim-linux-" + $arch + "\\.tar\\.gz$")) | .browser_download_url' |
    head -n1
)"
if [[ -z "${NVIM_URL}" ]]; then
  echo "Failed to determine Neovim download URL for arch ${ARCH}" >&2
  exit 1
fi

curl -fsSL "${NVIM_URL}" -o "${TMPDIR}/nvim.tar.gz"
mkdir -p /opt/nvim
tar xzf "${TMPDIR}/nvim.tar.gz" -C /opt/nvim --strip-components=1
ln -sf /opt/nvim/bin/nvim /usr/local/bin/nvim
ln -sf /opt/nvim/bin/nvim /usr/local/bin/vim
ln -sf /opt/nvim/bin/nvim /usr/local/bin/vi
ln -sf /opt/nvim/bin/vimdiff /usr/local/bin/vimdiff
