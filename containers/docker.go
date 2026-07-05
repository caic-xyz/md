// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"context"
	"slices"
)

// docker wraps the docker CLI.
type docker struct {
	base
}

// newDocker returns a Docker runtime wrapper.
func newDocker(name string, logger Logger, env []string) *docker {
	if name == "" {
		name = "docker"
	}
	return &docker{base: base{name: name, logger: logger, env: slices.Clone(env)}}
}

// UntagImage removes an image tag without deleting containers that use it.
func (d *docker) UntagImage(ctx context.Context, image string) error {
	_, err := d.Run(ctx, "", "rmi", "-f", "--no-prune", image)
	return err
}
