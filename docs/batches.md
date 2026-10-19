# Batches and worktree cleanup

[← README](../README.md)

## Batches

`hrn batch tasks.json` runs every task in the file against one repository, `parallel`
agents at a time, each in its own git worktree on its own branch. After an agent
finishes, the gate commands run in that worktree; a task whose run or gates fail is
retried on the next backend in `retry`. Every attempt is an ordinary recorded run.

```json
{
  "parallel": 3,
  "backend": "claude",
  "mode": "yolo",
  "gate": ["go build ./...", "go test ./..."],
  "retry": ["opencode"],
  "tasks": [
    {"id": "fix-add", "prompt": "add() in math.go subtracts. Fix it and add a test."},
    {"prompt": "Make list_files skip .venv directories.", "backend": "api", "gate": ["go test ./internal/tools"]}
  ]
}
```

`dir` (default: the file's directory) is any path inside the repository. Task fields
override the file's defaults of the same name; `id` defaults to `t1`, `t2`, …. A task's
`"gate": []` is refused: leave `gate` out to use the spec's gates, or move the gates onto
the tasks that need them (an answer-only task then has none), so a record reads as the run ran.

Every shared run option (`agent.Options`) is a spec field under the same name with
underscores (`--max-turns` is `max_turns`, `--max-cost` is `max_cost_usd`), on the spec
or on a task; the MCP `run` tool takes the same names. They are one struct in the code
(`agent.Options`), so the three cannot drift apart. A task's value replaces the spec's;
a switch a task turns on stays on, except `lean` and `skills`: lean is on by default for
tasks on claude and codex and a task can set it to `false` (see
[What a run loads](backends.md#what-a-run-loads)); a task's own `mcp` list replaces the spec's, and an
empty list (`"mcp": []`) means no servers at all, not even `~/.hrn/mcp.json`. Every list
is checked before the first task starts.

Each task may also set `purpose`: `review`, `debug`, `implement`, `refactor`,
`test`, `research`, `plan` or `ops`. It is a task field, not a spec-wide default.
An invalid value is refused before any task starts. If omitted or empty, a valid
`Purpose: <category>` header on the first non-blank prompt line supplies it (see
[Declared purpose](commands.md#declared-purpose)). The effective declaration is
saved on the batch task result and each attempt's `agent.Task`, without changing
the prompt.

When writing implementation prompts for this repository, include the comment rule in
one line: "Comments say why, not what: one line, three at most; no issue numbers,
dates or review history in code."

For each task hrn creates branch `hrn/<id>` from the current `HEAD` (uncommitted changes
in the main worktree are not included) and gives it a worktree (see below), runs the
agent there, then the gates in order, stopping at the first non-zero exit or after 15
minutes. Whatever the agent left is committed on the branch as `<id>: <prompt>`, pass or
fail, so the work is on the branch whatever happens to the worktree; if that commit fails
(no git identity, a pre-commit hook) the task ends as `error` and the work stays
uncommitted in the worktree. An agent that commits its own work on the branch and leaves nothing
uncommitted gets no extra commit; the task records the branch's tip, so `diff` and merge see its
work. hrn reads the branch's tip and `HEAD` before and after the agent runs: when the agent
moved either itself (a commit of all or part of its work, a reset, a rename, a commit on another
branch or on none), the attempt records `agent_committed` and `hrn batch show` marks the task
`agent committed`. A commit the agent made on no branch (a detached `HEAD`) is not
recorded as the task's commit, since a merge would not bring it. Nor is a tip that no longer contains the base (the agent reset or
rewrote the branch below it), since merging it would revert the base; the attempt's error
says so. hrn writes `.hrn/.gitignore` so the worktrees never show up
in the repository's status.

### Slots or fresh worktrees

By default (`"worktrees": "slots"`) a task runs in one of the repository's reusable
[slots](workspaces.md), `<main checkout>/.hrn/slots/<n>`, so a `target/` or
`node_modules/` built by an earlier task is still there and the gates build only what
changed. Each running task leases a slot of its own, holding it with a lease that names
the hrn process. Its slot bound is `max(parallel, 5)`, capped at 10, counting
running tasks' and kept slots (below) together. It tries the slot
numbers from 1 that hold that many slots other than sessions' (`hrn ws take`,
a lease without a pid, held until `hrn ws free`), worked out again at every
try: with a five-slot bound, a session on slot 2 makes it try 1 and 3 to 6,
one on slot 10 changes nothing, and a session that takes a slot in the range
just as a task tries it widens the range at once (up to three times in a row;
a fourth session arriving in that instant fails the task rather than retrying
for ever). The range never passes slot 10; when no slot through 10 can be used,
the task waits or fails with the repository limit named. So a batch never
waits on a session, and its own slots stay bounded whatever the sessions do.
The task's branch is
made from the batch's base and checked out in the slot. When the task ends, pass, fail or
error, the slot is detached at the base and given back. Its ignored files stay for the
next task, and the work stays on branch `hrn/<id>`, which `diff`, `merge` and `rm` read.
A failed task's line reads `branch hrn/<id> kept` rather than naming a folder, and the
record names the slot each task ran in (`worktree` and `slot`).

When every slot it tries is taken by another batch, or by this batch's own running
tasks once it holds as many as it may, a task does not fail:
it says it is waiting and which slots are held, looks again every 5 seconds, and is
canceled with the batch. A lease it cannot read keeps it waiting too, since one being
written reads on the next look. When such leases are all it is waiting for and every one has
stayed unreadable for 30 seconds (an empty or broken lease file, or a `<n>.lease.takeover`
file a crash left), no process will free them, so the task fails as `error` naming each file.
Check the slots with `hrn ws list`. A lease that stays unreadable, or a left `.takeover` file,
may be removed only while no hrn command at all runs on this repository, no `hrn ws` and no
`hrn batch`. A live take creates its lease empty and writes it next, before anything works in
the slot, so removing an empty lease lets a second take have the slot while the first goes on.
A live `hrn ws free` or takeover holds the `.takeover` file while the lease it guards may name
a dead process, and removing it lets another take in, whose fresh lease the free then removes.
Check with Task Manager or `tasklist | findstr hrn` on Windows, `pgrep hrn` elsewhere. When none can come free, because the rest are sessions', skipped
for what is in them or kept by this batch's own tasks (below), it fails as `error` instead. A slot that still holds files when its task ends (the commit
failed, say), or a commit on no branch (the agent detached HEAD and committed), is not
given back: the task's error says `slot <n> kept` and why. Its lease
goes stale once hrn exits, and later takes skip the slot without cleaning it. To keep its
work and free it, see [A slot a batch kept](workspaces.md#a-slot-a-batch-kept).

`"worktrees": "fresh"` keeps the older behaviour, for a build from nothing: each task gets
a new worktree `.hrn/worktrees/<id>`, kept after the batch until `hrn batch rm`. Any other
value is refused before anything runs. `hrn eval` uses fresh worktrees unless its spec
says otherwise, because its judge reads each cell's worktree after the batch; it refuses
`slots` together with a judge.

A task can also set `expect`, a regular expression (Go RE2) its final answer must match.
It is checked before the gates and reported as one, `expect /pattern/`, so a task whose
answer is the deliverable can fail on what it said: `"expect": "(?i)^no\\b"` for "is
there a race here? answer yes or no". A retry after a miss is shown the answer it gave.

Gates run in the worktree the agent just edited, so an agent can make them pass by
weakening the tests. `protect` names the files the gates rely on, as globs relative to
the repository in which `**` stands for any number of folders:
`"protect": ["**/*_test.go", "scripts/check.sh"]`. After an attempt's run ends `ok`, hrn
compares those files with the batch's base, committed or not; a file the agent added is
not a change, a modified or removed one is. The attempt's record lists them under
`protected` and the batch shows `! protected files changed: ...`. `protect_mode` says what
that means: `report` (the default) keeps the verdict, `fail` fails the attempt before its
gates run, as a gate named `protect ...` whose output lists the files, so a retry is told
which ones to restore. A task's `protect` and `protect_mode` replace the spec's. Evals
default to `fail`.

A failed attempt is retried on each backend named in `retry`, in order. The same name
may repeat, and `"retry": ["claude"]` after `"backend": "claude"` is the common case: a
second try with the same model, told what went wrong. A retry on a backend other than
the first attempt's drops the model and effort named or routed for that first backend,
and its record carries no `routed_by`. How the retry starts depends on how the attempt
failed.

With retrieval enabled, a same-scope retry after a failed gate searches again using
bounded failure evidence and the first attempt's pinned knowledge version. A change
of backend, model or retrieval scope is refused rather than silently repinned: a spec
or task with retrieval on whose `retry` names a backend other than the task's is refused
when the batch starts. When an attempt reaches its usage limit, or the first attempt or a
retry would move to the partner because the backend's limit is remembered, the task ends
there (status `fail`, its error saying the switch to the partner was skipped because
retrieval is bound to the task's backend) and nothing runs on the partner. A retry after required
retrieval failed before anything was pinned (the control service was unreachable, say)
searches afresh; once a pin is recorded, every retry honours it.

| Previous attempt | Next attempt |
|---|---|
| ran to the end, a gate failed | continues in the same worktree, on top of the work already committed, with the failing gate command, its exit code and output appended to the prompt |
| stopped at its plan's usage limit | continues in the same worktree, told that the limit stopped it and shown its last message |
| failed, refused or was cut short | starts afresh from the base commit (a new worktree, or the same slot with its branch reset to the base; the earlier attempt's commit leaves the branch, as a removed worktree's branch did; git's reflog still has it for a while), with one line saying how the previous attempt ended |

Backend preferences apply to both the spec's and each task's `backend` and `retry`.
A disabled primary backend is refused. Retry filtering preserves order and repeated
names, removes disabled entries, and refuses a nonempty list that becomes empty.
An explicit empty task retry list still overrides the spec's retries. See
[Backend preferences](backends.md#choosing-which-backends-hrn-may-use).

An attempt that ends `limited` is carried on by its ready enabled partner first, as
an extra attempt that does not use up a `retry` entry, and an attempt meant for a backend
whose limit is remembered starts on that partner (see [When a plan's usage limit is
reached](backends.md#when-a-plans-usage-limit-is-reached)). When both are limited, the attempt is
skipped, and a task with nothing left to run fails without starting an agent, its error
saying why. `no_switch` turns this off.

A continued attempt commits as `<id>: <prompt> (attempt N)`, so the branch reads as a
sequence of fixes. Each attempt records the prompt it was actually sent, whether it
continued, and `switched_from`, the backend a usage limit moved it from, which `hrn batch
--json` prints.

`hrn batch` refuses to start when tracked files in the repository are modified: every
worktree branches from `HEAD`, so that work would not reach the agents and the batch
would quietly be testing something else. Commit or stash first, or pass `--allow-dirty`
(spec field `allow_dirty`). Untracked files are not counted.

Nothing can answer approval prompts in a batch, so `mode` defaults to `auto` (file edits
allowed, no commands). Use `yolo` when the agent must build or run tests itself; the
gates run either way. Backend names, gate commands, durations and task budgets are all
checked before anything runs.

The [run budgets](commands.md#budgets) are spec fields too (`timeout`, `max_cost_usd`, `task_budget`), and
they apply per attempt rather than per task, so a retry gets its own allowance. A task
may set its own value or change the spec's, but zero means inherit, so it cannot remove
a limit the spec set.

Each gate command gets 15 minutes before it is killed with its whole process tree;
`gate_timeout` (e.g. `"90s"`, `"45m"`, at most `24h`) changes that on the spec or on one
task, so a slow integration suite and a quick lint can share a batch. A killed gate fails
with `[timed out after …]` in its output, and its record says why it was killed (`killed`,
`"timeout"` with the `timeout` that applied, or `"canceled"` when the batch was
interrupted, even one that could not start because of it), so every report of it says
`killed after 15m0s (gate_timeout; set "gate_timeout" in the spec to allow more)` or
`canceled` rather than `exited -1`. The watch page's badge keeps that short (`killed ·
gate_timeout 15m0s`, the full text as its tooltip), and a retry is told the gate `was
killed after 15m0s by gate_timeout` without the advice, which is the operator's, so it
does not search the output for a failing test.

If the gate shell cannot start, the task fails with an infrastructure reason and
does not use any remaining `retry` backends. This covers a process start error
(including a missing shell interpreter), or a nonzero exit whose output was decoded
from UTF-16 and contains an `HCS_E_*` error code or `The Windows Subsystem for Linux`
launcher diagnostic within the first 1 KiB of decoded output.
The gate records the reason in `infrastructure_error`; the agent's work is still
committed for inspection. Other nonzero exits, including a command not found
inside a running shell, keep the normal retry behavior. Cancellation and
`gate_timeout` keep the behavior described above.

Gate output is decoded from UTF-16 to UTF-8 before it is checked, shortened,
recorded or shown. A little- or big-endian BOM identifies UTF-16; without a BOM,
alternating NUL bytes over the first few code units identify the byte order.

The record goes to `~/.hrn/batches/<id>.json`, written before the first task starts,
again each time a task moves on (an attempt's run starts or ends, a gate finishes, a task
ends) and at the end, so `hrn watch` and `hrn batch show` can follow it; `--json` prints
it, `-v` streams every agent's tool calls. Its `ownership` is 1 in records hrn writes now: a
fresh task owns its worktree and branch only when its `branch_made` says so (older records
lack the field and still load). Ctrl+C
cancels running agents and skips the rest. Exit status is 1 unless every task passed.

### Reading and landing the work

```
hrn batch show 20260922-164137-1a216b     # the same table, during the run or after it
hrn batch diff 20260922-164137-1a216b alpha
hrn batch merge 20260922-164137-1a216b
```

`show` reprints the end-of-run table from the record; a batch still going shows its
tasks as `running` and says how many. The record is written before the first task and
renamed into place, so reading it mid-run gets a whole file, never half of one.

`diff` writes one task's changes to stdout and nothing else, so it pipes into `git
apply` or a pager: the commit against the base the batch branched from, or whatever is
still uncommitted in the worktree when the task never committed. Files git has never
seen cannot appear in a diff, so they are listed on stderr instead. A slot task that
committed nothing has an empty diff, and `diff` says it committed nothing: its slot has
gone back and may hold another task's work by now, or, when the task kept it (`slot <n>
kept`), `diff` says so and the work is still in it (`hrn ws list`).

`merge` merges the branches into whatever the repository has checked out, in spec
order, and never pushes. It takes every task that passed, or the ones you name; a task
that failed its gates is skipped even when named, so nothing unproven arrives by
accident. The tree must be clean first, since the merge would otherwise mix your work
with the agents'. At the first conflict it puts the tree back as it found it, says
which task, and stops — the branches are still there to merge by hand. `--squash`
records one commit per task, `<id>: <prompt>`, instead of a merge commit. Undo a whole
call with the printed `git reset --hard <commit>` command. It names the commit
from before the call and undoes every task merged in it, even if a later task conflicts.

`merge --rm` removes the worktree and branch of each task merged in that call,
including tasks merged before a later conflict; skipped and conflicting tasks stay.
It uses the same rules as `batch rm`: dirty worktrees stay and cause an error,
and slot tasks lose only their branch. The printed undo command names the commit
from before the merge; after removal, the task work is only in the reflog. If
removal fails for one task, the output counts only tasks whose worktree or branch
was removed.

`hrn batch rm <id>` removes what a batch made; the record and the run logs stay. For a
slot task that is only its branch `hrn/<id>`: the slot and its lease are not the batch's,
and stay. So is it for a task the record still shows waiting for a slot (hrn stopped
then), whose branch is made before the wait. It refuses while the branch is checked out in a worktree (a task still running).
For a fresh task it removes the worktree and the branch (it refuses a worktree with
uncommitted changes) when the record says the task made them (`branch_made`, set once
git made both, and dropped while a retry makes them again; a task whose record cannot say
so removes them at once, and one whose removal fails claims what is still there): a task that found them there
already, or that hrn left before it had them back, does not own them (they may be another
batch's), and they stay. Records from before the record's `ownership` field never said so:
there every fresh task's worktree goes, except one whose task failed before any attempt
with hrn's own "already exists; remove it or pick another task id" error. A directory git no longer counts as a worktree (on Windows a file
lock can leave one behind) is removed when it is empty and reported when it is not, and
its branch is deleted either way. One task that cannot be cleaned does not stop the
others.

## Clearing merged worktrees

```
hrn gc E:\work            # what would go, and why each worktree stays or goes
hrn gc --apply E:\work    # remove them
```

Worktrees outlive their pull requests: the session that made one rarely comes back, and with
a Rust `target/` or a `node_modules` inside, each can hold gigabytes. `hrn gc` looks at every
linked worktree of the repositories at PATH (a repository, any folder inside one, or a folder
whose repositories are found two levels down; the current directory by default) and removes a
worktree only on proof that its work is merged:

- **On a branch:** GitHub (`gh pr list`) reports no open pull request for the branch, and one
  merged at the worktree's commit, or at a later commit that contains it, more than `--grace`
  ago (default 1h, so a session finishing up after its merge keeps its folder).
- **Made by an hrn batch** (a batch record names the folder): the batch has no task still
  running, and the worktree's commit is on the branch origin names as its default
  (`git ls-remote --symref`), fetched by name just before. A squashed or reworded landing
  is also recognised when the patch id of the task's whole diff, whitespace included but
  line numbers ignored, exactly matches one commit on that branch since their merge base.
  The comparison checks fewer than 2000 non-merge commits; if it reaches that bound, or
  cannot prove an exact match, the worktree stays. The branch's patch ids are read once per
  merge base in a run, streamed from `git log` so large binaries are not held in memory.
  Git older than 2.40 does not support `patch-id --verbatim` and keeps these worktrees. With `--apply`, hrn deletes the task's
  branch with its worktree, including its own commits and messages; the change itself is
  on the default branch.

And only when nothing would be lost with it: the tree is clean (untracked files count), its
ignored files are build output (`target`, `node_modules`, `dist`, `.venv` and the like, or any
folder carrying `CACHEDIR.TAG`, with no repository and no link (symlink or junction) out of the
worktree inside, on every system, since git on Windows deletes through a junction; an ignored `.env` or notes keep it, because `git
worktree remove` deletes ignored files; its `.hrn/` may hold only the worktrees git lists there), it is
not locked (so a slot always stays, even one a batch record names), it holds no worktree that
stays and no separate repository (submodules are checked below), on Windows it has no junction
or directory symlink leading out of it anywhere in it (git deletes through those; on Linux and
macOS it unlinks a link, so a tracked `config -> ../shared/config` does not keep it, and a
tracked folder swapped for a link already shows as a change),
and the current directory is not inside it. Everything else stays and the listing says why: an open or
closed pull request, none at all, commits after the merged one, a detached `HEAD`, uncommitted
files. When `gh` fails (not installed, not logged in) or the fetch does, the worktrees that
needed it stay, and while any batch record cannot be read every worktree stays (it may be the
record of a batch still running). Main checkouts are never touched.

The listing puts the removals first, with the size each one frees and the pull request's URL,
which is the record once the folder is gone. `--apply` checks each one again just before it goes
(its `HEAD`, branch, lock and files, ignored ones included) and leaves one that changed since the
scan, or that holds a worktree that stays, then removes it with `git worktree remove` (never
`--force`) and deletes the branch if it still points at the commit that was checked. git's
removal is not all-or-nothing: one that fails (a file held open on Windows) can leave part of the
folder deleted. Only merged, clean worktrees get that far. If git no longer lists the worktree
and its folder is empty or gone, hrn prunes git's entries, tries to remove the empty folder, and
deletes the branch; a handle that still prevents deleting the empty folder is reported.
Otherwise the branch stays. When git refuses removal because the worktree contains submodules,
hrn checks their trees recursively (including untracked and ignored files). Each submodule's
HEAD must match the commit its parent recorded, it must have no stash, and HEAD and local
branches must have no commits absent from its remote-tracking refs. Deinitialized submodules
keep the worktree because their repositories cannot be checked by this pass, and so does a
submodule repository with linked worktrees of its own (they would lose their repository). If
all checks pass, hrn moves the folder aside (a `.gc-*` sibling), prunes git's entries, deletes
the moved folder and the branch. A failed submodule check, or a folder that cannot be moved
(a file held open on Windows), keeps the worktree and branch whole; so does one git still
lists after the prune (locked meanwhile), which is moved back. A failure partway through
deleting the moved folder is reported with its path, which is left to delete by hand; the
worktree is no longer registered by then, so no later scan sees a half-deleted worktree.
A batch's record stays (`hrn runs prune` can drop
it once its worktrees are gone). Open pull requests are read on their own, so a long history
cannot hide one; with 1000 or more open, no worktree on a branch is removed. Up to 1000 pull
requests of every state are read besides; a branch whose pull request is older stays.

### Build caches

```
hrn gc --free-below 50G E:\work C:\Users\me\cargo-target
```

The same run looks for build caches: folders carrying a `CACHEDIR.TAG` with the convention's
signature, which Cargo writes into every target folder (pytest and ruff tag their caches the same
way), at each PATH and up to two levels down (a `.sec-target` beside its repository, each folder of
a `cargo-target` pool) and inside each worktree that stays, up to two levels down. A cache goes when no build holds it (Cargo keeps
`<profile>/.cargo-lock` open while it builds: on Windows an open that shares nothing fails, elsewhere
a non-blocking `flock` does) and nothing has written it for `--idle` (default 24h). Its last write is
the newest time among its profile folders (`debug`, `release`, one under a target triple), their
`deps`, `.fingerprint`, `build` and `incremental` folders, the files directly in a profile, and
anything the walk that sizes it sees inside. With `--free-below SIZE` (`50G`, `500M`), a drive with less
free than SIZE also loses its caches idle past `--min-idle` (default 2h), oldest first, until what
goes would put it above SIZE; free space is asked of the drive (`GetDiskFreeSpaceEx` on Windows,
`statfs` elsewhere), and a drive that cannot answer loses nothing more.
In scan reasons and the free-space report, drives are named by their mount point on Linux and macOS.
After `--apply`, each drive that was below SIZE in the scan reports its actual free space,
whether it reached SIZE or how far it remains below; the listing is still the full set of
removals for that run.

A cache stays when git tracks a file in it (a tagged test fixture is someone's), when it holds a
repository or a link leading out of it, when the current directory is inside it, or when its lock
cannot be checked. One inside a worktree that goes is not listed: the worktree's removal takes it.
The listing shows each cache's size, last build and verdict; `--apply` checks each one again just
before it goes (still tagged, nothing written since the scan, no lock held, nothing inside that must
stay), moves it aside to a sibling `.gc-<pid>-<unix nanos>-<name>` folder, probes its locks
again (including `flock` on Unix), then deletes its contents with `CACHEDIR.TAG` last. If a
build holds a moved cache's lock, it is moved back and kept. A failed move keeps the original
cache; a failed deletion leaves a tagged `.gc-*` folder for the next run. That leftover is
eligible regardless of `--idle`, after the other safety checks. A cache has no pull request to
stand as its record: the build that needs it again rebuilds it.
