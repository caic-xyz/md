# Git completion and prompt helpers.
# shellcheck shell=bash

case $- in
*i*) ;;
*) return ;;
esac

if [ -f /usr/share/bash-completion/completions/git ]; then
	# shellcheck disable=SC1091
	. /usr/share/bash-completion/completions/git
fi

if [ -f /usr/lib/git-core/git-sh-prompt ]; then
	# shellcheck disable=SC1091
	. /usr/lib/git-core/git-sh-prompt
fi

export GIT_PS1_DESCRIBE_STYLE=tag
export GIT_PS1_SHOWCOLORHINTS=1
export GIT_PS1_SHOWDIRTYSTATE=1
export GIT_PS1_SHOWSTASHSTATE=1
export GIT_PS1_SHOWUNTRACKEDFILES=1
export GIT_PS1_SHOWUPSTREAM=auto

__md_ps1() {
	local exit_code=$?
	local status=""
	if [ "${exit_code}" -ne 0 ]; then
		status="\[\e[31m\]${exit_code}\[\e[0m\]"
	fi
	local before="\[\e]0;\W\a\]\[\e[0m\]${status}"
	local after="\[\e[36m\]\h\[\e[0m\] \[\e[33m\]\w\[\e[0m\]🐳"

	if declare -F __git_ps1 >/dev/null 2>&1; then
		__git_ps1 "${before}" "${after}"
	else
		PS1="${before}${after}"
	fi
}

if [[ "${PROMPT_COMMAND:-}" == *"__md_ps1"* ]]; then
	:
elif [ -n "${PROMPT_COMMAND:-}" ]; then
	PROMPT_COMMAND="__md_ps1; ${PROMPT_COMMAND}"
else
	PROMPT_COMMAND="__md_ps1"
fi
