# Decoupling start.sh from the base image

Goal: make it possible to run `md start -image <anything>` against a base image md did not
build, instead of requiring the `ghcr.io/caic-xyz/md-*` images. Today `start.sh` and the
whole runtime contract are baked into `md-root`; this document plans the decoupling and
records the pitfalls and trade-offs found while investigating.

## Motivation — this is really about a runtime-agnostic contract

"Bring your own image" is the near-term framing, but the load-bearing reason to decouple
`start.sh` from the base is to **let md target runtimes other than Docker/Podman later — virtual
machines (QEMU as one example, not exclusively: cloud-hypervisor, Firecracker, a cloud VM,
etc.).** A VM is not a container, and that difference is what should color the solution choices:

- **No container injection primitives.** A VM has no `docker exec`, no `docker cp`, no image
  layers, no `COPY --from` build context. Every Layer A delivery mechanism that leans on those
  (exec injection, cp-before-start, COPY-into-specialized-image) and the entire BuildKit
  **cache-injection** model are Docker-only. They do not port to a VM.
- **A VM boots a real init.** You don't "override `CMD`"; the kernel starts systemd/openrc.
  Provisioning happens via cloud-init / ignition / a seed disk / first-boot SSH — i.e. *an init
  the system already runs a hook for*. This **inverts** the container trade-off: systemd's
  weight is a liability in a container but a given in a VM.
- **Much of `start.sh` is container-specific.** The `/proc` remount + unmask for nested rootless
  podman, the `uid_map` detection + `usermod -aG root`, kvm-GID matching, `--userns=keep-id` —
  these exist only because of container constraints. In a VM with a real kernel, real `/proc`,
  real root, they are moot and must self-skip, not error.

The common denominator across containers *and* VMs is exactly two things md already has or can
define: **SSH as the connection model**, and **a provisioning contract the target satisfies**.
So the VM goal pushes the design toward: (1) treat the **contract (Layer B2)** as the durable,
runtime-agnostic abstraction — the thing a container image *or* a VM disk image conforms to;
(2) prefer delivery via *an init the target runs* plus a transport the target supports (SSH, a
seed disk), treating Docker-build-time injection as a container-only fast path, not the core
mechanism; (3) keep `start.sh` robust and capability-guarded (Layer B3) so the *same* script
runs unmodified in a VM, where the container-only blocks simply don't trigger. See
"Implications of the VM target" before the recommendations for how this re-weights each choice.

## Current coupling (where we are)

- `start.sh` reaches the image via `COPY root/ /root/` in `rsc/root/Dockerfile`, and three
  Dockerfiles carry `CMD ["/root/start.sh"]` (`rsc/root/Dockerfile`, `rsc/user/Dockerfile`,
  and the generated specialized Dockerfile in `generateDockerfile`, `client.go`).
- The `rsc/` tree is embedded in the binary (`//go:embed all:rsc`, `docker.go`), so the script
  bytes are already available host-side at runtime — this is the lever for moving them.
- md connects by SSH as `user` to a published `127.0.0.1::22` (`launchContainer`,
  `container.go`; `ssh.go` hardcodes `User user`). `start.sh` is also pid-1 keep-alive
  (`service ssh start` then `sleep infinity`).

The problem splits into three layers of increasing difficulty. Treat "any image" as **"any
conforming glibc Debian/Ubuntu-family bash image"** — musl/alpine and distroless break the
Go/Rust/node tooling and the `service`/`/etc/init.d` assumptions regardless of this work.

---

## Layer A — script delivery (mechanical, low risk)

Stop relying on the base to carry the scripts. Extract `start.sh`, `vnc-start.sh`,
`xfce-monitor.sh`, `xvnc-monitor.sh` from the embedded FS into the specialized build context
and `COPY` them into the specialized image, setting `CMD` there. Remove the `COPY root/` +
`CMD` reliance from the base.

Tasks:
- [ ] In `generateDockerfile`, emit `COPY` for the four scripts and `CMD ["/root/start.sh"]`
      (already emits the CMD; add the COPYs + ship the files in the build context dir).
- [ ] In `buildSpecializedImage`, write the four scripts into `tmpDir` alongside the SSH keys.
- [ ] Update `docker_test.go` assertions (currently pins `CMD ["/root/start.sh"]` and the four
      scripts being executable in the embedded FS).

Trade-offs:
- COPY-into-specialized: a `start.sh` edit now invalidates every specialized image instead of
  the rarely-rebuilt base. Acceptable.
- Alternative — bind-mount (`-v <extracted>:/root/<script>:ro` + `--entrypoint`): zero rebuild
  on script change, but the extracted temp dir must outlive the container, not just the run
  command, and Windows/Docker-Desktop path translation applies (`filepath.ToSlash`). More
  moving parts; prefer COPY unless rebuild latency proves painful.

This removes the script from the base. It does **not** deliver "any image" on its own.

### A' — how the script is delivered and how pid 1 stays alive

Layer A is really one axis ("get the script bytes into a runnable place") crossed with a
second, independent axis: **what runs as pid 1**. The current design fuses them — `start.sh`
*is* both the baked script and pid 1 (it ends in `sleep infinity`). Splitting them opens
several startup mechanisms. The user-raised `docker exec` idea lives here.

The pivotal constraint: **reviving a stopped container is `docker start` (`Container.Revive`,
`container.go`),
which only re-runs the image `CMD`.** Anything injected after run (exec/cp-after-start) is in
the writable layer or process tree, not the `CMD`, so it must be re-applied on every `md start`,
not just at create. So evaluate each mechanism on *two* events: first boot and revive.

| Mechanism | Script in image? | pid 1 | First boot | Revive (`docker start`) | Honors foreign CMD? |
|---|---|---|---|---|---|
| **Baked CMD** (today) | yes (COPY) | start.sh | CMD runs | CMD re-runs ✓ | no |
| **A: COPY into specialized** | yes (COPY, not base) | start.sh | CMD runs | CMD re-runs ✓ | no |
| **cp-before-start** | no | start.sh | `create` w/ cmd `bash /tmp/start.sh`, `cp` script, `start` | file persists in layer, CMD re-runs ✓ | no |
| **exec, imperative** | no | `sleep infinity` (override) | run keep-alive, then `exec` setup | keep-alive re-runs but setup is **gone** — must re-`exec` | no (unless image is long-lived) |
| **exec, self-installing** | no | init / generic stub | run init, `exec` setup that registers itself | init re-runs the registered unit ✓ | depends on init |
| **exec onto foreign CMD** | no | image's own CMD | run image, then `exec` setup | image CMD re-runs, setup **gone** — re-`exec` | **yes** |

#### docker exec injection — the trade-offs

Concretely: `docker run -d <img> sleep infinity` (or keep the image's own long-lived CMD),
then `docker exec -i -u 0 <name> bash -s -- <args> < start.sh`. The script is piped over stdin,
never entering the image or a host bind mount. `start.sh` must be split so the setup half
returns (sshd backgrounded) instead of ending in `sleep infinity` — pid 1 is now the keep-alive.

Upsides:
- **Directly serves "any image"** — delivers the script without `COPY`, with no host temp-file
  lifetime or Windows path-translation concern (stdin pipe, not a mount).
- **Better failure visibility** — a setup failure is a synchronous non-zero exec exit code
  captured in `md start` output, instead of a died container you diagnose via `docker logs`.
  This actually improves on `start.sh`'s "fail-fast" intent.
- **Secrets off the persistent container** — `MD_SUDO_PASSWORD` can pass via `docker exec -e`
  instead of `docker run -e`, keeping it out of `docker inspect`'s env (it's still in the
  `md.sudo-password` label today, but this removes one copy).
- **Privileges intact** — caps are granted at run (`--cap-add`), so the exec'd setup can still
  remount `/proc`, `groupmod`, etc.

Downsides / pitfalls:
- **Revive re-injection — mandatory only for the *imperative* variant.** `docker start` replays
  the image `CMD`, not the exec. If the exec'd setup is imperative and ephemeral, md must re-run
  it after every `docker start` (`Container.Revive`); miss it and a revived container has no sshd.
  This is removable — see "self-installing" below — but it is the default failure mode if you
  do the naive thing.
- **Keep-alive pid 1 still required** → still replaces the foreign `CMD` (Layer C unchanged),
  unless you trust the image's own process to stay alive (the "exec onto foreign CMD" row,
  which *is* the only mechanism that honors a foreign entrypoint — at the cost of betting the
  image never exits and is exec-able).
- **Two-phase orchestration in Go**: run → wait running → exec setup → wait sshd. More steps
  than the current single `docker run` + readiness wait.
- **Delivers the script, not its dependencies.** Exec injection is an alternative to Layer A
  only. The image still must satisfy the Layer B contract (sshd, `user`, bash, …). Orthogonal.

#### Self-installing — removing the revive-reinjection requirement

The re-injection burden is not inherent to exec; it is a property of doing imperative setup that
lives only in the process tree. Make the *first* injection **register a persistent boot hook**
that the container's own pid 1 replays on `docker start`, and revive costs nothing. What plays
pid 1 is a choice with a spectrum of how much we depend on it — systemd is **one** option, and
not necessarily the one we want to force. Ordered from least to most dependency:

1. **Generic pid-1 stub (no new dependency).** You cannot change an existing container's `CMD`,
   but you *choose* it at `docker create` (as run args, not baked in the image). Set it to a
   tiny image-independent loop, e.g. `bash -c 'for f in /etc/md/init.d/*; do . "$f"; done; exec
   sleep infinity'`. The first exec/cp drops `start.sh` into `/etc/md/init.d/`; on revive the
   stub (pid 1, fixed at create) re-runs it. No extra packages, no extra caps, only the `bash`
   the Layer B contract already assumes. Cost: you own a small bespoke init and keep the
   hand-rolled `xfce-monitor.sh`/`xvnc-monitor.sh` supervision.

2. **Image's own entrypoint drop-in (no new dependency, image-dependent).** Some base images
   already run a hook directory on boot (`/docker-entrypoint.d`, `/etc/cont-init.d`). If present,
   drop the bootstrap there and inherit the image's restart behaviour. Free where it exists, but
   not universal — can't be relied on for "any image".

3. **Container-native supervisor (small dependency).** A lightweight init/supervisor as pid 1 —
   `s6-overlay`, `runit`, `dumb-init`/`tini`+a supervisor, or `supervisord` — gives real
   service supervision and restart-on-revive without systemd's weight. Adds one small package to
   the contract; replaces the monitor scripts; no special caps or cgroup mounts. The pragmatic
   middle if we want supervision but not systemd.

4. **systemd as pid 1 (heavy dependency + posture).** Set the container command to `/sbin/init`,
   drop `md-bootstrap.service` (`Type=oneshot`) + sshd/dbus units, `systemctl enable` them;
   revive replays enabled units and `Restart=always` retires the monitor scripts entirely. The
   most "standard" supervision, but the strongest coupling:
   - **Base contract (Layer B):** the image must ship systemd and be bootable as init.
     `debian:stable`, `golang`, `node`, ubuntu-minimal, alpine do **not** by default — excluding
     most off-the-shelf images unless md installs+configures systemd at specialized-build time
     (back to B1's cost).
   - **Runtime posture:** needs a properly mounted `/sys/fs/cgroup`, cgroup namespace, writable
     `/run` tmpfs, and typically `CAP_SYS_ADMIN` (granted today only under `-sudo`). Docker
     needs explicit mounts/caps (or `--privileged`); podman has `--systemd=always`. New
     Docker-vs-podman divergence.

Guidance: default to **(1)** — it removes re-injection within the existing contract and keeps
"any image" honest. Reach for **(3)** if we decide supervision is worth one small dependency.
Treat **(4)** as available, not assumed: pick it only if standard-systemd integration is an
explicit goal, since it is the one option that materially narrows which images can be used.

#### cp-before-start — the quiet middle ground

`docker create <img> bash /tmp/start.sh` → `docker cp <hosttmp>/start.sh <name>:/tmp/start.sh`
→ `docker start`. Here **pid 1 is start.sh again**, so revive semantics are identical to today
(CMD re-runs, file persists in the writable layer) — no re-injection logic needed — *and* the
script never enters the image. CLAUDE.md flags `docker cp` as slower than COPY because the cost
is per-API-round-trip; that matters for multi-GB cache trees, but one small script is one cheap
call. The only cross-
platform note is `filepath.ToSlash` on the host source path. This is arguably the lowest-risk
way to get the script out of the base while preserving every current behavior.

**Recommendation for Layer A:** prefer **cp-before-start** if the only goal is decoupling the
script while keeping today's restart model (pid 1 stays start.sh, revive is free, no new
contract). Reach for **exec injection** when you want the failure-visibility / secret-handling
wins; pair it with a self-installing pid 1 so revive stays free without re-injection — default
to the **generic stub (option 1)**, which adds no dependency. A **container-native supervisor
(option 3)** or **systemd (option 4)** are alternatives if supervision is worth a dependency,
but systemd is the only one that narrows which images work, so don't force it. Use **exec onto
foreign CMD** only if honoring a foreign entrypoint (Layer C) becomes a hard requirement.

---

## Layer B — the runtime contract start.sh assumes (hard)

`start.sh` is glue over a fat Debian base. It needs the base to already provide:

| Need | Source today | Breaks on | Mandatory? |
|---|---|---|---|
| `bash`, root default user | — | distroless, non-root USER | **core** |
| `sshd`, `service ssh`, `sshd_config.d/md.conf` | root pkgs | alpine/distroless/minimal | **core** |
| `user` acct, UID 1000, `/home/user/.ssh` | `4_create_user.sh` | image lacking it; UID 1000 collision | **core** (provisionable) |
| `getent`, `usermod`/`groupmod`/`useradd`/`chpasswd` | root pkgs | busybox/musl | core-ish (needed to provision the above) |
| `dbus`, `/etc/init.d/dbus`, `dbus-launch` | root pkgs | non-Debian, no sysvinit | degradable (GUI session bus only) |
| Xvnc/XFCE (with `--display`) | root pkgs | missing | degradable (only if `--display`) |
| `tailscaled`/`tailscale` (with `--tailscale`) | `3_extrepo.sh` | missing | degradable (only if `--tailscale`) |
| `jq`, `findmnt` (util-linux) | root pkgs | busybox/musl | degradable (used by optional paths) |
| `BASH_ENV=/etc/bash_env` + `bash.d/*` | root config | unset on foreign image | degradable (PATH convenience) |

The right column is the lever: most of the contract is **optional**. Three approaches, which
compose — B3 shrinks the surface that B1 must install or B2 must validate:

**B1 — inject at specialized-build time.** Add `RUN` steps to the generated Dockerfile to
`useradd`, install `openssh-server`+`dbus`, drop `md.conf`, set `BASH_ENV`. Pitfalls: needs a
package manager (apt vs apk vs none) and network during the *per-user* build; today that build
is deliberately fast and uses `--pull=never`. It re-runs on every cache/base change. This
pushes base-build cost into specialized-build, reintroducing the cold-start latency the cache
injection design exists to avoid. Make it opt-in behind a flag if implemented at all.

**B2 — define a contract and validate it (recommended).** Document the required surface; probe
it at `md start` with a fast pre-flight; fail with a clear message otherwise. Cheap and honest.
"Any image" becomes "any conforming image."

Tasks (B2):
- [ ] Write the contract: bash, an sshd reachable on 22, `user`@UID 1000 with writable
      `/home/user`, glibc, root-capable first boot.
- [ ] Add a pre-flight probe: inspect the image (`docker run --rm <img> sh -c '...'`) for
      `getent passwd user`, `command -v sshd bash`, UID 1000. Fail fast with remediation text.
- [ ] Probe *requested* optional deps too and fail before container creation: `--display` ⇒
      Xvnc/XFCE present, `--tailscale` ⇒ tailscaled present. Failing at probe time is even
      cleaner than start.sh's in-container fail (no container to clean up).

**B3 — make `start.sh` degrade and self-provision (shrink the contract).** Rather than demand a
fat base, make the script tolerant of a thin one: skip what's optional, create what's missing
but mandatory. This is the highest-leverage approach because it *reduces* the contract B2 probes
and the packages B1 would install — ideally down to just `bash` + `sshd` + root-at-boot. Two
moves:

- **Detect-and-degrade for optional subsystems.** Gate each on capability + intent:
  `have() { command -v "$1" >/dev/null; }`. dbus, X/VNC, tailscale, kvm-GID match, USB/plugdev,
  `/proc` remount, `BASH_ENV` — wrap each. Crucial distinction: **silent** skip when the feature
  wasn't requested (no `MD_DISPLAY`); **hard fail** (non-zero exit) when it *was* requested but
  the dep is absent (`MD_DISPLAY=1` but no Xvnc). This matches `start.sh`'s existing
  "Intentionally fail-fast" stance: a warning the user has to notice in log noise is worse than
  an explicit failure at `md start`. So "degrade" means *only* "skip features the user didn't
  ask for" — a base without X still gives a working SSH dev box; asking for `--display` on that
  base errors out, it does not silently come up GUI-less.
- **Detect-and-provision for the mandatory few.** The `user` account and `/home/user`,
  `/home/user/.ssh`, `/run/md` dirs can be created at boot if absent (`getent passwd user ||
  useradd -m -u 1000 -s /bin/bash user`). Must be idempotent (re-runs on every revive) and pin
  UID 1000 so injected caches keep matching ownership. `sshd` itself is **not** boot-provisioned
  — installing a package on every boot is too slow and needs network; that stays a build-time
  (B1) or contract (B2) concern.

Tasks (B3):
- [ ] Add a `have()` guard and wrap dbus, tailscale, kvm-GID, USB, sudo/`proc` blocks; replace
      bare `usermod`/`groupmod`/`findmnt`/`jq` calls with guarded ones.
- [ ] Per optional block: "not requested" → silent skip; "requested but unavailable" → exit
      non-zero with a clear message naming the missing dep and the flag that needs it.
- [ ] Add idempotent user/dir provisioning at the top of `start.sh`, gated on `getent passwd user`.
- [ ] Expand smoke tests to a base matrix: full image, no-dbus, no-X, no-`user` (see pitfalls).

Pitfalls specific to B3:
- **Build-time COPY vs runtime user creation (chicken-and-egg).** The specialized Dockerfile
  does `COPY --chown=user:user authorized_keys /home/user/.ssh/...` (`generateDockerfile`,
  `client.go`). That `--chown` fails if `user` doesn't exist at *build* time. So either (a)
  provision the user in a specialized-build `RUN useradd` *before* the COPY (build-time, not
  start.sh), or (b) COPY `authorized_keys` to a root-owned staging path and have start.sh create
  the user and move/chown it at boot. (a) keeps a stable identity and the COPY semantics; (b)
  is purer "any image" but adds a runtime placement step. Note this is the same tension that
  makes build-time provisioning often cleaner than start.sh provisioning.
- **UID collision.** If the foreign image already uses UID 1000 for a different account,
  `useradd -u 1000` fails and cache ownership (chowned to 1000 at build) is wrong. Detect and
  fail loudly, or pick/echo a UID and feed it back into the chown — but a dynamic UID undermines
  the prebuilt-cache model. Easiest honest answer: require UID 1000 free, validate in B2.
- **Username is hardcoded `user`.** md assumes `user` across `ssh.go` (`User user`) and git
  remotes (`user@<name>:`). B3 force-creates `user` even if the base ships its own `node`/
  `ubuntu`/`vscode` account. Adapting to the image's existing user instead would touch `ssh.go`,
  remote URLs, and every `chown user:user` — larger change, deferred. Note it as a Layer-C-
  adjacent option.
- **Complexity / silent-failure surface.** More branches mean more to test and more paths that
  can mask real breakage. The fail-on-request discipline above is what keeps degradation safe:
  silent-skip is allowed *only* for features the user didn't request. Without that rule B3 would
  trade clear "missing dep" errors for confusing half-working containers.

---

## Layer C — the SSH model itself (architectural ceiling)

md *is* "SSH into a long-lived container as `user`": publishes `127.0.0.1::22`, hardcodes
`User user`, every git remote is `user@<name>:...`, and `start.sh` supplies both sshd and the
`sleep infinity` keep-alive. A foreign image's own `CMD` would run instead, likely exit
immediately, and carry no sshd.

Consequence: md **always** replaces the base image's `CMD`. You cannot honor a foreign
entrypoint *and* keep md's connection model. Document this as an invariant rather than trying
to solve it. If honoring foreign entrypoints ever matters, it requires a sidecar/bootstrap that
supplies keep-alive + sshd independently of the base — out of scope here.

---

## Cross-cutting pitfalls

- **UID/ownership**: the generated Dockerfile `chown user:user`s caches and dirs; rootless
  podman uses `--user 0:0 --userns=keep-id`. Both assume `user`/1000 exists on the base. A
  foreign image with UID 1000 already taken, or no `user`, breaks cache injection silently.
- **Debian-isms in start.sh**: `service ssh start`, `/etc/init.d/dbus`, apt layout. Porting to
  non-Debian means rewriting these, not just installing packages.
- **Privileged first boot**: `groupmod` (kvm/plugdev GID match), `/proc` remount (nested
  podman), `chpasswd` (sudo) all need root + caps even if the image's default USER is non-root.
- **Tests pin the layout**: `docker_test.go` asserts the `CMD` line and the four executable
  scripts. Any move updates these.

---

## Implications of the VM target (re-weighting the choices)

Holding the runtime-agnostic goal next to the per-layer analysis above shifts the verdicts:

- **Layer A — the "best" container mechanism is a dead end for VMs.** cp-before-start and exec
  injection were attractive *because* they exploit Docker primitives; none exist in a VM. The
  portable shape is "the target's init runs a hook that runs `start.sh`." For containers that
  hook is the generic pid-1 stub or `CMD`; for VMs it is a systemd unit / cloud-init. So pick a
  container mechanism for its container merits, but know it is a leaf, not the trunk — the trunk
  is the init-run hook. This *raises* the standing of the **self-installing-via-init** family
  (previously down-weighted for containers) because it is the one shape common to both worlds.
- **Layer B — the contract is the trunk.** B2 stops being merely "recommended" and becomes the
  abstraction that makes a container image and a VM image interchangeable. Write it
  runtime-neutrally (bash, sshd, `user`@1000, an init that runs our bootstrap, root-at-first-
  boot) and avoid Docker-specific phrasing.
- **Layer B3 — guards are what make one `start.sh` run everywhere.** The container-only blocks
  (`/proc` unmask, `uid_map`/`usermod -aG root`, kvm-GID, `--userns=keep-id`) must be gated on
  *the condition that makes them necessary*, not just tool presence — so they self-skip in a VM
  rather than firing pointlessly. This is a stronger reason to do B3 than the container story
  alone gave.
- **Cache injection is Docker-only and needs a parallel VM path.** `COPY --from=cache-*` /
  `buildSpecializedImage` / `userImageName()` do not translate. A VM seeds caches via a mounted
  disk, virtiofs/9p, rsync-over-SSH, or baking into the disk image. Out of scope here, but the
  contract should not assume the container cache mechanism.
- **B1 (install at build) is the least portable.** It is inherently a container-image-build
  step. A VM equivalent is "build the disk image" — a different pipeline entirely. Keep B1 an
  opt-in container fast path, not a load-bearing dependency.
- **Pre-flight probe needs a transport-neutral form.** `docker run --rm <img>` to probe the
  contract is container-only; for a VM the equivalent is checking the image/manifest or probing
  over the first SSH connection. Same contract, different inspection transport.

## Recommended sequencing

Ordered so the runtime-agnostic trunk (the contract + an init-run hook + a portable `start.sh`)
lands first, and container-only conveniences stay leaves:

1. **Layer A** — get the script out of the base. Default to **cp-before-start** (keeps today's
   restart model, lowest risk); choose **exec injection** instead if you want the failure-
   visibility / secret wins and will own revive re-injection. See A' for the comparison. Treat
   the chosen mechanism as a container leaf over the init-run hook, which is the VM-portable
   shape (see "Implications of the VM target").
2. **Layer B3** — make `start.sh` degrade-and-self-provision, gating container-only blocks on
   the condition that makes them necessary so the same script runs in a VM. Do this *before* B2,
   because it shrinks the contract B2 has to validate down to roughly `bash` + `sshd` +
   root-at-boot. Incremental and testable per optional subsystem; biggest leverage per unit work.
3. **Layer B2** — write the (now-smaller) contract doc + add the pre-flight probe. Makes "bring
   your own image" real and safe with no per-image install cost.
4. **Layer B1** — only if minimal bases are genuinely needed; opt-in flag, accept build cost.
5. **Layer C** — leave as a documented invariant: md owns pid 1 and replaces the base `CMD`.

## Observations for later

- The four VNC/monitor scripts only matter under `--display`. Consider COPYing them only when
  display is requested, to keep non-display specialized images smaller.
- `start.sh` mixes concerns (perms, dbus, vnc, tailscale, sshd, nested-podman /proc fixups).
  Splitting into composable units would make a non-Debian port tractable, let the pre-flight
  probe reuse the same dependency list, and make the B3 per-subsystem guards fall out naturally.
- B3's degrade guards and B2's probe want the *same* capability list. Generating both — guards,
  probe, and docs — from one machine-readable manifest avoids drift between "what we skip" and
  "what we require".
- A machine-readable manifest of the contract (the table in Layer B) could drive both the
  pre-flight probe and the docs from one source.

### Pre-existing issues surfaced while investigating

These are not introduced by this work but bear on it; fix independently.

- **UID 1000 is an unenforced invariant.** `4_create_user.sh` runs `useradd -ms /bin/bash user`
  with no `-u`, so `user` only gets UID 1000 by virtue of being the first account on fresh
  Debian. The entire cache-injection model (`COPY --chown=user:user`, the boot-time provisioning
  in B3, the B2 probe) depends on UID 1000. If a future root setup script ever creates an account
  first, the UID drifts silently and cache ownership breaks. Pin it: `useradd -u 1000 -ms
  /bin/bash user`. Doing this now de-risks every Layer-B path.
