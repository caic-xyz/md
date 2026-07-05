// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"context"
	"os"
	"runtime"
	"slices"
)

// podman wraps the podman CLI.
type podman struct {
	base
}

// newPodman returns a Podman runtime wrapper.
func newPodman(name string, logger Logger, env []string) *podman {
	if name == "" {
		name = "podman"
	}
	return &podman{base: base{name: name, logger: logger, env: slices.Clone(env)}}
}

// UntagImage removes an image tag without deleting containers that use it.
func (p *podman) UntagImage(ctx context.Context, image string) error {
	_, err := p.Run(ctx, "", "image", "untag", image)
	return err
}

// IsRootless reports whether Podman is running rootless.
//
// In rootless Podman on Linux, the default user namespace maps host UID 1000 to
// container UID 0, so bind-mounted host directories appear root-owned inside
// the container. Callers use this to inject --userns=keep-id so the host UID
// maps to the same UID inside the container.
func (p *podman) IsRootless() bool {
	return isRootlessPodman()
}

func isRootlessPodman() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return os.Getuid() != 0
}
