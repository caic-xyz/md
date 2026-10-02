#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Run numbered specialized-image setup scripts as the image's development account.
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo 'md: run-user-setup takes no arguments' >&2
  exit 1
fi

# The current image contract uses UID 1000. Resolve its name and home from
# passwd so changing either does not require changing the Dockerfile.
entry="$(getent passwd 1000)" || {
  echo 'md: specialized image is missing its UID 1000 development account' >&2
  exit 1
}
IFS=: read -r account _ _ _ _ account_home _ <<<"$entry"
if [[ -z "$account" || -z "$account_home" ]]; then
  echo 'md: specialized image has an invalid UID 1000 development account' >&2
  exit 1
fi

if [[ $(id -u) -eq 0 ]]; then
  exec su -s /bin/bash -c /usr/local/bin/md-run-user-setup "$account"
fi
if [[ $(id -u) -ne 1000 ]]; then
  echo 'md: user setup did not switch to the development account' >&2
  exit 1
fi

export HOME="$account_home"
cd -- "$HOME"
setup_dir=/usr/local/share/md/user_setup
shopt -s nullglob
scripts=("$setup_dir"/*)
if [[ ${#scripts[@]} -eq 0 ]]; then
  echo 'md: specialized image has no user setup scripts' >&2
  exit 1
fi
for script in "${scripts[@]}"; do
  name="${script##*/}"
  if [[ ! -f "$script" || ! -x "$script" || ! "$name" =~ ^[0-9]+_.*\.sh$ ]]; then
    echo "md: invalid user setup script: $script" >&2
    exit 1
  fi
  "$script"
done
