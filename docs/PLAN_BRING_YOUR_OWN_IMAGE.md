# Run md on Conforming Base Images

`md start --image` and `md run --image` accept any conforming Linux base image, produce the same SSH-managed development environment on Docker and Podman, and reject unsupported images before leaving a persistent container. md owns the container lifecycle, so the base image's `CMD`, `ENTRYPOINT`, and default `USER` do not control startup.

## Phase 1 — portable-bootstrap: Make startup capability-aware

- **Scope:** The specialized startup scripts and their behavior tests.
- **Preserve:** The bundled image starts exactly as it does today. Unrequested optional subsystems may be absent; a requested subsystem with missing dependencies fails clearly. Rootless Podman keeps the fixed `user` UID/GID 1000 contract described in [ROOTLESS.md](ROOTLESS.md).
- **Verify:** The bundled image still reaches SSH with every supported option. A reduced image reaches SSH when omitted capabilities were not requested, fails with the missing capability and requesting option when they were requested, and provisions the `user` account and required directories idempotently when UID/GID 1000 are available.

## Phase 2 — image-contract: Publish and enforce the base-image contract

- **Depends on:** portable-bootstrap
- **Scope:** Specialized-image construction, preflight validation for `start`, `run`, and `warmup`, CLI documentation, and a durable base-image contract under `docs/`.
- **Preserve:** Validation covers the requested platform and features before persistent container creation. md explicitly neutralizes inherited `CMD`, `ENTRYPOINT`, and non-root `USER` metadata; does not run a base image's application entrypoint; does not install packages during the per-user specialized build; and retains named-context cache injection, image freshness checks, Docker support, and Podman support.
- **Verify:** A matrix covering the bundled image, a conforming Debian/Ubuntu-family image, an image with inherited `ENTRYPOINT` and non-root `USER`, and images missing each mandatory or requested capability proves deterministic startup or actionable preflight rejection on Docker and Podman for `linux/amd64` and `linux/arm64` where the host can execute the platform.

## Later

- Consider opt-in build-time package installation only if users need nonconforming minimal images enough to justify network-dependent specialized builds.
- Reuse the contract for a future VM backend, with VM-specific bootstrap and cache delivery planned separately.
