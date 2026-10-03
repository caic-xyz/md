// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

// Container user identity for image ownership and runtime namespace mapping.

package md

import (
	"fmt"
	"strconv"

	"github.com/caic-xyz/md/containers"
)

// userIdentity is the numeric account md owns inside the container. keepID
// maps the host user to that account through Podman's user namespace.
type userIdentity struct {
	uid, gid int
	keepID   bool
}

func resolveUserIdentity(runtimeName, hostOS string, info containers.Info, uid, gid int) userIdentity {
	u := userIdentity{uid: containerUserUID, gid: containerUserGID}
	if runtimeName == "podman" && info.Rootless {
		// The default rootless namespace maps the host user to container root,
		// leaving host-owned bind mounts unwritable by user. keep-id maps the
		// host user to the image's fixed account without changing host ownership
		// or recursively copying up the large image home to chown it.
		//
		// keep-id ownership does not round-trip through podman commit, so Fork
		// must repair ownership in the snapshot. See docs/ROOTLESS.md.
		u.keepID = true
		return u
	}
	// Desktop translates ownership for macOS/Windows file sharing. Matching
	// those client IDs inside its Linux VM is unnecessary and can collide with
	// system accounts (macOS's staff GID 20 is Debian's dialout). A non-Linux
	// client talking to a native Linux daemon must still match its host IDs.
	if info.DockerDesktop && (hostOS == "darwin" || hostOS == "windows") {
		return u
	}
	if uid > 0 && gid > 0 {
		u.uid, u.gid = uid, gid
	}
	return u
}

func (u userIdentity) owner() string {
	return fmt.Sprintf("%d:%d", u.uid, u.gid)
}

func (u userIdentity) runArgs() []string {
	args := []string{"-e", "MD_HOST_UID=" + strconv.Itoa(u.uid), "-e", "MD_HOST_GID=" + strconv.Itoa(u.gid)}
	if u.keepID {
		// keep-id otherwise selects the mapped user as the initial process;
		// start.sh needs root for privileged setup such as starting sshd.
		args = append(args, "--userns=keep-id:uid=1000,gid=1000", "--user", "0:0")
	}
	return args
}
