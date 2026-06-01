// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build !linux

// Rootless podman stub for non-Linux platforms.
package md

// isRootlessPodman reports whether we are running under rootless podman.
// On non-Linux platforms podman runs rootful inside its VM, so no fix is needed.
func isRootlessPodman(_ string) bool { return false }
