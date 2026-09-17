# Git model: host branches, container branches, and what each command does

`md` keeps two checkouts of one repository: yours on the host, and one inside
the container. They share nothing. Every transfer is an explicit Git push or
fetch over SSH, which is why a command that moves work between them names the
direction: `md push` writes into the container, `md pull` reads out of it.

## Branches on the host

`md` maps one or more of your branches into a container. The first one is the
primary: the container checks it out and its name goes into the container name.

Each mapped branch must exist and must have an upstream, because **the container
branch is configured to track the same upstream**. `md` refuses to start, revive,
synchronize or fork otherwise, and tells you the command that repairs it.

A mapped branch may track another local branch through Git's `.` remote, and
`md` then mirrors that local branch into the container so the tracking resolves
there too. One case is refused: a non-primary mapped branch that tracks the
primary. `md fork` renames the primary, which would leave that branch tracking
a name the fork no longer has.

Per container, `md` adds two things to your checkout on the host:

- a Git remote named after the container, pointing at it over SSH;
- `refs/remotes/<container>/<branch>` for each mapped branch, holding what the
  container had at the last transfer. `md pull` integrates from these refs, so
  they are the record of what the host has seen.

`md` moves your branches in only two cases: `md pull` integrates container work
into them, and `md fork` creates new ones. Branches you did not map are never
read or written.

## Branches in the container

The container's checkout is built by pushing into it, not by cloning from your
remote. It holds:

- the mapped branches, with the primary checked out;
- the same upstream, and the same effective push remote, on each mapped branch
  as its host branch has;
- every remote-tracking ref your host checkout had cached, and the tags
  selected by `--tags`;
- while a branch is being reset, `refs/md/incoming/<branch>`, an internal
  checkout seed containing the host branch's commit when no mirrored remote ref
  holds that exact commit; it is deleted after the reset succeeds;
- `refs/md/sync/<branch>`, the integration point: the container commit most
  recently established in the host branch. This is what `md diff` compares
  against;
- your Git identity, so commits made in the container are attributed to you.
- the repository-local `core.hooksPath` when it is a relative path that stays
  within the repository, so its hooks run in the container too. Absolute,
  home-relative and repository-escaping hook paths are not transferred.

Several subtleties follow.

The mirrored remote refs make `git rebase origin/main` work inside the
container with no network and no credentials. Every command below re-pushes
them from your checkout before doing its own work, so they are as fresh as your
host checkout, and no fresher: fetch on the host first when a task needs the
latest remote state.

The integration point is a ref, not a recorded commit ID, so it keeps its commit
reachable. An agent that amends, resets or rebases its branch does not destroy
the base of your next diff. The container's Git configuration also disables
pruning of unreachable objects, which trades a growing object store for the
same guarantee; container lifetimes are finite, so the leak is acceptable.

Ordinary operations do not delete old integration points, so a branch dropped
from the mapping leaves its ref behind. A fork is the exception: it deletes the
old primary name and records fresh integration points for the fork's mapped
branches.

Containers created by older versions may retain an unused synthetic `host`
remote and `refs/remotes/host/*`. Current commands neither read nor update that
state; it disappears when the container is purged and recreated.

## What each command does

### md start, md run

`md start` creates the container, pushes your mapped branches, cached remote
refs and selected tags into it, configures the remotes, upstreams and identity,
checks out the primary branch, and records an integration point per branch. On
the host it only adds the container remote. Your branches do not move.

`md start` refuses a container that is already running and tells you to `ssh`
in. It revives a stopped one, without resetting its branches or its integration
points, so work in progress survives a stop.

`md run` provisions a temporary container the same way, runs a command, then
purges it. Whatever the command did is destroyed with the container unless you
pass `--apply-patch`, which pulls each repository back to the host first.

### md fork

`md fork` snapshots the source container's filesystem into an image and starts a
new container from it, so the fork inherits the source's work including
uncommitted changes. It then creates new host branches with the same upstreams,
renames the primary inside the fork, fetches every committed mapped branch tip
to the host, and records those tips as the fork's fresh integration points.
Therefore inherited committed work is already present in the new host branches
and does not appear in `md diff`; inherited uncommitted work still appears.

The source container is not modified. Each forked repository's primary branch
must have a new name; reusing any branch mapped by the source is refused. Its
non-primary mapped branches keep their existing names.

The new host branches are created before the fork's repository setup finishes,
so committed work has a host-side anchor while later steps run. If setup fails,
`md` removes the partial fork and rolls those branch changes back. A rollback
deletes a newly created branch or restores a pre-existing branch only while it
still points to the exact commit written by `md`; a concurrently moved branch
is preserved and reported instead.

### md push

`md push` overwrites the container with your host state. Before that it saves
the container's Git-visible work: it commits anything dirty, then creates
`backup-<timestamp>` at the container's HEAD and one
`backup-<timestamp>-<n>-<branch>` per mapped branch. Ignored untracked files are
not committed and can be overwritten if the host branch tracks the same path.
The container's checked-out state is then replaced.

It then force-pushes your mapped branches into the container, resets the
container's branches to them, and moves each integration point to the pushed
commit. So `md diff` reports nothing immediately after a push.

It refuses to run when you have uncommitted changes on a mapped branch on the
host, since those would not reach the container.

### md pull

`md pull` brings the container's work into your branches. It commits the
container's pending changes first, with a message written by an AI provider
unless you pass `-no-describe`, fetches every mapped branch into
`refs/remotes/<container>/<branch>`, then integrates each host branch:

- fast-forward, when your branch is an ancestor of the container's;
- rebase, when you have commits the container does not;
- `reset --hard`, when the container rewrote history and your branch still
  points exactly at the old container tip. An untracked file that blocks a
  tracked path can be removed here;
- `rebase --onto`, when the container rewrote history and you also added
  commits on top of the old tip.

So `md pull` can rewrite host commit IDs. It refuses to start when the host has
staged or unstaged changes to tracked files, or when a rebase is in progress. It
does not reset container branches from host state. After every mapped branch is
integrated successfully, it updates the container's integration points. A
failed integration leaves them unchanged, so unapplied work remains visible.
It also updates the container's remote configuration and cached remote refs,
and commits pending container work before integrating it.

Because that container commit runs in the repository checkout, it triggers the
repository's own `pre-commit` and `commit-msg` hooks. Pass `-no-verify` to
commit with `git commit --no-verify` instead, skipping both hooks. The flag
also applies to `md pull -all`.

A library caller that wants the host to take only what the container committed
can fetch without the commit step, with `Container.Fetch` and the zero
`FetchOpts`. The container's history and working tree then stay untouched, its
pending changes stay pending, and the exact fetched branch tips are returned.
Only the host's remote-tracking refs move; the integration points and host
branches do not.

### md diff

`md diff` reports the checked-out container branch's changes since its
integration point, including uncommitted and untracked files. `md diff -full`
reports the whole branch from its upstream merge base instead.

It moves no branch on either side. Like every command it re-pushes the cached
remote refs, and it first checks that every mapped branch still exists on the
host with the same upstream the container records; on a mismatch it stops and
asks for `md pull` or `md push`, which reconfigure the container.

Two cases make the base something other than the last integration point, and
`md diff` says so on stderr:

- a branch created inside the container has no integration point, so the diff
  covers the whole branch;
- a branch moved onto a newer upstream carries the new upstream commits into the
  diff. Use `md diff -full` to see the branch against its upstream instead.

The second note fires when the upstream tip is reachable from the branch but not
from its integration point. A rebase that replays the upstream commits onto the
branch's own work produces new commit IDs, which that test cannot recognize, so
the note says the diff *may* include upstream commits rather than promising it.

An interrupted rebase leaves the container's HEAD detached. `md diff` then
recovers the branch being rebased from Git's rebase state, so it still compares
against that branch's integration point.
