#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Container entrypoint: md runs this as root to prepare the container, then idles.
#
# Intentionally fail-fast: any startup failure should be visible immediately
# rather than masked, so the user can diagnose a broken container.
#
# Capability model. md runs on any conforming base image, so nothing beyond the
# startup contract is assumed to exist. Every capability is either always
# required or requested through an md option (an MD_* environment variable or a
# device md passes through):
#
#   always      sshd, the md SSH key, the fixed UID/GID 1000 "user" account
#   -display    Xvnc + XFCE (MD_DISPLAY)
#   -tailscale  tailscaled, tailscale, jq, /dev/net/tun (MD_TAILSCALE)
#   -sudo       sudo and password tooling (MD_SUDO_PASSWORD)
#   -usb        /dev/bus/usb and serial adapters
#   /dev/kvm    KVM acceleration
#   dbus        optional everywhere; skipped when absent
#
# A missing capability that was not requested is skipped. A missing capability
# that was requested fails with the capability and the requesting option named
# in the error, before the container is usable.

set -euo pipefail

MD_USER=user
MD_HOME=/home/user
# MD_GROUP is the account's primary group. It is resolved by ensure_account and
# is "user" for every image md builds itself.
MD_GROUP=
readonly MD_USER MD_HOME

log() {
  echo "[start.sh] $*"
}

fail() {
  echo "[start.sh] ERROR: $*" >&2
  exit 1
}

# require_capability CAPABILITY OPTION SPEC...
#
# SPEC is one of cmd:NAME, file:PATH or group:NAME. Every missing requirement is
# reported at once, naming the capability and the md option that wanted it.
require_capability() {
  local capability="$1" option="$2"
  shift 2
  local spec missing=()
  for spec in "$@"; do
    case "$spec" in
    cmd:*)
      command -v "${spec#cmd:}" >/dev/null 2>&1 || missing+=("${spec#cmd:} command")
      ;;
    file:*)
      [ -e "${spec#file:}" ] || missing+=("${spec#file:}")
      ;;
    group:*)
      getent group "${spec#group:}" >/dev/null 2>&1 || missing+=("group ${spec#group:}")
      ;;
    *)
      fail "internal error: unknown requirement '$spec'"
      ;;
    esac
  done
  if [ "${#missing[@]}" -ne 0 ]; then
    local joined
    printf -v joined ', %s' "${missing[@]}"
    fail "$capability is missing ${joined#, }, requested by $option"
  fi
}

write_environment_var() {
  local key="$1" value="$2" tmp
  tmp="$(mktemp)"
  if [ -f /etc/environment ]; then
    grep -v "^${key}=" /etc/environment >"$tmp" || true
  fi
  printf '%s=%s\n' "$key" "$value" >>"$tmp"
  cat "$tmp" >/etc/environment
  rm -f "$tmp"
}

remove_environment_var() {
  local key="$1" tmp
  if [ ! -f /etc/environment ]; then
    return
  fi
  tmp="$(mktemp)"
  grep -v "^${key}=" /etc/environment >"$tmp" || true
  cat "$tmp" >/etc/environment
  rm -f "$tmp"
}

# preflight resolves SSHD_BIN and reports every missing requirement of the
# requested capabilities before any side effect. It is the single definition of
# the md image contract: the specialized image build runs it through --check, and
# runtime startup runs it before preparing the container.
preflight() {
  SSHD_BIN=""
  if command -v sshd >/dev/null 2>&1; then
    SSHD_BIN="$(command -v sshd)"
  elif [ -x /usr/sbin/sshd ]; then
    SSHD_BIN=/usr/sbin/sshd
  fi
  [ -n "$SSHD_BIN" ] || fail "md startup is missing sshd, requested by md start"

  require_capability "md startup" "md start" \
    cmd:useradd cmd:groupadd cmd:usermod cmd:getent cmd:id \
    cmd:chown cmd:chmod cmd:install cmd:awk cmd:mktemp cmd:find \
    cmd:git cmd:grep cmd:groupmod cmd:head cmd:hostname cmd:passwd cmd:sleep cmd:stat cmd:tr \
    file:/etc/ssh/sshd_config file:/etc/ssh/ssh_host_ed25519_key \
    "file:$MD_HOME/.ssh/authorized_keys"

  if [ -n "${MD_DISPLAY:-}" ]; then
    require_capability "the desktop capability" "md start -display (MD_DISPLAY)" \
      cmd:Xvnc cmd:startxfce4 cmd:su cmd:pgrep cmd:seq cmd:tail cmd:tee \
      file:/root/vnc-start.sh file:/root/xvnc-monitor.sh file:/root/xfce-monitor.sh
  fi

  if [ -n "${MD_TAILSCALE:-}" ]; then
    require_capability "the Tailscale capability" "md start -tailscale (MD_TAILSCALE)" \
      cmd:tailscaled cmd:tailscale cmd:jq cmd:seq file:/dev/net/tun
  fi

  if [ -n "${MD_SUDO_PASSWORD:-}" ]; then
    require_capability "the sudo capability" "md start -sudo (MD_SUDO_PASSWORD)" \
      cmd:sudo cmd:chpasswd cmd:findmnt cmd:mount cmd:sort cmd:umount group:sudo
  fi
}

# ensure_account provisions the fixed UID/GID 1000 account on a base image that
# does not have it, and resolves the primary group used for the home directory.
# Idempotent: an existing account is left untouched.
ensure_account() {
  if id -u "$MD_USER" >/dev/null 2>&1; then
    MD_GROUP="$(id -gn "$MD_USER")"
    log "using existing $MD_USER account (UID/GID $(id -u "$MD_USER"):$(id -g "$MD_USER"))"
    return
  fi
  if getent passwd 1000 >/dev/null 2>&1; then
    fail "md startup cannot create the $MD_USER account: UID 1000 is already used by '$(id -nu 1000)'"
  fi
  if getent group 1000 >/dev/null 2>&1; then
    local gid_owner
    gid_owner="$(getent group 1000 | awk -F: '{print $1}')"
    fail "md startup cannot create the $MD_USER group: GID 1000 is already used by '$gid_owner'"
  fi
  local current_gid
  current_gid="$(getent group "$MD_USER" | awk -F: '{print $3}' || true)"
  if [ -z "$current_gid" ]; then
    groupadd --gid 1000 "$MD_USER" || fail "md startup cannot create the $MD_USER group with GID 1000"
  elif [ "$current_gid" != 1000 ]; then
    groupmod --gid 1000 "$MD_USER" || fail "md startup cannot move the $MD_USER group to GID 1000"
  fi
  useradd --uid 1000 --gid 1000 --home-dir "$MD_HOME" --shell /bin/bash --create-home "$MD_USER" ||
    fail "md startup cannot create the $MD_USER account with UID 1000"
  MD_GROUP="$MD_USER"
  log "created the $MD_USER account (UID/GID 1000)"
}

# ensure_directories creates the directories md relies on, idempotently.
ensure_directories() {
  install -d -m 0755 /run/md /var/lib/md
  # Pre-create the Tailscale well-known path and file so the host tooling can
  # inotify-wait for the first write.
  : >/run/md/tailscale_auth_url.json
  install -d -m 0755 -o "$MD_USER" -g "$MD_GROUP" "$MD_HOME"
  install -d -m 0700 -o "$MD_USER" -g "$MD_GROUP" "$MD_HOME/.ssh"
}

rewrite_user_identity() {
  local target_uid="$1"
  local target_gid="$2"
  local tmp

  # usermod -u/-g rewrites matching files under /home/user synchronously.
  # Update the account database directly so SSH can start before the broad
  # ownership repair runs in the background.
  tmp="$(mktemp)"
  if ! awk -F: -v OFS=: -v uid="$target_uid" -v gid="$target_gid" -v name="$MD_USER" '
		$1 == name {
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
	' /etc/passwd >"$tmp"; then
    rm -f "$tmp"
    fail "the $MD_USER account is missing from /etc/passwd"
  fi
  cat "$tmp" >/etc/passwd
  rm -f "$tmp"

  tmp="$(mktemp)"
  if ! awk -F: -v OFS=: -v gid="$target_gid" -v name="$MD_GROUP" '
		$1 == name {
			$3 = gid
			found = 1
		}
		{ print }
		END {
			if (found != 1) {
				exit 1
			}
		}
	' /etc/group >"$tmp"; then
    rm -f "$tmp"
    fail "the $MD_GROUP group is missing from /etc/group"
  fi
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
  old_uid="$(id -u "$MD_USER")"
  old_gid="$(id -g "$MD_USER")"
  local uid_owner gid_owner
  uid_owner="$(awk -F: -v id="$target_uid" -v name="$MD_USER" '$3 == id && $1 != name { print $1; exit }' /etc/passwd)"
  gid_owner="$(awk -F: -v id="$target_gid" -v name="$MD_GROUP" '$3 == id && $1 != name { print $1; exit }' /etc/group)"
  if [ -n "$uid_owner" ]; then
    fail "md startup cannot map $MD_USER to requested host UID $target_uid: it is already used by '$uid_owner'"
  fi
  if [ -n "$gid_owner" ]; then
    fail "md startup cannot map $MD_GROUP to requested host GID $target_gid: it is already used by '$gid_owner'"
  fi
  if [ "$target_uid" = "$old_uid" ] && [ "$target_gid" = "$old_gid" ]; then
    log "$MD_USER account already has requested host UID/GID $target_uid:$target_gid"
    return
  fi
  log "changing $MD_USER account identity from $old_uid:$old_gid to requested host UID/GID $target_uid:$target_gid"
  rewrite_user_identity "$target_uid" "$target_gid"
  for path in "$MD_HOME" "$MD_HOME/.ssh" "$MD_HOME/.ssh/authorized_keys"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      chown -h "$MD_USER:$MD_GROUP" "$path"
    fi
  done
  HOME_REPAIR_OLD_UID="$old_uid"
  HOME_REPAIR_OLD_GID="$old_gid"
}

repair_home_ownership() {
  local old_uid="$1"
  local old_gid="$2"
  local -a repair=(
    find "$MD_HOME" -xdev \( -uid "$old_uid" -o -gid "$old_gid" \) -exec chown -h "$MD_USER:$MD_GROUP" {} +
  )
  if command -v nice >/dev/null 2>&1; then
    repair=(nice -n 19 "${repair[@]}")
  fi
  if ! "${repair[@]}"; then
    log "WARNING: background $MD_HOME ownership repair failed"
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

# join_device_group GID NAME grants $MD_USER access to a device owned by GID,
# matching the group that owns the host device. A root-owned device (GID 0, as
# rootless runtimes map it) offers no group to join.
join_device_group() {
  local host_gid="$1"
  local name="$2"
  local current_gid existing
  if [ "$host_gid" = 0 ]; then
    log "device group $name: device is root-owned, leaving groups unchanged"
    return
  fi
  current_gid="$(getent group "$name" | awk -F: '{print $3}' || true)"
  if [ "$current_gid" = "$host_gid" ]; then
    usermod -aG "$name" "$MD_USER" || fail "cannot add $MD_USER to group $name"
    return
  fi
  existing="$(getent group "$host_gid" | awk -F: '{print $1}' || true)"
  if [ -n "$existing" ]; then
    usermod -aG "$existing" "$MD_USER" || fail "cannot add $MD_USER to group $existing"
    return
  fi
  if [ -n "$current_gid" ]; then
    groupmod --gid "$host_gid" "$name" || fail "cannot move group $name to GID $host_gid"
  else
    groupadd --gid "$host_gid" "$name" || fail "cannot create group $name with GID $host_gid"
  fi
  usermod -aG "$name" "$MD_USER" || fail "cannot add $MD_USER to group $name"
}

# start_dbus wires a persistent DBus session for the account. DBus is optional:
# a base image without it still serves SSH, it only loses the session bus.
start_dbus() {
  local session_file="$MD_HOME/.dbus-session-env"
  if ! command -v dbus-launch >/dev/null 2>&1 || ! command -v su >/dev/null 2>&1; then
    log "Skipping DBus: dbus-launch or su is not installed"
    remove_environment_var DBUS_SESSION_BUS_ADDRESS
    return
  fi
  if [ -x /etc/init.d/dbus ]; then
    log "Starting dbus service..."
    /etc/init.d/dbus start || log "WARNING: dbus service failed to start, continuing"
  fi
  log "Setting up persistent DBus session for user..."
  rm -f "$session_file" /etc/profile.d/50-dbus-session.sh
  if su "$MD_USER" -s /bin/bash -c "env -u DBUS_SESSION_BUS_ADDRESS dbus-launch --sh-syntax" >"$session_file"; then
    chown "$MD_USER:$MD_GROUP" "$session_file"
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
    log "WARNING: DBus session setup failed, continuing without DBUS_SESSION_BUS_ADDRESS"
    rm -f "$session_file"
    remove_environment_var DBUS_SESSION_BUS_ADDRESS
  fi
}

write_tailscale_device_id() {
  local ts_id
  ts_id="$(tailscale status --json 2>/dev/null | jq -r '.Self.ID // empty' || true)"
  if [ -n "$ts_id" ]; then
    echo "$ts_id" >/var/lib/md/tailscale_device_id
  fi
}

start_tailscale() {
  log "Starting Tailscale..."
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
    tailscale set --operator="$MD_USER"
    write_tailscale_device_id
    # Update MOTD with Tailscale FQDN and VNC URL if display is enabled
    local ts_fqdn
    ts_fqdn="$(tailscale status --json | jq -r '.Self.DNSName // empty' || true)"
    ts_fqdn="${ts_fqdn%.}"
    if [ -n "$ts_fqdn" ]; then
      echo "Connected to $ts_fqdn" >/etc/motd
      if [ -n "${MD_DISPLAY:-}" ]; then
        echo "VNC: vnc://$ts_fqdn:5901" >>/etc/motd
      fi
      log "Tailscale connected: $ts_fqdn"
    fi
  else
    # Redirect stdout+stderr to the well-known file. tailscale up --json
    # flushes each JSON line immediately, so the file is readable as soon
    # as inotify fires on the first write.
    (
      tailscale up --hostname="$(hostname)" --ssh --json >/run/md/tailscale_auth_url.json 2>&1
      tailscale set --operator="$MD_USER"
      write_tailscale_device_id
    ) &
  fi
}

start_sshd() {
  rm -f /run/sshd.pid /var/run/sshd.pid
  rm -rf /run/sshd
  install -d -m 0755 /run/sshd
  chown "$MD_USER:$MD_GROUP" "$MD_HOME" "$MD_HOME/.ssh" "$MD_HOME/.ssh/authorized_keys"
  chmod 0700 "$MD_HOME" "$MD_HOME/.ssh"
  chmod 0400 "$MD_HOME/.ssh/authorized_keys"
  find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*_key' -exec chown root:root {} + -exec chmod 0600 {} +
  find /etc/ssh -maxdepth 1 -type f -name 'ssh_host_*_key.pub' -exec chown root:root {} + -exec chmod 0644 {} +
  # Prefer the image's init script; fall back to running sshd itself, which is
  # all the daemon needs.
  if [ -x /etc/init.d/ssh ] && command -v service >/dev/null 2>&1 && service ssh start; then
    return
  fi
  if ! "$SSHD_BIN"; then
    log "ERROR: sshd failed to start:"
    "$SSHD_BIN" -t -e || true
    exit 1
  fi
}

# The specialized image build runs this script with --check so that a base image
# which cannot carry md fails its own build, naming what is missing, before any
# container exists. Runtime starts use no argument.
case "${1:-}" in
"")
  ;;
--check)
  preflight
  log "image satisfies the md startup contract"
  exit 0
  ;;
*)
  fail "unexpected argument '$1'"
  ;;
esac

preflight
ensure_account
ensure_directories

# Generate dynamic motd with hostname
echo "Connected to $(hostname)" >/etc/motd

configure_host_user_id

# If /dev/kvm exists, update the kvm group GID to match the host.
# In rootless Docker, device GIDs map to the overflow GID (65534) and groupmod
# would fail because that GID is already taken by nogroup; join_device_group
# skips the root-owned case for the same reason.
if [ -e /dev/kvm ]; then
  join_device_group "$(stat -c %g -- /dev/kvm)" kvm
fi

# Rootless container runtime detection: if UID 0 inside the container maps to a
# non-root host UID, add "user" to the container's root group so it can access
# root-owned paths. This applies to both Docker's default rootless mapping and
# Podman's keep-id mapping; the account itself must remain non-root.
if awk '$1 == 0 && $2 != 0 { found=1 } END { exit !found }' /proc/self/uid_map &&
  [ "$(id -u "$MD_USER")" != "0" ]; then
  usermod -aG root "$MD_USER"
fi

start_dbus

# Start XFCE4 and VNC. Dependencies were validated by preflight.
if [ -n "${MD_DISPLAY:-}" ]; then
  # Start Xvnc + XFCE with monitors (runs as root, unkillable by user)
  /root/vnc-start.sh
else
  log "MD_DISPLAY not set, skipping X/VNC startup"
fi

# Start Tailscale if enabled. Dependencies were validated by preflight.
if [ -n "${MD_TAILSCALE:-}" ]; then
  start_tailscale
fi

# If /dev/bus/usb exists, grant access through the group that owns the devices.
if [ -d /dev/bus/usb ]; then
  host_plugdev_gid="$(stat -c %g /dev/bus/usb/001/* 2>/dev/null | grep -v '^0$' | head -1 || true)"
  if [ -n "$host_plugdev_gid" ]; then
    join_device_group "$host_plugdev_gid" plugdev
  elif getent group plugdev >/dev/null 2>&1; then
    # Every device is root-owned right now; keep the image's default membership
    # so devices attached after startup stay reachable.
    usermod -aG plugdev "$MD_USER"
  fi
fi

# If USB serial adapters are mapped, grant access through the group that owns
# the device nodes. USB serial adapters normally use the host's dialout GID.
host_dialout_gid=
for serial_device in /dev/ttyACM* /dev/ttyUSB*; do
  if [ -c "$serial_device" ]; then
    host_dialout_gid="$(stat -c %g -- "$serial_device")"
    break
  fi
done
if [ -n "$host_dialout_gid" ]; then
  join_device_group "$host_dialout_gid" dialout
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
  usermod -aG sudo "$MD_USER"
  echo "$MD_USER:$MD_SUDO_PASSWORD" | chpasswd
  unset MD_SUDO_PASSWORD

  if findmnt -no OPTIONS /proc | grep -q nosuid; then
    mount -o remount,rw,suid /proc 2>/dev/null || true
  fi
  if findmnt -no OPTIONS /proc | grep -q nosuid; then
    log "WARNING: /proc remount failed (nosuid still set) — rootless Podman may not work"
  else
    log "Remounted /proc without nosuid for rootless Podman"
  fi
  # Unmount all submounts under /proc (deepest first) so the kernel
  # sees /proc as fully visible for nested user namespaces.
  findmnt -nl -o TARGET --submounts /proc | sort -r | while read -r p; do
    [ "$p" = "/proc" ] && continue
    umount "$p" 2>/dev/null || true
  done
  log "Unmasked Docker /proc paths for rootless Podman"
else
  if id -nG "$MD_USER" | tr ' ' '\n' | grep -qx sudo; then
    deluser "$MD_USER" sudo >/dev/null 2>&1 || true
  fi
  if id -nG "$MD_USER" | tr ' ' '\n' | grep -qx sudo; then
    log "ERROR: $MD_USER remains in the sudo group"
    exit 1
  fi
  passwd -l "$MD_USER" >/dev/null
fi

# Start SSH server (after VNC so DISPLAY is available)
start_sshd
start_home_ownership_repair

sleep infinity
