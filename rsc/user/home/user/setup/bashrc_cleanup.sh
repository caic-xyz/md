#!/bin/bash
# Remove installer-appended lines from .bashrc.
# PATH setup for these tools is handled by ~/.config/bash.d/ scripts
# sourced via /etc/bash_env.
#
# nvm is handled at install time via PROFILE=/dev/null.
# bun, opencode, amp, and claude don't support a similar mechanism, so we clean up after.
set -euo pipefail
echo "- $0"

bashrc="$HOME/.bashrc"

# Remove bun block (comment + export + PATH line)
sed -i '/^# bun$/,/^export PATH=.*\.bun.*$/d' "$bashrc"

# Remove opencode block
sed -i '/^# opencode$/,/^export PATH=.*\.opencode.*$/d' "$bashrc"

# Remove trailing blank lines
sed -i -e :a -e '/^\n*$/{$d;N;ba' -e '}' "$bashrc"
