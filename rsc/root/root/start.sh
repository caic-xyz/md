#!/bin/bash
# Intentionally fail-fast: any startup failure should be visible immediately
# rather than masked, so the user can diagnose a broken container.
set -eu

# Generate dynamic motd with hostname
echo "Connected to $(hostname)" >/etc/motd

# If /dev/kvm exists, update the kvm group GID to match the host.
# In rootless Docker, device GIDs map to the overflow GID (65534) and groupmod
# would fail because that GID is already taken by nogroup. Skip in that case.
if [ -e /dev/kvm ]; then
	host_kvm_gid=$(stat -c %g /dev/kvm)
	current_kvm_gid=$(getent group kvm | cut -d: -f3)
	if [ "$host_kvm_gid" != "$current_kvm_gid" ]; then
		existing=$(getent group "$host_kvm_gid" | cut -d: -f1)
		if [ -z "$existing" ]; then
			groupmod -g "$host_kvm_gid" kvm
		fi
	fi
fi

# Rootless container runtime detection: if UID 0 inside the container maps to a
# non-root host UID, bind-mounted host directories appear root-owned but the
# "user" account (UID 1000) can't write to them. In this case, add "user" to
# the "root" group.
# Skip when --userns=keep-id already mapped the host UID correctly (podman),
# detected by checking that "user" is no longer UID 1000.
if awk '$1 == 0 && $2 != 0 { found=1 } END { exit !found }' /proc/self/uid_map &&
	[ "$(id -u user)" != "0" ]; then
	usermod -aG root user
fi

# Start dbus service and ensure user has a DBus session available
echo "[start.sh] Starting dbus service..."
/etc/init.d/dbus start
echo "[start.sh] Setting up persistent DBus session for user..."
session_file="/home/user/.dbus-session-env"
su - user -c "dbus-launch --sh-syntax > '$session_file'"
chown user:user "$session_file"
cat <<EOF >/etc/profile.d/50-dbus-session.sh
if [ -f "$session_file" ]; then
    . "$session_file"
    export DBUS_SESSION_BUS_ADDRESS
fi
EOF

# Start XFCE4 and VNC
if [ -n "${MD_DISPLAY:-}" ]; then
	# Start Xvnc + XFCE with monitors (runs as root, unkillable by user)
	/root/vnc-start.sh
else
	echo "[start.sh] MD_DISPLAY not set, skipping X/VNC startup"
fi

# Start Tailscale if enabled
if [ -n "${MD_TAILSCALE:-}" ]; then
	echo "[start.sh] Starting Tailscale..."
	# /dev/net/tun is passed through from the host via --device (see docker.go).
	# The tun kernel module must be loaded on the host for this to work.
	tailscaled --state=/var/lib/tailscale/tailscaled.state &
	# Wait for tailscaled to be ready
	for _ in $(seq 1 30); do
		if tailscale status >/dev/null 2>&1; then
			break
		fi
		sleep 0.1
	done
	if [ -n "${TAILSCALE_AUTHKEY:-}" ]; then
		tailscale up --hostname="$(hostname)" --ssh --authkey="$TAILSCALE_AUTHKEY"
		# Allow non-root users to access tailscale CLI (must be after tailscale up)
		tailscale set --operator=user
		# Update MOTD with Tailscale FQDN and VNC URL if display is enabled
		ts_fqdn=$(tailscale status --json | jq -r '.Self.DNSName // empty' | sed 's/\.$//')
		if [ -n "$ts_fqdn" ]; then
			echo "Connected to $ts_fqdn" >/etc/motd
			if [ -n "${MD_DISPLAY:-}" ]; then
				echo "VNC: vnc://$ts_fqdn:5901" >>/etc/motd
			fi
			echo "[start.sh] Tailscale connected: $ts_fqdn"
		fi
	else
		# Capture auth URL for the host to display (MOTD not updated without authkey).
		# tailscale up blocks until user authenticates via the URL, then set operator.
		(
			tailscale up --hostname="$(hostname)" --ssh 2>&1 | tee /tmp/tailscale_auth_url
			tailscale set --operator=user
		) &
	fi
fi

# If /dev/bus/usb exists, grant plugdev access and match host GID.
# Same rootless Docker guard as above for the kvm group.
if [ -d /dev/bus/usb ]; then
	usermod -aG plugdev user
	host_plugdev_gid=$(stat -c %g /dev/bus/usb/001/* 2>/dev/null | grep -v '^0$' | head -1)
	if [ -n "$host_plugdev_gid" ]; then
		current_plugdev_gid=$(getent group plugdev | cut -d: -f3)
		if [ "$host_plugdev_gid" != "$current_plugdev_gid" ]; then
			existing=$(getent group "$host_plugdev_gid" | cut -d: -f1)
			if [ -z "$existing" ]; then
				groupmod -g "$host_plugdev_gid" plugdev
			fi
		fi
	fi
fi

# When -sudo was passed (MD_SUDO_PASSWORD is set), grant sudo access and
# fix /proc so rootless Podman can mount a new /proc inside nested user
# namespaces. /proc must not be nosuid (breaks newuidmap), and Docker's
# tmpfs masks + proc submounts must be unmounted (kernel requires fully
# visible /proc). Both fixes require SYS_ADMIN, granted by -sudo.
# See: https://www.redhat.com/sysadmin/podman-inside-container
#      https://github.com/containers/podman/discussions/28307
#      https://github.com/containers/podman/issues/4131
#      https://github.com/containers/podman/issues/10864
if [ -n "${MD_SUDO_PASSWORD:-}" ]; then
	usermod -aG sudo user
	echo "user:$MD_SUDO_PASSWORD" | chpasswd
	unset MD_SUDO_PASSWORD

	if findmnt -no OPTIONS /proc | grep -q nosuid; then
		mount -o remount,rw,suid /proc 2>/dev/null || true
	fi
	if findmnt -no OPTIONS /proc | grep -q nosuid; then
		echo "[start.sh] WARNING: /proc remount failed (nosuid still set) — rootless Podman may not work"
	else
		echo "[start.sh] Remounted /proc without nosuid for rootless Podman"
	fi
	# Unmount all submounts under /proc (deepest first) so the kernel
	# sees /proc as fully visible for nested user namespaces.
	findmnt -nl -o TARGET --submounts /proc | sort -r | while read -r p; do
		[ "$p" = "/proc" ] && continue
		umount "$p" 2>/dev/null || true
	done
	echo "[start.sh] Unmasked Docker /proc paths for rootless Podman"
fi

# Start SSH server (after VNC so DISPLAY is available)
service ssh start

sleep infinity
