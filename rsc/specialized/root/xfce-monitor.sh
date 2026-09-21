#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Monitor XFCE session, restart if it dies
# Runs as root - unkillable by user

set -euo pipefail

MD_USER=user

DISPLAY=":1"
LOGFILE="/var/log/display-server.log"

log() {
  echo "[xfce-monitor] $*" | tee -a "$LOGFILE"
}

# start_xfce starts a session and prints its pid once xfce4-session is visible.
start_xfce() {
  su - "$MD_USER" -c "DISPLAY=$DISPLAY startxfce4" </dev/null &
  local pid
  for _ in $(seq 1 50); do
    pid="$(pgrep -u "$MD_USER" -x xfce4-session | head -n 1 || true)"
    if [ -n "$pid" ]; then
      printf '%s\n' "$pid"
      return 0
    fi
    sleep 0.2
  done
  return 1
}

while true; do
  pid="$(pgrep -u "$MD_USER" -x xfce4-session | head -n 1 || true)"
  if [ -z "$pid" ] && ! pid="$(start_xfce)"; then
    log "ERROR: xfce4-session did not start within 10s"
    exit 1
  fi
  log "Watching XFCE (pid $pid)"
  tail --pid="$pid" -f /dev/null 2>/dev/null || true
  log "XFCE died"
done
