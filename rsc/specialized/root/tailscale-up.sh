#!/bin/bash
# Copyright 2026 Marc-Antoine Ruel. All rights reserved.
# Use of this source code is governed under the Apache License, Version 2.0
# that can be found in the LICENSE file.

# Log tailscaled in with the pre-authorized key in TAILSCALE_AUTHKEY.
# start.sh runs it on a container's first start; md Revive runs it when the
# tailnet removed the ephemeral node while the container was stopped.

set -euo pipefail

MD_USER=user

if [ -z "${TAILSCALE_AUTHKEY:-}" ]; then
  echo "[tailscale-up] ERROR: TAILSCALE_AUTHKEY is empty" >&2
  exit 1
fi
tailscale up --hostname="$(hostname)" --ssh --authkey="$TAILSCALE_AUTHKEY"
# Allow non-root users to access tailscale CLI (must be after tailscale up)
tailscale set --operator="$MD_USER"
# Update MOTD with Tailscale FQDN and VNC URL if display is enabled
ts_fqdn="$(tailscale status --json | jq -r '.Self.DNSName // empty' || true)"
ts_fqdn="${ts_fqdn%.}"
if [ -n "$ts_fqdn" ]; then
  echo "Connected to $ts_fqdn" >/etc/motd
  if [ -n "${MD_DISPLAY:-}" ]; then
    echo "VNC: vnc://$ts_fqdn:5901" >>/etc/motd
  fi
  echo "[tailscale-up] Tailscale connected: $ts_fqdn"
fi
