#!/bin/bash
# Install Android SDK tools, emulator, and system images on x64 (runs as user).
set -euo pipefail
echo "- $0"

# Create ~/.gradle and wrapper so bind-mounted subdirectories don't cause Docker to create it as root.
mkdir -p "$HOME/.gradle/wrapper"

# Detect architecture and set appropriate system image ABI.
ARCH=$(uname -m)
# linux/arm64 is still not supported; see https://issuetracker.google.com/issues/227219818
if [ "$ARCH" == "aarch64" ]; then
  echo "  Skipping Android SDK installation on $ARCH"
  exit 0
fi

ANDROID_SDK_ROOT="$HOME/.local/share/android-sdk"
mkdir -p "$ANDROID_SDK_ROOT"
cd "$ANDROID_SDK_ROOT"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

# Get the latest commandlinetools download URL dynamically.
# The URL embeds a build number that changes with each release, so we scrape
# the Studio download page. Fall back to the well-known "latest" redirect if
# the page layout changes.
SDK_URL=$(curl -fsSL "https://developer.android.com/studio" | grep -o 'https://dl\.google\.com/android/repository/commandlinetools-linux-[0-9]*_latest\.zip' | head -n 1)
if [ -z "$SDK_URL" ]; then
  echo "Failed to determine SDK URL" >&2
  exit 1
fi
curl -fsSL "$SDK_URL" -o "${TMPDIR}/cmdline-tools.zip"
unzip -q "${TMPDIR}/cmdline-tools.zip" -d "${TMPDIR}"

# Move cmdline-tools to proper location (sdkmanager expects this structure)
mkdir -p "$ANDROID_SDK_ROOT/cmdline-tools"
mv "${TMPDIR}/cmdline-tools" "$ANDROID_SDK_ROOT/cmdline-tools/latest"
chmod 755 "$ANDROID_SDK_ROOT/cmdline-tools/latest/bin"/*
SDKMANAGER="$ANDROID_SDK_ROOT/cmdline-tools/latest/bin/sdkmanager"

# Accept all licenses
yes | "$SDKMANAGER" --licenses >/dev/null 2>&1 || true

echo "Determining latest SDK versions..."
AVAILABLE_PACKAGES=$("$SDKMANAGER" --list | grep '^[[:space:]]' | awk '{print $1}')

LATEST_BUILD_TOOLS=$(echo "$AVAILABLE_PACKAGES" | grep -E '^build-tools;[0-9.]+$' | sort -V | tail -n 1)
LATEST_PLATFORM=$(echo "$AVAILABLE_PACKAGES" | grep -E '^platforms;android-[0-9]+$' | sort -V | tail -n 1)

if [ -z "$LATEST_BUILD_TOOLS" ] || [ -z "$LATEST_PLATFORM" ]; then
  echo "Error: Could not determine latest Android SDK versions."
  exit 1
fi

echo "Selected Build Tools: $LATEST_BUILD_TOOLS"
echo "Selected Platform: $LATEST_PLATFORM"

# Install required SDK components
SDK_PACKAGES=(
  "$LATEST_BUILD_TOOLS"
  "platform-tools"
  "$LATEST_PLATFORM"
)

"$SDKMANAGER" "${SDK_PACKAGES[@]}"
