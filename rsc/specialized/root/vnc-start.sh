#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Start Xvnc and XFCE - runs synchronously during container startup.
# start.sh validated Xvnc and startxfce4 before calling this script.

set -euo pipefail

MD_USER=user

DISPLAY=":1"
LOGFILE="/var/log/display-server.log"
DISPLAY_FILE="/etc/profile.d/60-vnc-display.sh"

log() {
  echo "[vnc-start] $*" | tee -a "$LOGFILE"
}

# write_environment_var replaces KEY= in /etc/environment so that restarting the
# container does not accumulate stale entries. /etc/profile.d is only sourced by
# login shells; /etc/environment is read by PAM's pam_env.so for every SSH
# session regardless of login status.
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

# Clean up any stale X locks/sockets
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1 2>/dev/null || true

# Prepare log file
: >"$LOGFILE"
chmod 666 "$LOGFILE"

# Start Xvnc
log "Starting Xvnc on $DISPLAY (port 5901)..."
Xvnc "$DISPLAY" -geometry 1920x1080 -depth 24 -SecurityTypes None -rfbport 5901 &
xvnc_pid=$!
# Wait for the X socket to appear instead of a fixed sleep, and fail clearly
# when Xvnc dies instead of leaving the container half-started.
for _ in $(seq 1 100); do
  if ! kill -0 "$xvnc_pid" 2>/dev/null; then
    log "ERROR: Xvnc exited during startup:"
    tail -n 40 "$LOGFILE" >&2
    exit 1
  fi
  if [ -e /tmp/.X11-unix/X1 ]; then
    break
  fi
  sleep 0.1
done
if [ ! -e /tmp/.X11-unix/X1 ]; then
  log "ERROR: Xvnc did not create /tmp/.X11-unix/X1 within 10s"
  exit 1
fi

# Write DISPLAY to profile.d
log "Writing DISPLAY=$DISPLAY to $DISPLAY_FILE"
{
  echo "# Display - set by container startup"
  echo "export DISPLAY=$DISPLAY"
} >"$DISPLAY_FILE"
chmod 644 "$DISPLAY_FILE"
write_environment_var DISPLAY "$DISPLAY"

# Start XFCE
log "Starting XFCE session as user..."
su - "$MD_USER" -c "DISPLAY=$DISPLAY startxfce4" </dev/null &

log "VNC startup complete, starting monitors"
/root/xvnc-monitor.sh &
/root/xfce-monitor.sh &
