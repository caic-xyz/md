#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Install current coding agents in a specialized image.
set -euo pipefail

export NVM_DIR="$HOME/.nvm"
export PNPM_HOME="$HOME/.local/share/pnpm"
for node_bin in "$NVM_DIR"/versions/node/*/bin; do
  if [[ -d "$node_bin" ]]; then
    PATH="$node_bin:$PATH"
  fi
done
export PATH

if ! command -v node >/dev/null 2>&1; then
  PROFILE=/dev/null curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash
  # shellcheck disable=SC1091
  . "$NVM_DIR/nvm.sh"
  nvm install --no-progress v24
fi
export PATH="$HOME/.local/bin:$PNPM_HOME/bin:$PATH"
if ! command -v pnpm >/dev/null 2>&1; then
  # Custom bases may lack pnpm. Install standalone without changing shell profiles.
  (
    config=$(mktemp)
    trap 'unlink "$config"' EXIT
    curl -fsSL https://get.pnpm.io/install.sh | ENV="$config" SHELL=/bin/sh sh -
  )
fi
# Install OpenCode through npm's registry to avoid GitHub release API rate limits.
# Its postinstall selects the executable from its platform-specific npm package.
pnpm add -g --allow-build=opencode-ai \
  @openai/codex@latest @earendil-works/pi-coding-agent@latest opencode-ai@latest

# A custom install directory avoids the installer's shell-profile edits.
# The endpoint can serve a gzip Content-Encoding even without Accept-Encoding.
curl -fsSL --compressed https://antigravity.google/cli/install.sh | bash -s -- --dir "$HOME/.local/bin"

mkdir -p "$HOME/.claude"
if [[ ! -f "$HOME/.claude/claude.json" ]]; then
  printf '{}\n' >"$HOME/.claude/claude.json"
fi
ln -sf "$HOME/.claude/claude.json" "$HOME/.claude.json"
curl -fsSL https://claude.ai/install.sh | bash
ln -sf "$HOME/.claude/claude.json" "$HOME/.claude.json"

for agent in agy claude codex opencode pi; do
  if ! command -v "$agent" >/dev/null 2>&1; then
    echo "md: $agent installation did not provide an executable" >&2
    exit 1
  fi
done

if [[ -f "$HOME/src/tool_versions.md" ]]; then
  {
    printf '\n## Specialized Image Coding Agents\n\n| Tool | Version |\n| :--- | :--- |\n'
    for agent in agy claude codex opencode pi; do
      version="$($agent --version 2>&1)"
      version="${version%%$'\n'*}"
      printf '| %s | %s |\n' "$agent" "${version//|/\\|}"
    done
  } >>"$HOME/src/tool_versions.md"
fi
