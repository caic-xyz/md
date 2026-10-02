# Run md on Conforming Base Images

`md start --image` and `md run --image` accept any conforming Linux base image, including one whose development account has a different name, UID/GID, or home directory. They produce the same SSH-managed development environment on Docker and Podman and reject unsupported images before a persistent container exists. md owns the container lifecycle, so the base image's `CMD`, `ENTRYPOINT`, and default `USER` do not control startup. The specialized build enforces the startup contract and installs current coding agents before a container exists (see Container Startup Capabilities in [AGENTS.md](../AGENTS.md)); a base image must provide curl, su, and network access during the build. The `foreign_base` group in `smoke_test.go` exercises this contract.

## Phase 1 — account-identity: Preserve the base image's development account

The specialized build and container startup use the selected development account's name, UID/GID, primary group, and home directory. When the base has no suitable account, md creates its default `user` account with UID/GID 1000.

- **Scope:** Account selection, specialized image construction, rootless Podman keep-id mapping, cache and fork ownership, SSH paths, coding-agent installation, container startup paths, and the account assumptions in [ROOTLESS.md](ROOTLESS.md).
- **Preserve:** Existing account identity and home directory; the default `user` UID/GID 1000 behavior for bases without a suitable account.
- **Verify:** A foreign base with an account named `developer`, UID 1500, GID 1600, and home `/home/developer` builds, starts, accepts SSH, and runs installed coding agents under that identity on Docker and rootless Podman; a base without a suitable account still provisions `user` and works on both runtimes.

## Phase 2 — capability-gate: Reject impossible requests before creating a container

The specialized build records which optional capabilities the base image satisfies, and `start`, `run`, and `warmup` compare that record with the requested options, failing with the missing capability and the requesting option named before a container exists.

- **Scope:** Specialized image construction, request validation for `start`, `run`, and `warmup`, and CLI documentation.
- **Preserve:** Docker and Podman parity, the platform and feature validation performed before container creation, and runtime startup as the last line of defense.
- **Verify:** A requested capability the image does not satisfy (`-display` without Xvnc, `-tailscale` without tailscaled or `/dev/net/tun`, `-sudo` without sudo) is rejected before container creation naming the capability and the option, while the same options still start on an image that satisfies them.

## Later

- Reuse the contract for a future VM backend, with VM-specific bootstrap and cache delivery planned separately.
- Surface the container log when a container exits after launch: `launchContainer` and `Revive` report a stopped container's state and log tail, the SSH wait still only times out.
- Add a fixture with sshd but no git to assert the git-specific build failure in `foreign_base`.
