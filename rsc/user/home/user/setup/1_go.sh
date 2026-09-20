#!/bin/bash
# Install the latest Go toolchain reported by go.dev and set up go tools.
set -euo pipefail
echo "- $0"

ARCH="$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

GO_VERSION="$(curl -fsSL https://go.dev/VERSION?m=text | head -n1 | tr -d '\r')"
if [[ -z "${GO_VERSION}" ]]; then
  echo "Failed to resolve Go version" >&2
  exit 1
fi

# Install Go to user's home directory
curl -fsSL -o "${TMPDIR}/go.tar.gz" "https://go.dev/dl/${GO_VERSION}.linux-${ARCH}.tar.gz"
rm -rf "$HOME/.local/go"
mkdir -p "$HOME/.local"
tar -C "$HOME/.local" -xzf "${TMPDIR}/go.tar.gz"

# Update PATH for this session
export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"

mkdir -p "$HOME/go/bin"

# GitHub API auth header
AUTH_HEADER=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

# download_github_binary downloads a bare binary from a GitHub release.
# Usage: download_github_binary <repo> <asset_pattern> <name>
download_github_binary() {
  local repo="$1" pattern="$2" name="$3"
  local api_url="https://api.github.com/repos/${repo}/releases/latest"
  local url
  url="$(
    curl -fsSL "${AUTH_HEADER[@]}" -H 'Accept: application/vnd.github+json' "${api_url}" |
      jq -r --arg pat "${pattern}" '.assets[]? | select(.name | test($pat)) | .browser_download_url' |
      head -n1
  )"
  if [[ -z "${url}" ]]; then
    echo "Failed to find release asset for ${repo} matching ${pattern}" >&2
    return 1
  fi
  curl -fsSL "${url}" -o "$HOME/go/bin/${name}"
  chmod +x "$HOME/go/bin/${name}"
}

# download_github_tarball downloads a .tar.gz from a GitHub release and extracts a binary.
# Usage: download_github_tarball <repo> <asset_pattern> <binary_name>
download_github_tarball() {
  local repo="$1" pattern="$2" name="$3"
  local api_url="https://api.github.com/repos/${repo}/releases/latest"
  local url
  url="$(
    curl -fsSL "${AUTH_HEADER[@]}" -H 'Accept: application/vnd.github+json' "${api_url}" |
      jq -r --arg pat "${pattern}" '.assets[]? | select(.name | test($pat)) | .browser_download_url' |
      head -n1
  )"
  if [[ -z "${url}" ]]; then
    echo "Failed to find release asset for ${repo} matching ${pattern}" >&2
    return 1
  fi
  local tmpdir
  tmpdir="$(mktemp -d)"
  curl -fsSL "${url}" -o "${tmpdir}/archive.tar.gz"
  tar xzf "${tmpdir}/archive.tar.gz" -C "${tmpdir}"
  mv "${tmpdir}/${name}" "$HOME/go/bin/${name}"
  chmod +x "$HOME/go/bin/${name}"
  rm -rf "${tmpdir}"
}

# Pre-built binary downloads
download_github_binary "mikefarah/yq" "^yq_linux_${ARCH}$" "yq"
download_github_tarball "rhysd/actionlint" "actionlint_[0-9.]+_linux_${ARCH}\\.tar\\.gz$" "actionlint"
download_github_binary "mvdan/sh" "shfmt_v[0-9.]+_linux_${ARCH}$" "shfmt"

# Go install (no pre-built binaries available)
go install github.com/maruel/ask/cmd/ask@latest
go install github.com/go-delve/delve/cmd/dlv@latest
go install golang.org/x/tools/cmd/goimports@latest
go install golang.org/x/tools/gopls@latest

# golangci-lint v2
# TODO(https://go.dev/issue/22040): Remove CGO_ENABLED=0 once using Go 1.25+
# which accepts ld.bfd 2.36+ on arm64 (CL 740480).
CGO_ENABLED=0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
