#!/bin/bash
# Install Standalone LLM Tools: OpenCode, Amp, Claude (as user)
set -euo pipefail
echo "- $0"

cd "$HOME"

# OpenCode
# The installer appends PATH to .bashrc; cleaned up by bashrc_cleanup.sh.
# PATH setup is in bash.d/80-path.sh.
# Resolve the version first so GITHUB_TOKEN avoids the installer's unauthenticated API limit.
github_headers=()
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  github_headers=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi
opencode_version="$(
  curl -fsSL "${github_headers[@]}" \
    -H 'Accept: application/vnd.github+json' \
    https://api.github.com/repos/anomalyco/opencode/releases/latest |
    jq -er '.tag_name | ltrimstr("v")'
)"
readonly opencode_version
curl -fsSL https://opencode.ai/install | VERSION="$opencode_version" bash

# Amp
# Note: Amp may require Node.js v24 environment to run, but the installer is standalone.
curl -fsSL https://ampcode.com/install.sh | bash

# Claude Code
# Handling configuration linking for the installer
mkdir -p "$HOME/.claude"
if [ ! -f "$HOME/.claude/claude.json" ]; then
  echo "{}" >"$HOME/.claude/claude.json"
fi
ln -sf "$HOME/.claude/claude.json" "$HOME/.claude.json"
curl -fsSL https://claude.ai/install.sh | bash
rm "$HOME/.claude.json"
ln -sf "$HOME/.claude/claude.json" "$HOME/.claude.json"
