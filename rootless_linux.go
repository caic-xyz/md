// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Rootless podman detection for Linux.
package md

import "os"

// isRootlessPodman reports whether we are running under rootless podman.
//
// In rootless podman on Linux, the default user namespace maps host UID 1000 →
// container UID 0, so bind-mounted host directories appear root-owned inside
// the container. Callers use this to inject --userns=keep-id so the host UID
// maps to the same UID inside the container.
func isRootlessPodman(rt string) bool {
	return rt == "podman" && os.Getuid() != 0
}
