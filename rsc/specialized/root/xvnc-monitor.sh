#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Monitor Xvnc, restart if it dies
# Runs as root - unkillable by user

set -eu

DISPLAY=":1"
LOGFILE="/var/log/display-server.log"

log() {
	echo "[xvnc-monitor] $*" | tee -a "$LOGFILE"
}

start_xvnc() {
	rm -f /tmp/.X1-lock /tmp/.X11-unix/X1 2>/dev/null || true
	Xvnc "$DISPLAY" -geometry 1920x1080 -depth 24 -SecurityTypes None -rfbport 5901 &
	echo $!
}

while true; do
	pid=$(pgrep -x Xvnc || start_xvnc)
	log "Watching Xvnc (pid $pid)"
	tail --pid="$pid" -f /dev/null 2>/dev/null || true
	log "Xvnc died"
done
