# Agent development guide

A file to [guide coding agents](https://agents.md/).

## Requirements

- Make sure the code passes shellcheck after every change. Then format with `shfmt -l -w $script_name`
- Update this file (AGENTS.md) everytime you make a change that affects this project's requirements.
- Update rsc/user/home/user/src/AGENTS.md everytime you make a change that affects the agent inside the container.
- **Glob/find tools may skip dotfiles by default.** The `rsc/` tree contains important config under dot-directories (e.g. `rsc/user/home/user/.config/git/config`). Use Grep or explicit dot-inclusive patterns when searching for files under `rsc/`.
- When adding a new setup script in `rsc/root/root/setup/`, add a corresponding `RUN` command to `rsc/root/Dockerfile`. When adding a new setup script in `rsc/user/home/user/setup/`, add a corresponding `RUN` command to `rsc/user/Dockerfile`.
- No tests should be written for Python or shell script changes.
- **NEVER run `go build ./cmd/md/` without `-o`** — the repo root contains a Python script named `md` and `go build` will overwrite it. Always use `go build -o /tmp/md-test ./cmd/md/` or similar.
- For Go code changes, ensure code passes `go test ./...`, `go vet ./...`, and `golangci-lint run ./...`.
- **Cross-platform paths**: When passing host paths to Docker CLI or SSH config files, always use `filepath.ToSlash()`.
  Docker Desktop on Windows expects forward slashes; SSH config uses POSIX convention.
- For Python code changes, ensure code passes `pylint` and `ruff` checks as defined in `.github/workflows/docker-build-user.yml`
- When adding new tools to the system, they must also be added to `rsc/user/home/user/setup/generate_version_report.sh` to ensure they appear in version reports. The script generates `/home/user/src/tool_versions.md` which is used in release notes and build reports

## Smoke Tests

Smoke tests verify end-to-end image build, container launch, cache injection, and nested podman inside a sudo-enabled container. They are gated behind the `smoke` build tag:

```bash
# Fast: skip image builds, use pre-existing or remote images
go test -tags=smoke -run TestSmoke -short -v -timeout 30m

# Full: build images if needed, including clean rebuild test
go test -tags=smoke -run TestSmoke -v -timeout 30m
```

The test requires a container runtime (docker or podman) in PATH. Nested podman subtests are skipped under rootless podman due to user namespace stacking (`newuidmap` fails with `EPERM`).

## md Tool: Image Build and Cache Injection

### Image hierarchy

- **`md-root-local`** — root image built locally from `rsc/root/Dockerfile` via `md build-image` (first step).
- **`md-user-local`** — user image built locally from `rsc/user/Dockerfile` on top of `md-root-local` via `md build-image` (second step). Used as base when `-image md-user-local` is passed.
- **`ghcr.io/caic-xyz/md-root:latest`** — remote root image with system packages. Rebuilt infrequently (when root setup scripts change). Built by `docker-build-root.yml`.
- **`ghcr.io/caic-xyz/md-user:latest`** (default) or any `-image`/`-tag` variant — remote user image with Go, Node, Rust, etc. Rebuilt weekly. Built by `docker-build-user.yml` on top of `md-root`.
- **`md-specialized-<hash>`** — specialized per-user image built on top of the chosen base via a generated Dockerfile + `docker build`. A Dockerfile is created at runtime with `COPY --chown` for SSH keys and `COPY --from=<named-context> --chown` for cache directories, then built with `--no-cache --pull=never --build-context cache-<name>=<hostpath>`. This approach was chosen over `docker create`/`cp`/`commit` (slower: `docker cp` uses API round-trips vs COPY's storage-driver-level tar streaming, and requires starting the container for permission fixes) and over a static Dockerfile (cannot adapt to dynamic cache sets). Built automatically by `md start` and `md run` when needed. The image name includes a 32-hex-char hash of (base image, active cache key) so that different base images or cache sets get distinct images without clobbering each other. Computed by `userImageName()` in `docker.go`.

### When the user image is rebuilt

`imageBuildNeeded` (`docker.go`) returns `true` (triggering a rebuild) when any of the following change:
1. `md.base_digest` label missing/empty, or differs from the current base image digest.
2. For remote base images: registry has a newer version than the local copy.
3. `md.context_sha` label differs from the SHA of the SSH keys.
4. `md.cache_key` label differs from `cacheSpecKey` of the **active** caches (those whose host directories currently exist). The key includes the cache name, resolved host path, container path, read-only flag, and shallow flag.

### Cache injection

`md start` and `md run` bake host cache directories into the user image at build time via `COPY --from=<name>` in the Dockerfile. This avoids slow cold-start downloads inside the container.

**Default behaviour**: all `WellKnownCaches` entries are included. Caches whose host directory does not exist are silently skipped (no rebuild triggered for missing dirs).

**CLI flags** (on both `md start` and `md run`):
- `-no-cache <name>` — exclude a specific well-known cache (repeatable).
- `-no-caches` — disable all default caches; use `-cache` to add back specific ones.
- `-cache <spec>` — add a well-known name (re-adds when used with `-no-caches`) or a custom `host:container[:ro]` path.

**Well-known cache names** (defined in `WellKnownCaches`, `client.go`): android-keys, bun, cargo, go-mod, gradle, maven, npm, pip, pnpm, uv.

**Shallow caches**: setting `Shallow: true` on a `CacheMount` copies only top-level files from the host directory, ignoring subdirectories. This is useful for directories like `~/.android` where only a few files (debug.keystore, adbkey) are needed but subdirectories (avd/, cache/) are large and unwanted. The generated Dockerfile emits one `COPY` per file instead of `COPY . <dest>/`. If no top-level files exist, the cache is skipped.

**Adding a new well-known cache**: add an entry to `WellKnownCaches` in `client.go`. No other changes needed — it is automatically picked up by `resolveCaches` and the flag help text.

### Key labels on user image

| Label | Value |
|---|---|
| `md.base_image` | Base image reference used at build time |
| `md.base_digest` | Digest (or image ID for local images) of the base |
| `md.context_sha` | SHA-256 of SSH keys |
| `md.cache_key` | 8-byte hex hash of the **active** (injected) cache names, resolved host paths, container paths, read-only flags, and shallow flags |
| `md.base_manifest_digest` | Per-platform manifest digest from the registry (remote bases only) |

## Adding a New Tool Checklist

When installing a new tool in the container, ensure you update:

1. Create setup script in `rsc/root/root/setup/` or `rsc/user/home/user/setup/` (with appropriate numbering)
2. Add `RUN measure_exec.sh` command to `rsc/root/Dockerfile` or `rsc/user/Dockerfile` accordingly
3. Add version check to `rsc/user/home/user/setup/generate_version_report.sh`
4. Update `rsc/user/home/user/src/AGENTS.md` "Preinstalled Tools" section to reflect the change
5. If the tool needs PATH setup, update `rsc/user/home/user/.config/bash.d/80-path.sh` (see [Shell Environment](#shell-environment-bash_env))
6. Run `shellcheck` and `shfmt` on any shell scripts

## Shell Environment (BASH_ENV)

The container uses `BASH_ENV=/etc/bash_env` to ensure PATH and environment variables are available in **all** bash invocations — interactive, non-interactive, login, and non-login. This solves the classic problem where `ssh host command` runs a non-interactive non-login shell that skips `.bashrc`'s interactive guard.

### How it works

1. **`/etc/bash_env`** — sourced by bash for non-interactive shells via the `BASH_ENV` env var (set in Dockerfile). It sources all `~/.config/bash.d/*.sh` scripts. Has a double-source guard.
2. **`/etc/profile.d/00-bash-env.sh`** — sources `/etc/bash_env` for login Bash shells such as `bash -lc`, which some agents use for tool commands.
3. **`/etc/bash.bashrc`** — system-wide bashrc, sources `/etc/bash_env` before the interactive guard. No patching of `/etc/skel/.bashrc` is needed.
4. **`~/.config/bash.d/*.sh`** — modular scripts for PATH and environment:
   - `10-env.sh` — sources `~/.env` and `~/.config/md/env` (API keys, etc.)
   - `20-android.sh` — Android SDK paths
   - `30-nvm.sh` — nvm-managed node PATH (loads nvm function only in interactive shells)
   - `80-path.sh` — common PATH entries and shell defaults
   - `90-git.sh` — git completions and prompt (self-guards for interactive only)
   - `95-shell.sh` — interactive aliases

### Adding a tool that modifies PATH

When installing a tool whose installer appends PATH lines to `.bashrc` (like nvm, bun, opencode):

1. Add the tool's bin directory to `~/.config/bash.d/80-path.sh`
2. If the installer supports `PROFILE=/dev/null` (like nvm), use it to prevent writing to `.bashrc`
3. Otherwise, add a cleanup pattern to `rsc/user/home/user/setup/bashrc_cleanup.sh` to remove the appended lines
4. Interactive-only features (completions, shell functions) should be guarded with `case $- in *i*) ... ;; esac`

## Chrome/Chromium Configuration

Initial preferences are configured via:
- `/opt/google/chrome/initial_preferences` - Chrome
- `/etc/chromium/initial_preferences` - Chromium

Reference for all available preference names. The file is large so first download it and then take a look:
https://chromium.googlesource.com/chromium/src/+/refs/heads/main/chrome/common/pref_names.h?format=TEXT

## Runtime Requirements

- **Chrome Sandbox**: To run Chrome/Chromium with the sandbox enabled, the container must be launched with `--security-opt seccomp=unconfined` and `--security-opt apparmor=unconfined`. The `md` script handles this automatically.
- **Debugging Tools**: strace requires `--cap-add=SYS_PTRACE`. The `md` script handles this automatically.
- **Tailscale**: Requires `--cap-add=NET_ADMIN` and `--cap-add=NET_RAW`. The host's `/dev/net/tun` is passed through via `--device=/dev/net/tun:/dev/net/tun` instead of creating it inside the container with `mknod` (which would require the `MKNOD` capability — a security liability). Requires the host to have the `tun` kernel module loaded. The `md` script handles this automatically when `--tailscale` is passed to `md start`.
- **USB Passthrough**: Requires `--device=/dev/bus/usb` to expose host USB devices (e.g. for ADB). The `md` script handles this automatically when `-usb` is passed to `md start`.
- **Nested Containers (rootless Podman inside md)**: Supported on **rootful Docker/Podman hosts** with `kernel.unprivileged_userns_clone=1` (default on most modern distros). Requires `-sudo` (`md start -sudo`) to grant `SYS_ADMIN` and `/dev/fuse`; `/dev/net/tun` is passed through when `-sudo` or `-tailscale` is set. `start.sh` remounts `/proc` without `nosuid` and unmounts Docker's tmpfs masks on `/proc` so the kernel allows mounting a new `/proc` inside nested user namespaces. Rootless Docker/Podman hosts are not supported: `newuidmap` fails with EPERM because the container itself already runs inside a user namespace, and `start.sh` logs a warning at startup. See: https://www.redhat.com/sysadmin/podman-inside-container, https://github.com/containers/podman/discussions/28307, https://github.com/containers/podman/issues/4131.

## For End Users: Remote GUI Access

The container runs Xvnc (TigerVNC) + XFCE4 on port 5901 accessible via any VNC client. Xvnc runs as root (unkillable by user), while the XFCE session runs as user:
- **Xvnc** (root): Combined X server + VNC server on :1, port 5901
- **XFCE4** (user): Desktop session, auto-restarts if killed

## Directory Layout (rsc/)

The `rsc/` directory is split into three build contexts, one per image layer:

- `rsc/root/` — Build context for `md-root` (root image with system packages)
  - `rsc/root/Dockerfile` - Root image build file (FROM debian:stable)
  - `rsc/root/etc/`, `rsc/root/opt/` - System config files mirrored into the container
    - `rsc/root/etc/bash_env` - Environment bootstrap sourced by BASH_ENV (see Shell Environment below)
    - `rsc/root/etc/bash.bashrc` - System-wide bashrc, sources bash_env for interactive shells
    - `rsc/root/etc/profile.d/00-bash-env.sh` - Login Bash hook for the shared environment
  - `rsc/root/root/` - Root-context setup and utilities
    - `rsc/root/root/setup/` - Root-level installation scripts (numbered 1+)
    - `rsc/root/root/start.sh` - Container entrypoint
  - `rsc/root/usr/` - Custom executables (measure_exec.sh)
- `rsc/user/` — Build context for `md-user` (user image with Go, Node, Rust, etc.)
  - `rsc/user/Dockerfile` - User image build file (FROM md-root)
  - `rsc/user/home/user/` - User-context setup (copied as user to `/home/user/`)
    - `rsc/user/home/user/.config/bash.d/` - Modular bash extensions sourced via `/etc/bash_env` (see Shell Environment below)
    - `rsc/user/home/user/setup/` - User-level installation scripts (numbered 1+)
    - `rsc/user/home/user/src/AGENTS.md` - Agent documentation inside container (keep in sync)

<!-- BEGIN FILE INDEX -->
## File Index

Autogenerated from first-line comments. Run scripts/update_agents_file_index.py to refresh.

- `.github/scripts/cleanup_docker_images.py`: Cleans up old Docker images from GitHub Container Registry.
- `.github/scripts/merge_tool_versions.py`: Merges tool version reports from amd64 and arm64 architectures.
- `.github/workflows/cleanup-docker-images.yml`: Cleans up old Docker images from GitHub Container Registry.
- `.github/workflows/docker-build-dispatch.yml`: Dispatch image builds from triggering events.
- `.github/workflows/docker-build-root.yml`: Build the root base image on schedule.
- `.github/workflows/docker-build-user.yml`: Build the user base image on schedule.
- `.github/workflows/release.yml`: Release workflow for goreleaser and GitHub releases.
- `.github/workflows/test.yml`: Run tests, linters, and smoke tests on push.
- `.golangci.yml`: Static analysis linter rules and enabled checks for the Go codebase.
- `.goreleaser.yml`: GoReleaser configuration for building multi-platform binaries.
- `README.md`: md: My Development containers
- `client.go`: Package md manages isolated Docker development containers for AI coding
- `client_test.go`: Tests for client.go
- `cmd/md/main.go`: Package main implements the md CLI for isolated Docker development containers.
- `cmd/md/main_test.go`: Tests for the md CLI tool.
- `container.go`: Container lifecycle and configuration types.
- `container_test.go`: Tests for container types and lifecycle.
- `docker.go`: Docker operations and image building.
- `docker_test.go`: Tests for Docker operations.
- `docs/NETWORKING.md`: Container Outbound Network Restrictions
- `gitutil/commitmsg.go`: Commit message generation and formatting.
- `gitutil/commitmsg_test.go`: Tests for commit message generation.
- `gitutil/git.go`: Package gitutil provides git utility functions for repository introspection,
- `gitutil/git_test.go`: Tests for git repository utilities.
- `kvm_linux.go`: KVM availability detection for Linux.
- `kvm_other.go`: KVM stub for non-Linux platforms.
- `rootless_linux.go`: Rootless podman detection for Linux.
- `rootless_other.go`: Rootless podman stub for non-Linux platforms.
- `rsc/root/Dockerfile`: syntax=docker/dockerfile:1.6
- `rsc/root/etc/profile.d/00-bash-env.sh`: Source md's shared Bash environment for login shells.
- `rsc/root/root/setup/1_packages.sh`: Install core system packages (runs as root).
- `rsc/root/root/setup/2_neovim.sh`: Install the latest stable Neovim build and wire common aliases.
- `rsc/root/root/setup/3_extrepo.sh`: Install extrepo packages: Google Chrome (amd64 only), GitHub CLI, Tailscale.
- `rsc/root/root/setup/4_create_user.sh`: Create the unprivileged user account.
- `rsc/root/root/setup/5_kvm.sh`: Make sure kvm is accessible to user inside the container.
- `rsc/root/root/setup/6_radare2.sh`: Install the latest radare2 release from GitHub.
- `rsc/root/root/setup/7_podman.sh`: Configure Podman for rootless container-in-container support.
- `rsc/root/root/start.sh`: Intentionally fail-fast: any startup failure should be visible immediately
- `rsc/root/root/vnc-start.sh`: Start Xvnc and XFCE - runs synchronously during container startup
- `rsc/root/root/xfce-monitor.sh`: Monitor XFCE session, restart if it dies
- `rsc/root/root/xvnc-monitor.sh`: Monitor Xvnc, restart if it dies
- `rsc/root/usr/local/bin/measure_exec.sh`: Wrapper script to measure execution time of a command and log it to a markdown table.
- `rsc/user/Dockerfile`: syntax=docker/dockerfile:1.6
- `rsc/user/home/user/.config/agents/skills/md-container-environment/SKILL.md`: Development environment and tool integration guide for the md container.
- `rsc/user/home/user/.config/bash.d/10-env.sh`: User environment files (API keys).
- `rsc/user/home/user/.config/bash.d/20-android.sh`: Android SDK paths.
- `rsc/user/home/user/.config/bash.d/30-nvm.sh`: NVM (Node Version Manager) paths.
- `rsc/user/home/user/.config/bash.d/80-path.sh`: Common PATH entries and shell defaults.
- `rsc/user/home/user/.config/bash.d/90-git.sh`: Git completion and prompt helpers.
- `rsc/user/home/user/.config/bash.d/95-shell.sh`: Interactive shell aliases.
- `rsc/user/home/user/setup/1_go.sh`: Install the latest Go toolchain reported by go.dev and set up go tools.
- `rsc/user/home/user/setup/2_nodejs.sh`: Install nvm, node.js, npm, typescript, eslint, MCP servers, and global LLM packages (as user)
- `rsc/user/home/user/setup/3_bun.sh`: Install Bun (as user)
- `rsc/user/home/user/setup/4_android.sh`: Install Android SDK tools, emulator, and system images on x64 (runs as user).
- `rsc/user/home/user/setup/5_rust.sh`: Install the latest Rust toolchain via rustup.
- `rsc/user/home/user/setup/6_python.sh`: Install Python development tools (runs as user)
- `rsc/user/home/user/setup/7_llm_tools.sh`: Install Standalone LLM Tools: OpenCode, Amp, Claude (as user)
- `rsc/user/home/user/setup/bashrc_cleanup.sh`: Remove installer-appended lines from .bashrc.
- `rsc/user/home/user/setup/generate_version_report.sh`: Generate version report for installed tools
- `rsc/user/home/user/src/AGENTS.md`: Environment
- `scripts/lint_binaries.py`: Lint for unexpected binary or executable files in the repository.
- `scripts/update_agents_file_index.py`: Update AGENTS.md files (containing a file index marker) with an auto-generated index.
- `smoke_test.go`: End-to-end container lifecycle smoke tests.
- `ssh.go`: SSH key generation and configuration.
- `ssh_test.go`: Tests for ssh.go
- `tailscale.go`: Tailscale authentication and networking.
- `tailscale_test.go`: Tests for tailscale.go
<!-- END FILE INDEX -->
