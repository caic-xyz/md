#!/bin/bash
# Install Standalone LLM Tools: OpenCode, Amp, Claude (as user)
set -euo pipefail
echo "- $0"

cd "$HOME"

# OpenCode
# The installer appends PATH to .bashrc; cleaned up by bashrc_cleanup.sh.
# PATH setup is in bash.d/80-path.sh.
curl -fsSL https://opencode.ai/install | bash

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
