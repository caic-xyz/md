# Bun paths.
# shellcheck disable=SC2148
if [ -d "${HOME}/.bun/bin" ]; then
	PATH="${HOME}/.bun/bin:${PATH}"
fi
export PATH
