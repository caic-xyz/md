#!/bin/bash
# Install Bun (as user)
set -euo pipefail
echo "- $0"

cd "$HOME"

# The installer appends PATH to .bashrc; cleaned up by bashrc_cleanup.sh.
# PATH setup is in bash.d/80-path.sh.
curl -fsSL https://bun.sh/install | bash
