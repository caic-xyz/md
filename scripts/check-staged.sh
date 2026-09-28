#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Checks staged files and the AGENTS.md index before committing.

set -euo pipefail

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(CDPATH='' cd -- "$script_dir/.." && pwd)"
readonly repo_root

cd -- "$repo_root"

if ! git diff --quiet; then
  printf '%s\n' 'Stage all tracked changes before committing; checks read the worktree.' >&2
  exit 1
fi

python3 scripts/lint_binaries.py

python3 scripts/update_agents_file_index.py --check
