#!/bin/bash
# Install extrepo packages: Google Chrome (amd64 only), GitHub CLI, Tailscale.
set -euo pipefail
echo "- $0"

export DEBIAN_FRONTEND="${DEBIAN_FRONTEND:-noninteractive}"

ARCH="$(dpkg --print-architecture)"
PACKAGES=(gh tailscale)

if [[ "$ARCH" == "amd64" ]]; then
	# Google Chrome only available for amd64 as of 2026-01-16.
	extrepo enable google_chrome
	# Refresh the signing key; extrepo's bundled copy may lack newer subkeys.
	curl -fsSL https://dl.google.com/linux/linux_signing_key.pub \
		-o /var/lib/extrepo/keys/google_chrome.asc
	PACKAGES+=(google-chrome-stable)
fi
extrepo enable github-cli
extrepo enable tailscale
apt-get update -qq >/dev/null
apt-get install -qq -y --no-install-recommends "${PACKAGES[@]}" >/dev/null

if [[ "$ARCH" == "amd64" ]]; then
	# Register google-chrome as www-browser at the same priority it uses
	# for x-www-browser (200), so it overrides chromium's lower priority (40).
	update-alternatives --install /usr/bin/www-browser www-browser /usr/bin/google-chrome-stable 200
fi
