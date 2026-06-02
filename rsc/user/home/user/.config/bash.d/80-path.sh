# Common PATH entries and shell defaults.
# shellcheck shell=bash

export SHELL="${SHELL:-/bin/bash}"
export PNPM_HOME="$HOME/.local/share/pnpm"
export PATH="$PNPM_HOME/bin:$PNPM_HOME:$HOME/.local/bin:$HOME/.opencode/bin:$HOME/.bun/bin:$HOME/.local/go/bin:$HOME/go/bin:$HOME/.cargo/bin:$PATH"
export EDITOR=nvim
