# shellcheck shell=bash
# System-wide .bashrc file for interactive bash(1) shells.
#
# This overrides the Debian default /etc/bash.bashrc. Differences:
# - Sources /etc/bash_env before the interactive guard (PATH, env vars for
#   interactive shells)
# - Removed commented-out bash-completion block (unused)
# - Reformatted to pass shellcheck/shfmt

# Source environment (PATH, API keys, etc.) for interactive shells.
# Non-interactive and login shells are handled by BASH_ENV and /etc/profile.d.
# shellcheck disable=SC1091
[ -r /etc/bash_env ] && . /etc/bash_env

# If not running interactively, don't do anything
[ -z "${PS1-}" ] && return

# check the window size after each command and, if necessary,
# update the values of LINES and COLUMNS.
shopt -s checkwinsize

# set variable identifying the chroot you work in (used in the prompt below)
if [ -z "${debian_chroot:-}" ] && [ -r /etc/debian_chroot ]; then
	debian_chroot=$(</etc/debian_chroot)
fi

# set a fancy prompt (non-color, overwrite the one in /etc/profile)
# but only if not SUDOing and have SUDO_PS1 set; then assume smart user.
if [ -z "${SUDO_USER-}" ] || [ -z "${SUDO_PS1-}" ]; then
	PS1='${debian_chroot:+($debian_chroot)}\u@\h:\w\$ '
fi

# if the command-not-found package is installed, use it
if [ -x /usr/lib/command-not-found ] || [ -x /usr/share/command-not-found/command-not-found ]; then
	command_not_found_handle() {
		if [ -x /usr/lib/command-not-found ]; then
			/usr/lib/command-not-found -- "$1"
			return $?
		elif [ -x /usr/share/command-not-found/command-not-found ]; then
			/usr/share/command-not-found/command-not-found -- "$1"
			return $?
		else
			printf "%s: command not found\n" "$1" >&2
			return 127
		fi
	}
fi
