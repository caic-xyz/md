# Rootless Podman: keep-id, commit, and mount ownership

This explains a non-obvious ownership interaction under rootless podman and why
`Fork` re-chowns repositories. It exists because the behavior cost hours to
pin down and is easy to "fix" wrongly.

## The default rootless mapping

Rootless podman runs in a user namespace where the host user (e.g. UID 1001)
maps to container UID 0. So a bind-mounted host directory owned by the host
user appears **root-owned** inside the container, and the unprivileged `user`
account cannot write it unless its UID maps to the host UID.

## Why md uses `--userns=keep-id`

`md` bind-mounts the host user's real config directories into the container
(`AgentMounts`: `~/.claude`, `~/.config/*`, ...). The agent runs as `user` and
must read **and write** them, while the host must keep ownership (the user edits
those same dirs outside the container). At launch, md passes the host UID/GID to
`start.sh`, which moves the image's `user` account to those IDs. For rootless
podman, `--userns=keep-id` then maps that UID/GID back to the host user, so
mounts stay host-owned and are writable by `user` with no chown of the host
tree. `--user 0:0` keeps `start.sh` running as root for privileged setup (sshd,
groupmod, dbus).

## The cost: `podman commit` does not round-trip keep-id ownership

`Fork` snapshots the source container with `podman commit`, then runs the fork
from that image. Under keep-id this **collapses `user`-owned files to root**:
files the source container owned as the host-mapped `user` UID come back owned
by UID 0 in the fork.

Observed directly (podman 5.4.2): a file created as the host-mapped container
user in a keep-id container is recorded in the committed image as UID **0**
(confirmed by mounting the snapshot image outside any user namespace).
`buildah`'s commit path does reverse the container's ID mapping (`copier.Get`
with the container `UIDMap`, `image.go`), but that reversal does not round-trip
for keep-id: the mapping places the host user at the namespace's outer-0 slot,
and the user-owned files land back at 0.

Consequence for `Fork`: the pushed repositories (created as `user`) become
root-owned in the fork, which breaks git with "detected dubious ownership" and,
past that, leaves the repos unwritable by the SSH `user` during
`git-receive-pack`. It is not limited to the repos — every file the source
container wrote as `user` (anywhere the commit captured it) collapses the same
way, so a forked agent could hit root-owned paths outside `~/src` too. Docker
preserves ownership on commit, so this is rootless-podman-only.

`start.sh`'s "add `user` to the root group" path does not help here: git checks
the owning UID (not group), and the repo dirs are not group-writable.

## Why this is a fundamental tension, not a misconfiguration

The two desirable properties are in direct conflict under rootless podman:

| userns mode                | mounts host-owned + writable by `user` | `commit` round-trips ownership |
|----------------------------|:--:|:--:|
| default (no userns)        | no (mounts appear root-owned)          | **yes** (user UID preserved) |
| `--userns=keep-id`         | **yes**                                | no (user UID -> 0) |
| `--userns=keep-id:uid=...` | yes                                    | no (user UID -> 0) |
| `--userns=auto`            | no                                     | no (user UID shifted) |

For a host-owned mount to be writable by a non-root container user, the host
user must map to that non-root container UID (keep-id does this). But placing
the host user at that slot is exactly what breaks `commit`'s reverse-map. You
cannot have both.

### `:idmap` does not bridge it (rootless)

Idmapped bind mounts (`-v host:ctr:idmap`) can map ownership into the namespace
without chowning the host, and would let md use the default userns (which
round-trips `commit`). Tested on kernel 6.8 / crun 1.21, it does not work for
this case:

- **Plain `:idmap`** works but presents the host-owned dir as container **root**
  (UID 0), not as `user` — so the agent still cannot write it.
- **Custom `:idmap`** to present the dir as the container `user` UID is rejected
  rootless: mapping a backing UID to a host subuid fails with
  `uid_map: Operation not permitted`, and partial maps fail with
  `mount_setattr: Invalid argument`. Rootless podman/crun will not build the
  required namespace.

## What md does

- Keep `--userns=keep-id` for the mount contract.
- Move the image's `user` account to the host UID/GID at startup using
  `MD_HOST_UID`/`MD_HOST_GID`, then repair files already in `/home/user` from
  the image UID/GID to the new UID/GID without crossing bind mounts.
- In `Fork`, after the fork's SSH is up and before pushing branches, restore
  every collapsed file in the home back to `user`
  (`find /home/user -xdev -uid 0 -exec chown user:user {} +`), gated on
  `IsRootless()`. Whole-home rather than just `~/src`, because the collapse is
  not limited to the repos. This is safe: a fresh home has no legitimately
  root-owned files (verified), so `-uid 0` only ever matches collapse
  artifacts. `-xdev` keeps the walk on the container's own filesystem, so it
  never descends into a bind-mounted host directory (which keep-id presents as
  user-owned anyway) — host ownership is never rewritten, the same reason `:U`
  was rejected. The walk is cheap (~0.2s over ~180k inodes).
- `smoke_test.go`'s `mounts` subtest guards the keep-id invariant (a writable,
  host-owned bind mount and a read-only one), so a future attempt to drop
  keep-id will fail observably rather than silently regress mounts.

Docker does not remap ownership on commit; rootless Docker is handled inside
`start.sh` via `/proc/self/uid_map` detection.
