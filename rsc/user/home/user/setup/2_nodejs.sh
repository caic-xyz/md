#!/bin/bash
# Install nvm, node.js, standalone pnpm, and global Node packages (as user).
set -euo pipefail
echo "- $0"

cd "$HOME"

# 1. Setup Node.js via NVM
if ! which nvm &>/dev/null; then
  # TODO: Update from time to time.
  # PROFILE=/dev/null prevents nvm from appending to .bashrc; PATH setup is in bash.d/30-nvm.sh.
  PROFILE=/dev/null curl -sSL -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash
  # shellcheck disable=SC1090
  . ~/.nvm/nvm.sh
fi

if ! which node &>/dev/null; then
  # Lock to v24 as @sourcegraph/amp requires it as of 2025-10-15. Switch back to "node" to use latest.
  nvm install --no-progress v24
fi

export PNPM_HOME="$HOME/.local/share/pnpm"
mkdir -p "$PNPM_HOME/bin"
export PATH="$PNPM_HOME/bin:$PATH"
# The shared Bash environment owns PATH; redirect the installer's shell configuration.
(
  config=$(mktemp)
  trap 'unlink "$config"' EXIT
  curl -fsSL https://get.pnpm.io/install.sh | ENV="$config" SHELL=/bin/sh sh -
)

# 2. Install Global Node Packages
pnpm add -g \
  agent-browser \
  @kilocode/cli \
  @earendil-works/pi-coding-agent \
  @openai/codex \
  @qwen-code/qwen-code \
  chrome-devtools-mcp \
  eslint \
  prettier \
  tsx \
  typescript \
  typescript-eslint \
  vscode-langservers-extracted
