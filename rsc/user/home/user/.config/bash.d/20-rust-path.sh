# Rust toolchain paths.
# shellcheck disable=SC2148
if [ -d "${HOME}/.cargo/bin" ]; then
	PATH="${HOME}/.cargo/bin:${PATH}"
fi
export PATH
