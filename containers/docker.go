// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"context"
	"log/slog"
)

// docker wraps the docker CLI.
type docker struct {
	base
}

// newDocker returns a Docker runtime wrapper.
func newDocker(executable string, logger *slog.Logger, env []string) *docker {
	if executable == "" {
		executable = "docker"
	}
	return &docker{base: newBase(executable, logger, env)}
}

// UntagImage removes an image tag without deleting containers that use it.
func (d *docker) UntagImage(ctx context.Context, image string) error {
	_, err := d.Run(ctx, "", "rmi", "-f", "--no-prune", image)
	return err
}
