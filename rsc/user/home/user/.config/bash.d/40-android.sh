# Android SDK paths.
# shellcheck disable=SC2148
if [ -d "$ANDROID_SDK_ROOT/cmdline-tools" ]; then
	export ANDROID_SDK_ROOT
	export ANDROID_HOME="$ANDROID_SDK_ROOT"
	export PATH="$ANDROID_SDK_ROOT/cmdline-tools/latest/bin:$ANDROID_SDK_ROOT/platform-tools:$ANDROID_SDK_ROOT/emulator:$PATH"
	# Enable KVM for emulator acceleration (if available)
	export ANDROID_EMULATOR_KVM_DEVICE="/dev/kvm"
fi
