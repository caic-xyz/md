# Environment

You are running inside a docker container.

Subdirectories from the current working directory are the projects (as git repositories) the user wants to work on.

## Preinstalled Tools

The complete list of tool versions is at `tool_versions.md`

Notable executables available in the container:

- Core utilities: bash, git, curl, wget, rsync, jq, grep, ripgrep, less, file, find, xargs, sed, awk, bubblewrap, xvfb, tokei, xxd
- Editors: nvim (vi, vim, vimdiff)
- Browsers: google-chrome (amd64), chromium (arm64 fallback), chrome-devtools-mcp
- Compression: brotli, zstd, unzip
- Development: build-essential, git, actionlint, shellcheck, shfmt, golangci-lint, cmake, ninja-build, ccache, make, gcc, g++, cargo-binstall, pylint, ruff, uv, asciinema
- Embedded: flex, bison, gperf, dfu-util, libusb-1.0-0
- Languages: go, python3, java, rust (cargo, rustc)
- Languages (web): node (v24), npm, npx, pnpm, bun, typescript, bun, eslint, tsx
- AI Tools: claude, gemini, codex, kilo, qwen-code, opencode, amp
- Virtualization: qemu-kvm, libvirt-clients
- Media: ffmpeg, imagemagick
- Android: android-sdk, gradle, adb, sdkmanager
- Database: sqlite3
- Network: curl, wget, net-tools, iproute2, tailscale
- GitHub: gh
- Debugging: strace, lsof, dlv (Go), lldb/rust-lldb (Rust), objdump, radare2 (r2)

Web Remote Debugging: `google-chrome --remote-debugging-port` requires `--user-data-dir` pointing to a non-default directory.
