# Run md on Conforming Base Images

`md start --image` and `md run --image` accept any conforming Linux base image, produce the same SSH-managed development environment on Docker and Podman, and reject unsupported images before a persistent container exists. md owns the container lifecycle, so the base image's `CMD`, `ENTRYPOINT`, and default `USER` do not control startup. The specialized build enforces the startup contract before a container exists (see Container Startup Capabilities in [AGENTS.md](../AGENTS.md)), and the `foreign_base` group in `smoke_test.go` exercises it.

## Phase 1 — capability-gate: Reject impossible requests before creating a container

The specialized build records which optional capabilities the base image satisfies, and `start`, `run`, and `warmup` compare that record with the requested options, failing with the missing capability and the requesting option named before a container exists.

- **Scope:** Specialized image construction, request validation for `start`, `run`, and `warmup`, and CLI documentation.
- **Preserve:** Docker and Podman parity, the platform and feature validation performed before container creation, runtime startup remains the last line of defense, and the fixed `user` UID/GID 1000 contract described in [ROOTLESS.md](ROOTLESS.md).
- **Verify:** A requested capability the image does not satisfy (`-display` without Xvnc, `-tailscale` without tailscaled or `/dev/net/tun`, `-sudo` without sudo) is rejected before container creation naming the capability and the option, while the same options still start on an image that satisfies them.

## Later

- Consider opt-in build-time package installation only if users need nonconforming minimal images enough to justify network-dependent specialized builds.
- Reuse the contract for a future VM backend, with VM-specific bootstrap and cache delivery planned separately.
- Surface the container log when a container exits after launch: `launchContainer` and `Revive` report a stopped container's state and log tail, the SSH wait still only times out.
- Add a fixture with sshd but no git to assert the git-specific build failure in `foreign_base`.
