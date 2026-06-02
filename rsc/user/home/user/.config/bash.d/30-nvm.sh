# NVM (Node Version Manager) paths.
# shellcheck shell=bash
export NVM_DIR="${HOME}/.nvm"
if [ -d "${NVM_DIR}" ]; then
	# Add the default node version's bin to PATH without loading the full nvm function.
	# Use the last (highest) installed version as a fast approximation.
	__nvm_latest=""
	for __nvm_d in "${NVM_DIR}"/versions/node/*/bin; do
		__nvm_latest="${__nvm_d}"
	done
	if [ -n "${__nvm_latest}" ] && [ -d "${__nvm_latest}" ]; then
		PATH="${__nvm_latest}:${PATH}"
	fi
	unset __nvm_d __nvm_latest
	# Source nvm function only for interactive shells (completions, nvm use, etc.)
	case $- in
	*i*)
		# shellcheck disable=SC1091
		[ -s "${NVM_DIR}/nvm.sh" ] && \. "${NVM_DIR}/nvm.sh"
		# shellcheck disable=SC1091
		[ -s "${NVM_DIR}/bash_completion" ] && \. "${NVM_DIR}/bash_completion"
		;;
	esac
fi
export PATH
