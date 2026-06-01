# OpenCode paths.
# shellcheck disable=SC2148
if [ -d "${HOME}/.opencode/bin" ]; then
	PATH="${HOME}/.opencode/bin:${PATH}"
fi
export PATH
