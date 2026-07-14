#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Intentionally fail-fast: any startup failure should be visible immediately
# rather than masked, so the user can diagnose a broken container.
set -eu

# Generate dynamic motd with hostname
echo "Connected to $(hostname)" >/etc/motd

# Pre-create the Tailscale well-known path and file so the host tooling
# can inotify-wait for the first write.
mkdir -p /run/md /var/lib/md
: >/run/md/tailscale_auth_url.json

write_tailscale_device_id() {
	local ts_id
	ts_id=$(tailscale status --json 2>/dev/null | jq -r '.Self.ID // empty' || true)
	if [ -n "$ts_id" ]; then
		echo "$ts_id" >/var/lib/md/tailscale_device_id
	fi
}

write_environment_var() {
	local key="$1"
	local value="$2"
	local tmp
	tmp=$(mktemp)
	if [ -f /etc/environment ]; then
		grep -v "^${key}=" /etc/environment >"$tmp" || true
	fi
	printf '%s=%s\n' "$key" "$value" >>"$tmp"
	cat "$tmp" >/etc/environment
	rm -f "$tmp"
}

remove_environment_var() {
	local key="$1"
	local tmp
	if [ ! -f /etc/environment ]; then
		return
	fi
	tmp=$(mktemp)
	grep -v "^${key}=" /etc/environment >"$tmp" || true
	cat "$tmp" >/etc/environment
	rm -f "$tmp"
}

rewrite_user_identity() {
	local target_uid="$1"
	local target_gid="$2"
	local tmp

	# usermod -u/-g rewrites matching files under /home/user synchronously.
	# Update the account database directly so SSH can start before the broad
	# ownership repair runs in the background.
	tmp=$(mktemp)
	awk -F: -v OFS=: -v uid="$target_uid" -v gid="$target_gid" '
		$1 == "user" {
			$3 = uid
			$4 = gid
			found = 1
		}
		{ print }
		END {
			if (found != 1) {
				exit 1
			}
		}
	' /etc/passwd >"$tmp"
	cat "$tmp" >/etc/passwd
	rm -f "$tmp"

	tmp=$(mktemp)
	awk -F: -v OFS=: -v gid="$target_gid" '
		$1 == "user" {
			$3 = gid
			found = 1
		}
		{ print }
		END {
			if (found != 1) {
				exit 1
			}
		}
	' /etc/group >"$tmp"
	cat "$tmp" >/etc/group
	rm -f "$tmp"
}

configure_host_user_id() {
	local target_uid="${MD_HOST_UID:-}"
	local target_gid="${MD_HOST_GID:-}"
	case "$target_uid" in
	"" | *[!0-9]*)
		return
		;;
	esac
	case "$target_gid" in
	"" | *[!0-9]*)
		return
		;;
	esac
	if [ "$target_uid" = 0 ] || [ "$target_gid" = 0 ]; then
		return
	fi

	local old_uid old_gid
	old_uid=$(id -u user)
	old_gid=$(id -g user)
	if [ "$target_uid" = "$old_uid" ] && [ "$target_gid" = "$old_gid" ]; then
		return
	fi
	rewrite_user_identity "$target_uid" "$target_gid"
	for path in /home/user /home/user/.ssh /home/user/.ssh/authorized_keys; do
		if [ -e "$path" ] || [ -L "$path" ]; then
			chown -h user:user "$path"
		fi
	done
	HOME_REPAIR_OLD_UID="$old_uid"
	HOME_REPAIR_OLD_GID="$old_gid"
}

repair_home_ownership() {
	local old_uid="$1"
	local old_gid="$2"
	if ! nice -n 19 find /home/user -xdev \( -uid "$old_uid" -o -gid "$old_gid" \) -exec chown -h user:user {} +; then
		echo "[start.sh] WARNING: background /home/user ownership repair failed"
	fi
}

start_home_ownership_repair() {
	local old_uid="${HOME_REPAIR_OLD_UID:-}"
	local old_gid="${HOME_REPAIR_OLD_GID:-}"
	if [ -z "$old_uid" ] || [ -z "$old_gid" ]; then
		return
	fi
	repair_home_ownership "$old_uid" "$old_gid" &
}

configure_host_user_id

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
rm -f "$session_file" /etc/profile.d/50-dbus-session.sh
if su user -s /bin/bash -c "env -u DBUS_SESSION_BUS_ADDRESS dbus-launch --sh-syntax" >"$session_file"; then
	chown user:user "$session_file"
	chmod 600 "$session_file"
	# shellcheck disable=SC1090
	. "$session_file"
	write_environment_var DBUS_SESSION_BUS_ADDRESS "$DBUS_SESSION_BUS_ADDRESS"
	cat <<EOF >/etc/profile.d/50-dbus-session.sh
if [ -f "$session_file" ]; then
    . "$session_file"
    export DBUS_SESSION_BUS_ADDRESS
fi
EOF
else
	echo "[start.sh] WARNING: DBus session setup failed, continuing without DBUS_SESSION_BUS_ADDRESS"
	rm -f "$session_file"
	remove_environment_var DBUS_SESSION_BUS_ADDRESS
fi

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
	if [ -n "${MD_TAILSCALE_RESET:-}" ]; then
		rm -rf /var/lib/tailscale /run/tailscale
		rm -f /var/lib/md/tailscale_device_id
		mkdir -p /var/lib/tailscale
	fi
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
		write_tailscale_device_id
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
		# Redirect stdout+stderr to the well-known file. tailscale up --json
		# flushes each JSON line immediately, so the file is readable as soon
		# as inotify fires on the first write.
		(
			tailscale up --hostname="$(hostname)" --ssh --json >/run/md/tailscale_auth_url.json 2>&1
			tailscale set --operator=user
			write_tailscale_device_id
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
else
	if id -nG user | tr ' ' '\n' | grep -qx sudo; then
		deluser user sudo >/dev/null 2>&1 || true
	fi
	if id -nG user | tr ' ' '\n' | grep -qx sudo; then
		echo "[start.sh] ERROR: user remains in sudo group"
		exit 1
	fi
	passwd -l user >/dev/null
fi

# Start SSH server (after VNC so DISPLAY is available)
rm -f /run/sshd.pid /var/run/sshd.pid
rm -rf /run/sshd
install -d -m 0755 /run/sshd
chown user:user /home/user /home/user/.ssh /home/user/.ssh/authorized_keys
chmod 0700 /home/user /home/user/.ssh
chmod 0400 /home/user/.ssh/authorized_keys
find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*_key' -exec chown root:root {} + -exec chmod 0600 {} +
find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*_key.pub' -exec chown root:root {} + -exec chmod 0644 {} +
if ! service ssh start; then
	echo "[start.sh] ERROR: sshd failed config validation:"
	/usr/sbin/sshd -t -e || true
	exit 1
fi
start_home_ownership_repair

sleep infinity
