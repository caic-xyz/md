# md: My Development containers

Each container is locked to a repository-branch pair. No confusion. Safe parallel work.

**Safe parallel work with multiple AI coding agents.** Run Claude Code, Codex,
Amp CLI, Gemini CLI, Kilo CLI, Pi, and other tools in isolated containers
without branch conflicts, file interference, or environmental headaches.

[![codecov](https://codecov.io/gh/caic-xyz/md/graph/badge.svg?token=Q2ZK312LNF)](https://codecov.io/gh/caic-xyz/md)

## Installation

```bash
curl caic.xyz/install.sh | bash
```

### From source

```bash
go install github.com/caic-xyz/md/cmd/md@latest
```

**Recommended:** Also install [git-maruel](https://github.com/maruel/git-maruel) for the `git squash` and `git rb` helpers.

## Quick Start

```bash
# Start container for your current branch; this automatically ssh in.
git checkout -b wip origin/main
md start

# You are now inside the container
cd ~/src/<repo-name>
claude
exit

# Check pending changes
md diff

# Pull changes back when done
md pull
```

### Multiple mapped branches

`md start --extra-branch <branch>` maps additional branches into the same container. The first branch (`Branches[0]`: current branch or `-b`) remains the primary branch.

For now, `md diff` reports changes for the primary branch only. Extra mapped branches are available for `push`, `pull`, and `fork`, but they are not diffed by `md diff`; inspect them inside the container with Git directly.

## Documentation

🔥 Full documentation is at [docs.caic.xyz](https://docs.caic.xyz/md/) 🔥

## Contributing

Made with ❤️  by [Marc-Antoine Ruel](https://maruel.ca). Contributions are very appreciated! Thanks in advance! 🙏
