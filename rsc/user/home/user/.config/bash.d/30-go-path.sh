# Go toolchain paths.
# shellcheck disable=SC2148
if [ -d "${HOME}/.local/go/bin" ]; then
	PATH="${HOME}/.local/go/bin:${PATH}"
fi
if [ -d "${HOME}/go/bin" ]; then
	PATH="${HOME}/go/bin:${PATH}"
fi
export PATH
