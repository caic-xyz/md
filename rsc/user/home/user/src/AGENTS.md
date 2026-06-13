# Environment

You are running inside a docker container.

Subdirectories from the current working directory are the projects (as git repositories) the user wants to work on.

Bash shells source `/etc/bash_env` for PATH and environment setup. This includes non-interactive shells via `BASH_ENV`, login Bash shells via `/etc/profile.d/00-bash-env.sh`, and interactive shells via `/etc/bash.bashrc`.

## Preinstalled Tools

The complete list of tool versions is at `tool_versions.md`

Notable executables available in the container:

- Core utilities: bash, git, curl, wget, rsync, jq, grep, ripgrep, less, man, file, find, xargs, sed, awk, col, bubblewrap, inotifywait, xvfb, tmux, tokei, xxd, sudo
- Editors: nvim (vi, vim, vimdiff)
- Browsers: google-chrome (amd64), chromium (arm64 fallback), chrome-devtools-mcp
- Compression: brotli, zstd, unzip
- Development: build-essential, git, actionlint, shellcheck, shfmt, golangci-lint, cmake, ninja-build, ccache, make, gcc, g++, cargo-binstall, pylint, ruff, uv, asciinema
- Embedded: flex, bison, gperf, dfu-util, libusb-1.0-0
- Languages: go, python3, java, R, rust (cargo, rustc)
- Languages (web): node (v24), npm, npx, pnpm, bun, typescript, bun, eslint, tsx
- AI Tools: claude, gemini, codex, kilo, qwen-code, kimi, opencode, amp, pi, agent-browser
- Containers: podman (rootless, requires -sudo for nested containers)
- Virtualization: qemu-kvm, libvirt-clients
- Media: ffmpeg, imagemagick
- Android: android-sdk, gradle, adb, sdkmanager
- Database: sqlite3
- Network: curl, wget, net-tools, iproute2, ping, fping, traceroute, tcptraceroute, nmap, tcpdump, dig, host, nslookup, whois, tailscale
- GitHub: gh
- Debugging: strace, lsof, dlv (Go), lldb/rust-lldb (Rust), objdump, radare2 (r2)

Web Remote Debugging: `google-chrome --remote-debugging-port` requires `--user-data-dir` pointing to a non-default directory.
