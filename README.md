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

# Check what the container did since the last sync
md diff

# Check the whole branch
md diff -full

# Pull changes back when done
md pull
```

### Git synchronization commands

`md` keeps a container checkout separate from the host checkout. Each command
names the direction it moves work:

| Command | Effect |
| --- | --- |
| `md start` | Copies the mapped branches, cached remote refs and tags into a new container, then checks out the primary branch there. Your branches do not move. |
| `md diff` | Reports what the container did since the last synchronization. `md diff -full` reports the whole branch. It moves no branch. |
| `md pull` | Commits the container's pending changes, then integrates every mapped branch into your host branches. It can rewrite host commit IDs. |
| `md push` | Saves the container's Git-visible work on timestamped backup branches, then replaces the container's mapped branches with your host state. |
| `md fork` | Snapshots the container and starts a new one on new host branches. The source container is untouched. |

Every mapped host branch must exist and have an upstream, because the container
branch tracks the same upstream. `md` reports an error and the repair command
when that is not the case.

### Sync points

Every command that synchronizes the host and the container remembers where each
mapped branch stood. `md diff` shows the work done since then, so you see what
the container did since you last looked at it, and `md diff -full` shows the
whole branch. The record survives the agent amending, resetting or rebasing its
branch.

[docs/GIT_MODEL.md](docs/GIT_MODEL.md) explains what each side holds, what each
command moves, and the cases where a diff reports something other than the work
since the last synchronization.

### Remote branches and fork workflows

`md` mirrors every cached remote branch from the host into the container. A task
without network or repository credentials can therefore run commands such as
`git rebase origin/main` or `git rebase upstream/release`. The refs are only as
fresh as the host checkout; fetch on the host before starting the task when the
latest remote state is required.

All host remotes are configured in the container. A branch's effective Git push
remote is preserved independently from its upstream, so triangular workflows
can rebase against `upstream` and push to `origin`. An actual network push from
the container still requires repository access, such as `md start --github` for
GitHub repositories.

`md start` maps all host tags by default, equivalent to `--tags '.*'`. Use
`md start --tags '<regexp>'` to limit mapped tags, for example
`--tags '^v2\.'`, or `--tags ''` to map none. Quote the expression so the shell
does not expand it. The same expression filters tags in initialized submodules.
Selecting fewer tags avoids transferring old histories reachable only from tags
in repositories with large tag sets. `md run` and the Go API retain opt-in
semantics: an empty tag expression maps no tags.

### Multiple mapped branches

`md start --extra-branch <branch>` maps additional branches into the same
container. The first branch, your current branch or `-b`, remains the primary
one and is the branch the container checks out. `md diff` reports whichever
branch is checked out there; `push`, `pull` and `fork` cover every mapped
branch.

`md pull` integrates mapped branches in order. If a later branch cannot be
rebased, branches already completed remain integrated; the failed rebase is
aborted and the original host checkout is restored.

## Documentation

🔥 Full documentation is at [docs.caic.xyz](https://docs.caic.xyz/md/) 🔥

## Contributing

Made with ❤️  by [Marc-Antoine Ruel](https://maruel.ca). Contributions are very appreciated! Thanks in advance! 🙏
