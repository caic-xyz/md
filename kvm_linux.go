// Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// KVM availability detection for Linux.
package md

import "syscall"

// kvmAvailable reports whether /dev/kvm is present and writable.
func kvmAvailable() bool {
	return syscall.Access("/dev/kvm", 2 /* W_OK */) == nil
}
