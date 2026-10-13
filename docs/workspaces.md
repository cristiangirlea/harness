# Reusable workspaces

[← README](../README.md)

An agent that needs a clean checkout usually runs `git worktree add` beside the
repository and rarely removes it, and each new folder builds its `target/` or
`node_modules/` from nothing. `hrn ws` keeps a fixed set of worktrees per repository
instead, called slots, and hands them out one at a time. A slot keeps its build output
from one task to the next.

```bash
cd "$(hrn ws take --branch fix-x)"   # a slot on branch fix-x; only its path goes to stdout
# ... work, commit, git push -u origin fix-x ...
hrn ws free                          # give it back (from anywhere inside the slot)
hrn ws list                          # who holds which slot, and what is in it
```

## Slots and leases

Slots live at `<main checkout>/.hrn/slots/1`, `2`, and so on, whichever worktree of the
repository you run hrn from. hrn writes `.hrn/.gitignore` so they never show in the
repository's status. A slot is made on first use with `git worktree add --detach` and
then locked (`git worktree lock --reason "hrn ws slot"`), so `hrn gc` and
`git worktree prune` leave it alone. A bare repository has no checkout and is refused.

Who holds a slot is a lease file beside it, `<n>.lease`:
`{"holder": "...", "taken": "...", "branch": "..."}`. It is created so that two callers
can never get the same slot. A lease held from the command line lasts until
`hrn ws free`. A lease that names a process (`"pid"`, which batch uses) is stale once
that process has exited, and the next take may take it over.

## Batches

`hrn batch` takes the same slots, one per running task, unless its spec says
`"worktrees": "fresh"` (see [Slots or fresh worktrees](batches.md#slots-or-fresh-worktrees)).
Its lease names the task (`batch <id> task <task>`) and hrn's pid, so a crash leaves a
stale lease rather than a held slot. A batch waits only on slots other batches hold: a slot
held by `hrn ws take` (no pid) among the ones it would try is passed over and it tries the
next number instead, up to the bound on its own slots (`parallel`, at least 4). The task's branch `hrn/<id>` is made from the batch's
base and checked out in the slot. When the task ends, the slot is detached at the base
and its lease removed without asking for a push, because the work is meant to stay on
that local branch for `hrn batch merge`. A slot left with files in it keeps its lease
until hrn exits. A slot whose HEAD commits are on a local branch or tag is not skipped,
since a switch strands nothing there.

## Take

`hrn ws take` walks slots 1 to `--max` (default 4) and leases the first one that has no
lease and nothing in it:

- A slot whose folder holds changes or untracked files, or whose HEAD has commits that
  a switch would strand (on no remote, local branch or tag), is skipped and named on
  stderr. hrn never cleans or
  resets someone's files. Ignored files (build output) are never
  touched.
- `--branch NAME` checks out the local branch of that name, or origin's branch of that
  name, tracked. If neither exists, it makes a new branch from the base. Git refuses a
  branch that is checked out in another worktree; hrn shows git's message and releases
  the lease.
- Without `--branch`, the slot is detached at the base.
- The base is `--base REF`, or origin's default branch, fetched first. With `--no-fetch`
  it is `origin/HEAD` as already fetched; if the clone has no `origin/HEAD`, take refuses
  rather than guess, and asks for `--base` or a fetch. A repository without an origin
  uses the main checkout's `HEAD`.

`--for TEXT` is the holder written into the lease (default `hrn ws take`). When every
slot up to `--max` is taken or skipped, take fails and names each slot, its holder and
since when, or why it was skipped.

## Free

`hrn ws free [PATH]` (default: the current folder) gives back the slot PATH is in. It
refuses while the slot holds changes or untracked files, and names the first few. It
also refuses while HEAD has commits that are on no remote, and says how many. In a
repository without remotes it checks for commits that are on no branch or tag instead.
It refuses a lease that names a process still running (a batch task's), which gives the
slot back itself. Otherwise it detaches HEAD where it is, so the branch is free for other checkouts and
for `hrn gc`, and removes the lease. The folder and its build output stay for the next
take.

## List

`hrn ws list [--json]` prints one row per slot folder or lease: the number, the path,
`free`, `taken` or `stale`, the holder, since when, the branch (or the detached commit),
and `clean` or what is in it. A lease without a folder and a folder without a lease
both show.
