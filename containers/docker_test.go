// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import "testing"

func TestDocker(t *testing.T) {
	t.Parallel()
	t.Run("IsRootless", func(t *testing.T) {
		t.Parallel()
		if newDocker("docker", nil, nil).IsRootless() {
			t.Error("Docker.IsRootless() = true, want false")
		}
	})
}
