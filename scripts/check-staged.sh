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

if ! git diff --quiet || [[ -n "$(git ls-files --others --exclude-standard)" ]]; then
  printf '%s\n' 'Stage or remove all changes before committing; checks run against the staged snapshot.' >&2
  exit 1
fi

uv run --locked python scripts/lint_binaries.py

uv run --locked python scripts/update_agents_file_index.py --check
