// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Tests for runtime-specific container identity and ownership.

package md

import (
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/md/containers"
)

func TestUserIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, runtime, hostOS string
		info                  containers.Info
		uid, gid              int
		owner                 string
		keepID                bool
	}{
		{name: "mac_desktop", runtime: "docker", hostOS: "darwin", info: containers.Info{DockerDesktop: true}, uid: 501, gid: 20, owner: "1000:1000"},
		{name: "windows_desktop", runtime: "docker", hostOS: "windows", info: containers.Info{DockerDesktop: true}, uid: -1, gid: -1, owner: "1000:1000"},
		{name: "linux_docker", runtime: "docker", hostOS: "linux", uid: 1001, gid: 1002, owner: "1001:1002"},
		{name: "mac_remote_linux", runtime: "docker", hostOS: "darwin", uid: 501, gid: 20, owner: "501:20"},
		{name: "linux_desktop", runtime: "docker", hostOS: "linux", info: containers.Info{DockerDesktop: true}, uid: 1001, gid: 1002, owner: "1001:1002"},
		{name: "rootless_podman", runtime: "podman", hostOS: "linux", info: containers.Info{Rootless: true}, uid: 1001, gid: 1002, owner: "1000:1000", keepID: true},
		{name: "remote_rootless_podman", runtime: "podman", hostOS: "darwin", info: containers.Info{Rootless: true}, uid: 501, gid: 20, owner: "1000:1000", keepID: true},
		{name: "rootful_podman", runtime: "podman", hostOS: "linux", uid: 1001, gid: 1002, owner: "1001:1002"},
		{name: "root_host", runtime: "docker", hostOS: "linux", owner: "1000:1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := resolveUserIdentity(tc.runtime, tc.hostOS, tc.info, tc.uid, tc.gid)
			if got := u.owner(); got != tc.owner {
				t.Errorf("image owner = %s, want %s", got, tc.owner)
			}
			args := u.runArgs()
			// Image ownership and launch identity must agree even when host IDs differ.
			parts := strings.Split(tc.owner, ":")
			want := []string{"-e", "MD_HOST_UID=" + parts[0], "-e", "MD_HOST_GID=" + parts[1]}
			if tc.keepID {
				want = append(want, "--userns=keep-id:uid=1000,gid=1000", "--user", "0:0")
			}
			if !slices.Equal(args, want) {
				t.Errorf("launch arguments = %v, want %v", args, want)
			}
		})
	}
}
