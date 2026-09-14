# Environment

You are running inside a container. Rootless Podman keeps the `user` account at UID/GID 1000 and maps a differently numbered host user to it. Other runtimes may rewrite the account to the host UID/GID at startup.

Subdirectories from the current working directory are the projects (as git repositories) the user wants to work on.

## Installing a missing tool

When a tool is missing, tell the user to run:

```bash
docker exec -it -u root <container> bash -lc 'apt-get install -y <package>'
```

## Preinstalled Tools

The complete list of tool versions is at `tool_versions.md`

Notable executables available in the container:

- Core utilities: bash, git, curl, wget, rsync, jq, grep, ripgrep, less, man, file, find, xargs, sed, awk, col, bubblewrap, inotifywait, time, xvfb, tmux, tokei, xxd, sudo
- Editors: nvim (vi, vim, vimdiff)
- Browsers: google-chrome (amd64), chromium (arm64 fallback), chrome-devtools-mcp
- Compression: brotli, zstd, unzip
- Development: build-essential, git, actionlint, shellcheck, shfmt, golangci-lint, cmake, ninja-build, ccache, make, gcc, g++, arm-linux-gnueabi-gcc, libsodium-dev, protobuf-compiler, cargo-binstall, pylint, ruff, uv, asciinema
- Embedded: flex, bison, gperf, dfu-util, libusb-1.0-0, pahole (dwarves), libelf-dev. When md starts with `-usb`, it maps currently attached `/dev/ttyUSB*` and `/dev/ttyACM*` serial adapters; restart the container after attaching another adapter.
- Languages: go, python3, java, R, rust (cargo, rustc)
- Languages (web): node (v24), npm, npx, pnpm, bun, typescript, bun, eslint, tsx
- Math: bc
- AI Tools: claude, codex, kilo, qwen-code, kimi, opencode, amp, pi, agent-browser
- Containers: podman (rootless, requires -sudo for nested containers)
- Virtualization: qemu-kvm, libvirt-clients, libguestfs-tools (guestfish, guestmount, virt-inspector)
- Media: ffmpeg, imagemagick
- Android: android-sdk, gradle, adb, sdkmanager
- Database: sqlite3
- Network: curl, wget, net-tools, iproute2, ping, fping, traceroute, tcptraceroute, nmap, tcpdump, dig, host, nslookup, whois, sshpass, tailscale
- GitHub: gh
- Debugging: binwalk, strace, lsof, dlv (Go), lldb/rust-lldb (Rust), objdump, aarch64-linux-gnu-objdump, arm-linux-gnueabi-objdump, radare2 (r2)

Web Remote Debugging: `google-chrome --remote-debugging-port` requires `--user-data-dir` pointing to a non-default directory.
