#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Monitor XFCE session, restart if it dies
# Runs as root - unkillable by user

set -eu

DISPLAY=":1"
LOGFILE="/var/log/display-server.log"

log() {
	echo "[xfce-monitor] $*" | tee -a "$LOGFILE"
}

start_xfce() {
	su - user -c "DISPLAY=$DISPLAY startxfce4" </dev/null &
	for _ in $(seq 1 50); do
		pid=$(pgrep -u user -x xfce4-session) && {
			echo "$pid"
			return
		}
		sleep 0.2
	done
	log "xfce4-session did not start within 10s"
	return 1
}

while true; do
	pid=$(pgrep -u user -x xfce4-session || start_xfce)
	log "Watching XFCE (pid $pid)"
	tail --pid="$pid" -f /dev/null 2>/dev/null || true
	log "XFCE died"
done
