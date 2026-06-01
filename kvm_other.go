// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build !linux

// KVM stub for non-Linux platforms.

package md

// kvmAvailable reports whether /dev/kvm is present and writable.
// KVM is Linux-only.
func kvmAvailable() bool {
	return false
}
