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
override the file's defaults of the same name; `id` defaults to `t1`, `t2`, ….

Every option `hrn run` takes as a flag is a spec field under the same name with
underscores (`--max-turns` is `max_turns`, `--max-cost` is `max_cost_usd`), on the spec
or on a task; the MCP `run` tool takes the same names. They are one struct in the code
(`agent.Options`), so the three cannot drift apart. A task's value replaces the spec's;
a switch a task turns on stays on; a task's own `mcp` list replaces the spec's, and an
empty list (`"mcp": []`) means no servers at all, not even `~/.hrn/mcp.json`. Every list
is checked before the first task starts.

For each task hrn creates branch `hrn/<id>` from the current `HEAD` (uncommitted changes
in the main worktree are not included) and gives it a worktree (see below), runs the
agent there, then the gates in order, stopping at the first non-zero exit or after 15
minutes. Whatever the agent left is committed on the branch as `<id>: <prompt>`, pass or
fail, so the work is on the branch whatever happens to the worktree; if that commit fails
(no git identity, a pre-commit hook) the task ends as `error` and the work stays
uncommitted in the worktree. hrn writes `.hrn/.gitignore` so the worktrees never show up
in the repository's status.

### Slots or fresh worktrees

By default (`"worktrees": "slots"`) a task runs in one of the repository's reusable
[slots](workspaces.md), `<main checkout>/.hrn/slots/<n>`, so a `target/` or
`node_modules/` built by an earlier task is still there and the gates build only what
changed. Each running task leases a slot of its own, holding it with a lease that names
the hrn process. A batch holds at most `parallel` slots (at least 4) at once, running
tasks' and kept ones (below) together, and tries the slot numbers from 1 that hold that
many slots other than sessions' (`hrn ws take`, a lease without a pid, held until `hrn ws
free`), worked out again at every try: a session on slot 2 makes it try 1 and 3 to 5, one on
slot 10 changes nothing, and a session that takes a slot in the range just as a task tries
it widens the range at once (up to three times in a row; a fourth session arriving in that
instant fails the task rather than retrying for ever). So a batch never waits on a session, and its own slots stay
bounded whatever the sessions do. The task's branch is
made from the batch's base and checked out in the slot. When the task ends, pass, fail or
error, the slot is detached at the base and given back. Its ignored files stay for the
next task, and the work stays on branch `hrn/<id>`, which `diff`, `merge` and `rm` read.
A failed task's line reads `branch hrn/<id> kept` rather than naming a folder, and the
record names the slot each task ran in (`worktree` and `slot`).

When every slot it tries is taken by another batch, or by this batch's own running
tasks once it holds as many as it may, a task does not fail:
it says it is waiting and which slots are held, looks again every 5 seconds, and is
canceled with the batch. A lease it cannot read keeps it waiting too, since one being
written reads on the next look; one that stays unreadable keeps it waiting for ever, which
is [#193](https://github.com/cristiangirlea/harness/issues/193). When none can come free, because the rest are sessions', skipped
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

A failed attempt is retried on each backend named in `retry`, in order. The same name
may repeat, and `"retry": ["claude"]` after `"backend": "claude"` is the common case: a
second try with the same model, told what went wrong. How the retry starts depends on
how the attempt failed.

| Previous attempt | Next attempt |
|---|---|
| ran to the end, a gate failed | continues in the same worktree, on top of the work already committed, with the failing gate command, its exit code and output appended to the prompt |
| stopped at its plan's usage limit | continues in the same worktree, told that the limit stopped it and shown its last message |
| failed, refused or was cut short | starts afresh from the base commit (a new worktree, or the same slot with its branch reset to the base; the earlier attempt's commit leaves the branch, as a removed worktree's branch did; git's reflog still has it for a while), with one line saying how the previous attempt ended |

An attempt that ends `limited` on claude or codex is carried on by the other one first, as
an extra attempt that does not use up a `retry` entry, and an attempt meant for a backend
whose limit is remembered starts on the other one (see [When a plan's usage limit is
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
with `[timed out after …]` in its output, which a retry then sees.

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
run with `git reset --hard ORIG_HEAD`.

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
  (`git ls-remote --symref`), fetched by name just before.

And only when nothing would be lost with it: the tree is clean (untracked files count), its
ignored files are build output (`target`, `node_modules`, `dist`, `.venv` and the like, or any
folder carrying `CACHEDIR.TAG`, with no repository and no link out of the worktree inside, since
git on Windows deletes through a junction; an ignored `.env` or notes keep it, because `git
worktree remove` deletes ignored files; its `.hrn/` may hold only the worktrees git lists there), it is
not locked (so a slot always stays, even one a batch record names), it holds no worktree that stays and no repository, and the current directory is
not inside it. Everything else stays and the listing says why: an open or
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
folder deleted. Only merged, clean worktrees get that far, and the branch then stays. A batch's record stays (`hrn runs prune` can drop
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

A cache stays when git tracks a file in it (a tagged test fixture is someone's), when it holds a
repository or a link leading out of it, when the current directory is inside it, or when its lock
cannot be checked. One inside a worktree that goes is not listed: the worktree's removal takes it.
The listing shows each cache's size, last build and verdict; `--apply` checks each one again just
before it goes (still tagged, nothing written since the scan, no lock held, nothing inside that must
stay) and removes it whole. A cache has no pull request to stand as its record: the build that
needs it again rebuilds it.
