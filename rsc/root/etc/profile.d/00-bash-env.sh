# Source md's shared Bash environment for login shells.
# shellcheck shell=bash

if [ -n "${BASH_VERSION:-}" ] && [ -r /etc/bash_env ]; then
	# shellcheck disable=SC1091
	. /etc/bash_env
fi
