// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"os"
	"runtime"
	"testing"
)

func TestPodman(t *testing.T) {
	t.Parallel()
	t.Run("IsRootless", func(t *testing.T) {
		t.Parallel()
		got := newPodman("podman", nil, nil).IsRootless()
		if runtime.GOOS == "linux" {
			want := os.Getuid() != 0
			if got != want {
				t.Errorf("Podman.IsRootless() = %v, want %v (uid=%d)", got, want, os.Getuid())
			}
		} else if got {
			t.Error("Podman.IsRootless() = true on non-Linux, want false")
		}
	})
}
