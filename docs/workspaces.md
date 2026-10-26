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
Takes add, list and lock slot worktrees one at a time, holding an OS file lock on
`.hrn/slots/.create.lock` while they do; a take waits up to two minutes for it. The
system releases the lock when its holder exits or crashes, so the file itself stays and
is harmless: it never needs deleting.
Git commands used by slot takes and batches can also inspect worktrees outside
that lock. If Git fails to read `.git/worktrees/<name>/commondir` while another
worktree is being created, hrn waits 250 ms and retries the command once. Other
errors, or a second failure, are returned as usual; cancellation stops the wait.
These commands use `LC_ALL=C` and an empty `LANGUAGE` so the retry recognises
Git's error text. Batch task creation (`worktree add -b`) is not retried: Git
may already have created the branch before failing to read another worktree.

Who holds a slot is a lease file beside it, `<n>.lease`:
`{"holder": "...", "taken": "...", "branch": "..."}`. It is created so that two callers
can never get the same slot. A lease held from the command line lasts until
`hrn ws free`. A lease that names a process (`"pid"`, which batch uses) is stale once
that process has exited, and the next take may take it over.
A process lease records its holder's start identity in `"start"` when available. On
Windows and Linux, a live process with the same id but another start does not hold it.
Leases written before `"start"` was added keep the old rule: a live pid holds the slot.

## Batches

`hrn batch` takes the same slots, one per running task, unless its spec says
`"worktrees": "fresh"` (see [Slots or fresh worktrees](batches.md#slots-or-fresh-worktrees)).
Its lease names the task (`batch <id> task <task>`) and hrn's pid, so a crash leaves a
stale lease rather than a held slot. A batch never waits on a session: a slot
held by `hrn ws take` (no pid) among the ones it would try is passed over and it tries the
next number instead, up to the bound on its own slots (`parallel`, at least 5,
capped at 10). It waits on
slots other batches or its own running tasks hold, and on a lease it cannot read (see
[Batches](batches.md#slots-or-fresh-worktrees)). The task's branch `hrn/<id>` is made from the batch's
base and checked out in the slot. When the task ends, the slot is detached at the base
and its lease removed without asking for a push, because the work is meant to stay on
that local branch for `hrn batch merge`. A slot left with files in it keeps its lease
until hrn exits. A slot whose HEAD commits are on a local branch or tag is not skipped,
since a switch strands nothing there.

## Take

`hrn ws take` walks slots 1 to `--max` (default 5, at most 10 per repository)
and leases the first one that has no lease and nothing in it:

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

An empty orphan branch has no commits to keep. Free detaches such a slot at the
main checkout's HEAD and removes its lease.

## A slot a batch kept

When a batch task ends with files in its slot, or with a commit on no branch, the slot
is not given back (`slot <n> kept` in the task's error). Free refuses it as it stands:
the work is on no remote, and a batch's work sits on a local branch by design. To get
the slot back:

1. Find it. `hrn ws list` shows it `taken` while hrn runs and `stale` once hrn has
   exited, with the holder `batch <id> task <task>`. If it is still `taken`, wait for
   the batch to end: free refuses a lease whose process is running.
2. Look at what is in it: `git -C <slot> status --ignored` and `git -C <slot> log`.
   Plain `git status` hides ignored files, and free does not look at them either; but
   checking out another commit overwrites an ignored file wherever that commit tracks the
   same path, in step 4 or at the slot's next take. Copy out any ignored file you value now.
3. Keep the work on a branch. Commit what you want to keep (`git -C <slot> add ...`,
   `git -C <slot> commit`), or remove what you don't. If HEAD is on the task's branch
   `hrn/<id>`, the commits are kept there. If HEAD is detached, name the commits:
   `git -C <slot> branch <name>`.
4. With any remote, whatever its name: detach the slot at a commit a remote-tracking
   branch holds. Free counts a commit at HEAD as unpushed when no remote-tracking branch
   holds it, and a local branch does not change that, so it would still refuse. Use the
   remote's default branch: `git -C <slot> switch --detach origin/main`, or
   `upstream/main` when the remote is `upstream`. `git -C <slot> branch -r` lists the
   remote-tracking branches; `git -C <slot> remote show <remote>` names the default.
   If `branch -r` shows no branch of that remote, it was never fetched: run
   `git -C <slot> fetch <remote>` first, then detach at `<remote>/<default>`. Only with no remote at all: skip this step. Free then counts only the commits at HEAD
   that no local branch or tag holds, and step 3 left none, so HEAD can stay where it is.
5. `hrn ws free <slot>`. It detaches HEAD where it is and removes the lease; the work stays
   on its branch. Merge or push the branch that now holds the work yourself, from the
   repository: `git merge <name>` when step 3 made a branch for detached commits,
   `git merge hrn/<id>` when the work was committed on the task's branch. `hrn batch merge`
   merges only tasks whose status is `pass`, even when you name them, and a task whose slot
   was kept usually is not (a failed commit ends as `error`). Use `hrn batch merge` only
   when `hrn batch show` reports the task passed.

## List

`hrn ws list [--json]` prints one row per slot folder or lease: the number, the path,
`free`, `taken` or `stale`, the holder, since when, the branch (or the detached commit),
and `clean` or what is in it. A lease without a folder and a folder without a lease
both show.

## Issue leases

`hrn run --issue REF`, the MCP `run` tool's `issue` and a batch task's `issue` lease a GitHub
issue to one run, so two runs never work on the same issue. REF is `owner/name#N`,
`https://github.com/owner/name/issues/N`, or `#N` (or `N`) for the repository of the
workspace's `origin` remote. Owner and name are lower-cased, as GitHub ignores their case.

- **Where.** `~/.hrn/leases/issues/<owner>__<name>__<N>.lease` (under `$HRN_HOME` when set), so
  every checkout of the repository on this computer sees the same lease. The file is a slot
  lease with `issue`, `run` (the run id, or `<batch id>/<task id>`), `backend`, once
  known, `pr`, and `retired` while a Released comment is owed. It is taken after the run record exists and before the agent starts.
- **Refusal.** A lease held by a live process refuses the take, even one of the same `hrn
  mcp` server: two runs are two holders. `hrn run` exits 2 and the MCP tool returns a tool
  error, naming the holder, its run, backend, branch and age; the run record is closed as an
  error. `--take-over` (`take_over` in a spec or MCP call) replaces the holder and notes whom.
  A lease whose process exited, or whose process id now names another process (the same start
  identity check as slots), is stale and taken over by the next take.
- **GitHub.** A take also asks GitHub through `gh`: an open pull request whose closing
  references or body (`Fixes #N`, `closes: owner/name#N`, the issue's URL, with any of
  close/closes/closed/fix/fixes/fixed/resolve/resolves/resolved) close the issue, other than
  one from the run's own branch, refuses it like a live holder; so does a live claim comment
  by another worker. A claim is the first line ``Claim: <worker> on branch `<branch>` ``, and
  a later `Released: <worker>` closes it. Only comments by the account gh is logged in as
  count: every worker posts as that account, and anyone can comment on a public issue. It is stale after 24 hours when its branch has had no
  commit in 24 hours or does not exist. When the issue is free hrn adds the `claimed` label
  (created if missing) and comments ``Claim: hrn <run> (<backend>) on branch `<branch>` ``,
  plus `Takes over from <holder>.` on a takeover. These formats are shared with a claim tool
  outside hrn. A bare mention of the issue never holds it.
- **Replaced holders.** When a take replaces a lease (its process ended, its pull request is
  merged or closed, or `--take-over`), the replaced worker's claim does not hold the issue: hrn
  comments, when that claim is open, `Released: <old worker>`, a blank line and why (`its process
  ended`, `pull request <url> is merged or closed`, `taken over by <worker>`). A dead or closed
  holder is released as soon as the claims are read, even if the take is then refused; a live
  holder only after hrn's own claim. A takeover releases other workers' live claims the same way.
  A Released comment that fails is owed: its worker goes into the new lease's `retired` list,
  the run's release posts it first (`retired by <worker>`) and keeps the lease if it fails
  again, and a take that replaces the lease releases those workers too. A refused take that
  owes one puts the replaced lease back.
- **Two claims at once.** After commenting, hrn reads the claims again; the oldest open claim
  that is not stale or replaced wins. If that is another worker's, hrn comments
  `Released: <worker>`, a blank line and `claimed at the same time as <winner>`, gives the
  local lease back and refuses the take naming the winner. If that comment fails, the lease
  stays until the process ends, so the next take releases the claim.
- **gh failing** (not installed, not logged in, not a GitHub repository) keeps the local
  lease and prints `GitHub claim skipped: <reason>`; the run goes on. With no branch (a
  detached `HEAD`, or not a git checkout) hrn keeps the local lease and prints
  `GitHub claim skipped: no branch (detached HEAD or not a git checkout)`; a branch with a
  backtick or a control character, which the Claim line cannot carry, is skipped the same way.
  Either way a replaced dead holder's release is owed, since its claim may be open.
- **Release.** When the run (or batch task) ends, an open pull request from its branch keeps
  the issue: the lease is rewritten without a process and with `pr` set, so it outlives hrn.
  When gh finds none and hrn claimed the issue, it comments `Released: <worker>`, a blank line
  and the reason (`run <id> ended`), then removes the lease and the label when no other claim
  is open; if that comment fails the lease stays for the next take to release. When that lookup fails, the lease and the claim stay and the
  error is printed: the lease goes stale when the process ends, and the next take releases it.
- **Held by a pull request.** That lease is checked when a take or `hrn ws issues` reads it
  (`gh pr view <url> --json state`): merged or closed makes it free (listed as `released`,
  removed by the next take, which also releases its claim); an open pull request, or gh
  failing, keeps it held. Nothing polls.

`hrn ws issues [--json]` lists every issue lease: `held`, `held by pull request`, `stale` (and
why) or `released`, with holder, run, backend, branch, since and pull request. It removes
nothing and asks GitHub only for the state of a holding pull request. `--free owner/name#N`
removes one by hand, refusing while a running process holds it. `hrn flow`, the MCP `flow` tool
and the `hrn watch` flow page show a line per issue its run or batch holds (also a lease kept
under the first id of a run that switched backend and carried on, up to 20 steps back and only
through runs the viewer may read), read from disk alone and redacted and filtered like the rest
of the flow.
