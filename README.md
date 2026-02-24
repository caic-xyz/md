# md: Branch-locked Development Containers for AI Coding Agents

Each container is locked to a repository-branch pair. No confusion. Safe parallel work.

**Safe parallel work with multiple AI coding agents.** Run Claude Code, Codex,
Amp CLI, Gemini CLI, Kilo CLI, Pi, and other tools in isolated containers
without branch conflicts, file interference, or environmental headaches.

## The Problem

AI coding agents work best when given full command execution (YOLO mode). But running them locally is risky:

- **Branch conflicts** - Agent changes on one branch interfere with your local checkout
- **Test conflicts** - Running tests simultaneously causes failures and race conditions
- **Environment pollution** - Dependencies and state accumulate, causing hidden bugs
- **Context switching** - Switching branches while an agent is working loses progress

## The Solution

`md` gives each AI agent a **complete, isolated container** with a full git clone. You can:

- Run agents on multiple branches simultaneously
- Switch local branches without affecting running agents
- Run tests in parallel without conflicts
- Keep your local checkout clean
- Delete containers cleanly when done
- Access your frontend dev server over [Tailscale](TAILSCALE.md) with HTTPS!
- Share your host's USB port for Android debugging
- Let the coding agent control an Android Emulator and see it over VNC

## Quick Start

```bash
# Start container for your current branch; this automatically ssh in.
git checkout -b wip origin/main
md start

# You are now inside the container
cd ~/src/<repo-name>
# Run the coding harness of your choice, a bash alias will automatically start it in YOLO mode:
claude
# Exit from the ssh session into the container (or use a separate terminal)
exit

# Check pending changes
md diff

# Pull changes back when done
md pull

# Clean up the container
md kill
```

## Installation

```bash
go install github.com/caic-xyz/md/cmd/md@latest
```

**Recommended:** Also install [git-maruel](https://github.com/maruel/git-maruel) for the `git squash` and `git rb` helpers.

### Harnesses

For coding agents: The container includes `/home/user/src/AGENTS.md` which provides information about
preinstalled tools (list available in `~/src/tool_versions.md`).

Harnesses preinstalled:

- [amp](https://ampcode.com/manual#AGENTS.md): `~/.config/amp/AGENTS.md`
- [claude](https://www.anthropic.com/engineering/claude-code-best-practices): `~/.claude/CLAUDE.md`
- [codex](https://developers.openai.com/codex/guides/agents-md/): `~/.codex/AGENTS.md`
- [gemini](https://geminicli.com/docs/cli/gemini-md/): `~/.gemini/GEMINI.md`
    - Recommended in `~/.qwen/settings.json` to change `"context"` / `"fileName"` to `AGENTS.md`
- [kilo](https://kilo.ai/docs/agent-behavior/custom-rules): `~/.kilocode/rules/*.md`
- [opencode](https://opencode.ai/docs/rules/): `~/.config/opencode/AGENTS.md`
- [pi](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/README.md): `~/.pi/agent/AGENTS.md`
- [qwen](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/settings/#example-context-file-content-eg-qwenmd): `~/.qwen/QWEN.md`
    - Recommended in `~/.qwen/settings.json` to change `"context"` / `"fileName"` to `AGENTS.md`

### Readme for agents (https://agents.md) and Skills (https://agentskills.io)

FYI, here's locations of AGENTS.md for each harness:

```bash
mkdir -p ~/.config/agents ~/.config/amp ~/.claude ~/.codex ~/.gemini ~/.kilocode/rules ~/.config/opencode ~/.pi/agent ~/.qwen
echo "Read ~/AGENTS.md if present." >> ~/.config/agents/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.config/amp/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.claude/CLAUDE.md
ln -s ../.config/agents/AGENTS.md ~/.codex/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.gemini/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.kilocode/rules/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.config/opencode/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.pi/agent/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.qwen/AGENTS.md
```

FYI, here's locations of skills for each harness:

- [amp](https://ampcode.com/manual#agent-skills): `~/.config/agents/skills/**/SKILL.md` (recursive)
    - Fallbacks to `~/.claude/skills/`
- [antigravity](https://antigravity.google/docs/skills): `~/.gemini/antigravity/skills/<name>/SKILL.md`
- [claude](https://code.claude.com/docs/en/skills): `~/.claude/skills/<name>/SKILL.md`
- [codex](https://developers.openai.com/codex/skills): `~/.codex/skills/**/SKILL.md` (recursive)
- [cursor](https://cursor.com/docs/context/skills): `~/.cursor/skills/<name>/SKILL.md`
    - Fallbacks to `~/.claude/skills/`
- [gemini](https://geminicli.com/docs/cli/skills/): `~/.gemini/skills/<name>/SKILL.md`
- [kilo](https://kilo.ai/docs/agent-behavior/skills): `~/.kilocode/skills/<name>/SKILL.md`
- [opencode](https://opencode.ai/docs/skills/): `~/.config/opencode/skill/<name>/SKILL.md`
    - Fallbacks to `~/.claude/skills/`
- [pi](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/README.md#skills): `~/.pi/agent/skills/**/SKILL.md` (recursive)
    - Fallbacks to `~/.claude/skills/`, `~/.codex/skills/` (recursive)
- [qwen](https://qwenlm.github.io/qwen-code-docs/en/users/features/skills/): `~/.qwen/skills/<name>/SKILL.md`

Centralize your skills with symlinks:

```bash
mkdir -p ~/.config/agents/skills ~/.claude ~/.codex ~/.cursor ~/.gemini/antigravity ~/.kilocode ~/.config/opencode ~/.pi/agent ~/.qwen
ln -s ../.config/agents/skills/ ~/.claude/skills
ln -s ../.config/agents/skills/ ~/.codex/skills
ln -s ../.config/agents/skills/ ~/.cursor/skills
ln -s ../../.config/agents/skills/ ~/.gemini/antigravity/skills
ln -s ../.config/agents/skills/ ~/.gemini/skills
ln -s ../.config/agents/skills/ ~/.kilocode/skills
ln -s ../../.config/agents/skills/ ~/.config/opencode/skill
ln -s ../../.config/agents/skills/ ~/.pi/agent/skills
ln -s ../.config/agents/skills/ ~/.qwen/skills
```

## How It Works

### Container Setup

Each container is named `md-<repo-name>-<branch-name>` with:

- **Isolated git clone** - `~/src/<repo-name>` inside the container is a git clone of your local repository. It tracks branch `base` which matches your local branch. This is useful for commit-happy agents like Codex to track pending changes.
- **User-mapped permissions** - Container runs as your local user ID for proper file permissions
- **SSH access** - Connect via `ssh md-<repo>-<branch>`
- **Remote GUI (VNC)** - Optional full desktop environment (via `-display`) accessible via VNC on a dynamic port
- **Remote network (Tailscale)** - Optional full access to your tailnet (via `-tailscale`)
- **Local USB debugging** - Optional USB debugging (via `-usb`), especially for Android development
- **Minimal overhead** - Only sshd runs by default; no unnecessary background services

### Preinstalled Tools

- TypeScript, Go, Rust, Node.js, Python development environments
- Neovim editor
- Android SDK and ADB on x64
- And more! See [rsc/root/setup/](rsc/root/setup/) and [rsc/home/user/setup/](rsc/home/user/setup/).

### Configuration

Agent configurations and credentials are automatically mounted:

- Agent configurations: `~/.amp`, `~/.claude`, `~/.codex`, `~/.gemini`, `~/.kilocode`, `~/.kimi`, `~/.pi`,
  `~/.qwen`, `~/.config/agents`, `~/.config/amp`, `~/.config/goose`, `~/.config/opencode`,
  `~/.local/share/amp`, `~/.local/share/goose`, `~/.local/share/opencode`, `~/.local/state/opencode`
- Android ADB keys: `~/.android`

### Build Cache Injection

`md start` and `md run` automatically bake your local build-tool caches into the `md-user` Docker image at
build time. This means the container starts with warm caches, skipping the slow cold downloads that would
otherwise happen on every fresh container.

**Enabled by default** (host directory must exist; silently skipped otherwise):

| Name | Host path | Container path |
|------|-----------|----------------|
| `bun` | `~/.bun/install/cache` | `/home/user/.bun/install/cache` |
| `cargo` | `~/.cargo/registry`, `~/.cargo/git` | `/home/user/.cargo/{registry,git}` |
| `go-mod` | `~/go/pkg/mod` | `/home/user/go/pkg/mod` |
| `gradle` | `~/.gradle/caches`, `~/.gradle/wrapper/dists` | `/home/user/.gradle/{caches,wrapper/dists}` |
| `maven` | `~/.m2/repository` | `/home/user/.m2/repository` |
| `npm` | `~/.npm` | `/home/user/.npm` |
| `pip` | `~/.cache/pip` | `/home/user/.cache/pip` |
| `pnpm` | `~/.local/share/pnpm/store` | `/home/user/.local/share/pnpm/store` |
| `uv` | `~/.cache/uv` | `/home/user/.cache/uv` |

The `md-user` image is only rebuilt when the set of available caches changes, the base image updates, or
the build context changes. Cache contents are snapshotted at build time; they are not kept in sync after
that.

**Opt out** of specific caches:

```bash
md start --no-cache go-mod --no-cache cargo   # skip specific caches
md start --no-caches                           # disable all caches
md start --no-caches --cache go-mod            # only go-mod
```

**Custom cache directories** (any `host:container[:ro]` path pair):

```bash
md start --cache /path/to/my/cache:/home/user/.mycache
```

Environment variables can be passed via:

1. `.env` file in your repository (auto-mapped)
2. `~/.config/md/env` on your local machine (applies to all containers)

For example:

```bash
# ~/.config/md/env
ANTHROPIC_API_KEY=your_key
OPENAI_API_KEY=your_key
```

### GitHub Authentication

The container doesn't have access to your GitHub credentials. To enable git credentials and access to GitHub
(e.g. to create PRs or issues), authenticate inside the container via:

```bash
gh auth login
```

## Commands

| Command | Purpose |
|---------|---------|
| `md start` | Create and start a container for the current branch |
| `md start -display` | Start container with X11/VNC desktop environment enabled |
| `md start -tailscale` | Start container with [Tailscale](TAILSCALE.md) networking for remote SSH access |
| `md start -usb` | Start container with USB device passthrough (for ADB, etc.) |
| `md start -no-cache <name>` | Exclude a default cache (repeatable); e.g. `-no-cache go-mod` |
| `md start -no-caches` | Disable all default caches |
| `md start -cache host:container` | Add a custom cache directory |
| `md run <cmd>` | Start a temporary container, run a command, then clean up |
| `md list` | List all md containers |
| `ssh md-<repo>-<branch>` | Access the container via SSH |
| `md vnc` | Open VNC connection to the container |
| `md diff` | Show changes (runs `git diff base`). Arguments are passed through, e.g. `md diff -- --stat` |
| `md pull` | Pull changes from container back to local branch |
| `md push` | Push local changes to the container |
| `md kill` | Stop and remove the container |
| `md build-image` | Build the base Docker image locally as `md-local` |
| `md start -image md-local` | Start a container using the locally built base image |

## Remote GUI Access (VNC)

Each container can include a full XFCE4 desktop environment. It must be enabled at startup:

```bash
md start --display
md vnc
```

`md vnc` opens the VNC connection in your default VNC client.

**Recommended VNC clients by OS:**
- **Windows**: [RealVNC Viewer](https://www.realvnc.com/download/viewer/), [TightVNC](https://www.tightvnc.com/), or [UltraVNC](https://www.uvnc.com/)
- **macOS**: Built-in VNC support or [RealVNC Viewer](https://www.realvnc.com/download/viewer/)
- **Linux**: `tigervnc-viewer`, `vinagre`, or `vncviewer` command-line tools

The DISPLAY environment variable is automatically set in SSH sessions, so X11 applications launched from SSH will appear on the VNC desktop.

## Contributing

Made with ❤️  by [Marc-Antoine Ruel](https://maruel.ca). Contributions are very appreciated! Thanks in advance! 🙏
