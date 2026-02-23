#!/bin/bash
# Install Python development tools (runs as user)
set -euo pipefail
echo "- $0"

# Install uv package manager
curl -LsSf https://astral.sh/uv/install.sh | sh -s -- --quiet

# shellcheck disable=SC1091
. "$HOME/.local/bin/env" 2>/dev/null || true

# Install pip in user mode.
curl -sSL https://bootstrap.pypa.io/get-pip.py | python3 - --user --quiet --break-system-packages

# Install virtualenv and pyyaml (claude sometimes spontaneously uses them)
"$HOME/.local/bin/pip" install --user --quiet --break-system-packages virtualenv pyyaml

# Install Python development tools
uv tool install --quiet black
uv tool install --quiet pylint
uv tool install --quiet ruff
# Kimi CLI: belongs in 7_llm_tools.sh but uv isn't guaranteed available there
# due to parallel execution of setup scripts.
uv tool install --quiet kimi-cli
