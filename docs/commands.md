# Command reference

[← README](../README.md)

## Commands

| Command | What it does |
|---|---|
| `hrn run [flags] [--purpose CATEGORY] <prompt>` | Run a task. Prompt from args, or stdin when omitted / `-`; optionally declare its purpose (see [Declared purpose](#declared-purpose)). |
| `hrn exec --protocol v1` | Run one recorded task over the supervisor JSONL protocol (see [hrn exec](#hrn-exec)). |
| `hrn backends` | Show enabled/disabled, default, availability and remembered limits (see [Backend preferences](backends.md#choosing-which-backends-hrn-may-use)). |
| `hrn backends set --only NAMES [--default NAME]` | Replace the preference with comma-separated registered backend names in preference order; the optional default must be among them. |
| `hrn backends reset` | Remove the preference file and restore unrestricted backend selection. |
| `hrn remote submit --tenant ID --project ID --base-ref COMMIT --key KEY --max-cost USD [--max-turns N] <prompt>` | Queue a read-only API job on a registered project (see [Remote development jobs](#remote-development-jobs)). |
| `hrn remote show --tenant ID --project ID JOB_ID` | Show a remote job's state and result. |
| `hrn remote events --tenant ID --project ID [--after EVENT_ID] [--watch] JOB_ID` | Read up to 100 events, or stream until the job ends and resume from the last durable event ID after a disconnect. |
| `hrn remote cancel --tenant ID --project ID JOB_ID` | Request cancellation of a remote job. |
| `hrn knowledge import\|search\|inspect\|active\|activate\|disable\|rollback request.json` | Manage approved project knowledge through the control service (see [Project knowledge](#project-knowledge)). |
| `hrn retrieval fixture [--json]` | Compare bundled synthetic accounting samples without a model call; the fixture proves no real savings. |
| `hrn retrieval compare --manifest FILE --samples FILE [--baseline-preparation FILE] [--candidate-preparation FILE] [--reuse N] [--runs] [--json]` | Compare saved retrieval evidence and verify measured run references locally, without a model call. Match each run on the requested model in `task.json`; a run without a requested model matches only when its reported result model exactly equals the variant model. Text output shows a differing provider-reported model. |
| `hrn version` | Print the hrn version, the Go version, and the commit it was built from (a `go install` build shows its module version instead; Go does not stamp builds made inside a `git worktree`). |
| `hrn update [--main \| --ref REF \| --file PATH] [--dry-run] [--rollback]` | Replace every hrn copy on this computer with the newest release, a build of a ref or a file, each under a backup, and restart a running `hrn watch`; `--rollback` puts the backups back (see [hrn update](#hrn-update)). |
| `hrn runs [-n N]` | List recent runs. |
| `hrn runs prune --older-than 30d [--keep N] [--dry-run]` | Remove old runs, batch records and eval records (see [Run records](run-records.md#run-records)). |
| `hrn show [--json] <id>` | Replay a recorded run. |
| `hrn show -f <id>` | Follow a run as it happens, until it ends (see [Watching runs live](watch.md#watching-runs-live)). |
| `hrn show --usage [--json] <id>` | A run's model calls, one row each: tokens, the context each carried, the cache reads so far, and compactions recorded or probable (see [Usage per turn](run-records.md#usage-per-turn)). |
| `hrn watch [--addr HOST:PORT] [--lan[=NAME]] [--public https://HOST --access-team TEAM.cloudflareaccess.com --access-aud TAG]` | Follow runs and batches live and view the [Backends card](watch.md#backends); loopback and verified public viewers can change backend preferences. Public mode keeps the loopback listener and verifies Cloudflare Access on every request; it cannot be combined with `--lan` (see [Behind Cloudflare Access](watch.md#behind-cloudflare-access)). |
| `hrn mcp` | Serve hrn as an MCP server on stdio (see [MCP](mcp.md#mcp)). |
| `hrn batch [flags] <tasks.json>` | Run tasks in parallel git worktrees with gates and retries; each task may declare `purpose` (see [Batches](batches.md#batches)). |
| `hrn batch show [--json] <id>` | Reprint a batch's table and attempt notes, while it runs or long afterwards. |
| `hrn batch diff <id> <task-id>` | What one task changed, as a diff on stdout. |
| `hrn batch merge [--squash] [--rm] <id> [task-id…]` | Merge passed tasks; `--rm` removes each task's worktree and branch only when it merged in this call. |
| `hrn batch rm <id>` | Remove what a batch created: its branches, and its worktrees when they were fresh (a slot stays; see [Batches](batches.md#reading-and-landing-the-work)). |
| `hrn eval [flags] <eval.json>` | Run a task set across a matrix of backends, models and efforts (see [Evals](evaluations.md#evals)). |
| `hrn eval show <id>` | Reprint a saved eval. |
| `hrn eval list [-n N]` | List saved evals, newest first: cells, pass rate, cost with the judge (`-` when none is known; cells without a cost are counted). |
| `hrn eval diff [--json] <a> <b>` | Two evals side by side, variant by variant and task by task. |
| `hrn eval setup [--base REF] [--paths P,P] [--held-out SHARE --seed N] [--keep] [--json] [-v] <eval.json>` | Run the spec's one variant twice on this commit's code, with the base's setup files and with the branch's, and say whether the setup change helped (see [Comparing a setup change](evaluations.md#comparing-a-setup-change)). |
| `hrn route show` | List purpose routing rows, or `(empty)` (see [Routing by evidence](evaluations.md#routing-by-evidence)). |
| `hrn route set <purpose> [-b NAME] [-m MODEL] [--effort LEVEL]` | Replace one routing row by hand; accepts any nonempty subset of the three choices. Warns when the backend is disabled (runs ignore the row until it is enabled). |
| `hrn route from-eval <id> [--margin 0.05]` | Print and save the cheapest qualifying rows from saved eval results. |
| `hrn report [--since 7d] [--by backend\|model\|dir\|purpose\|routed\|day] [--json]` | Tables over the finished runs, their tokens and context per turn, and the batches; `routed` groups by the source routing row (see [Reports](evaluations.md#reports)). |
| `hrn gc [--apply] [--grace 1h] [--idle 24h] [--min-idle 2h] [--free-below SIZE] [PATH…]` | Remove worktrees whose work is merged, with their build output, and build caches (`CACHEDIR.TAG`) idle past `--idle`, or past `--min-idle` on a drive below `--free-below` (see [Clearing merged worktrees](batches.md#clearing-merged-worktrees) and [Build caches](batches.md#build-caches)). |
| `hrn ws take [--repo DIR] [--branch NAME] [--base REF] [--for TEXT] [--max N] [--no-fetch]` | Lease a reusable worktree of the repository and print only its path, on `--branch` or detached at origin's default branch (see [Reusable workspaces](workspaces.md#take)). |
| `hrn ws free [PATH]` | Give the slot back once its work is pushed: HEAD detached, lease removed (see [Free](workspaces.md#free)). |
| `hrn ws list [--repo DIR] [--json]` | The slots: who holds each, since when, its branch and what is in it (see [List](workspaces.md#list)). |
| `hrn scripts hint` | A Claude Code PreToolUse hook that reads the payload on stdin and names the kept script whose `matches:` header covers the inline script a command is about to run; advice only, silent otherwise (see [The hint hook](scripts.md#the-hint-hook)). |

Run flags: `-b/--backend` (api, claude, codex, gemini, opencode, agy), `-m/--model`, `-C/--dir`, `--mode`, `--max-turns`,
`--effort` (api, codex, claude and agy: low, medium, high, xhigh, max; unset uses the
backend default; gemini and opencode report it as not enforced),
`--json` (JSONL event stream for scripts), `--thinking`,
`--mcp FILE` (repeatable; codex gets them as `-c mcp_servers` overrides), `--no-instructions`,
`--lean=false` (claude and codex are lean by default: only the run's `--mcp` servers, and on
claude no skills; false loads everything), `--skills` (claude: keep skills and slash commands in
a lean run; see [What a run loads](backends.md#what-a-run-loads)),
`--compact-window N` (claude: compact at N tokens, 100000 to 1000000, or `auto`; see
[Compaction window](backends.md#compaction-window)),
`--append-system-prompt TEXT` (api and claude: added after the backend's own system prompt,
at most 16 KiB), `--no-skills` (claude: skills off; see
[Harness variants](evaluations.md#harness-variants)),
`--allow-dangerous`, `--no-switch` (see
[When a plan's usage limit is reached](backends.md#when-a-plans-usage-limit-is-reached)), and the
budgets below.
`--shape-tools` opts the api backend into summaries of large terminal, log and
test-runner results from shell and MCP tools, with the full original saved behind
a handle (see [Run records](run-records.md#run-records)). It is off by default
pending the #296 eval; JSON and tabular summaries are deferred. Summaries must
preserve failing tests and error lines and introduce no numbers, negations or
paths absent from the original; a rejected summary uses the existing cut. This
includes generated counts, so a count absent from the original also rejects the
summary. Failing-test diagnostics must stay with their test name, in order;
ambiguous interleaved diagnostics reject the summary and use the existing cut.
Sequential subtests are nested, not interleaved: a diagnostic after `=== RUN TestA/sub`
belongs to that subtest while its parent waits. Go before 1.20 prints no `=== NAME`
marker when the parent logs again after a subtest, so that parent diagnostic is kept with the
subtest; if the subtest passed, the summary drops it and only `read_original` shows it.
`--shape-tools=false` keeps the existing behavior. The additive `shape_tools`
boolean is also available in run options, batch specs/tasks and MCP
run inputs. Eval matrix variants each select `shape_tools` (default false),
overriding task/spec shaping so a suite can compare raw and shaped variants;
give the variants distinct names. This flag does not change CLI backend tools.
`--retrieval off|optional|required` opts into project references; enabled modes
require `--retrieval-tenant`, `--retrieval-project`, `--retrieval-revision` and
an explicit `--model`. `--retrieval-version` pins a version; otherwise the
active version is resolved once and saved with the run. A pin stays with its
backend: `--continue` with `--retrieval optional|required` on another backend than
the chain's latest pin is refused with exit 2 before anything runs, naming the
pinned run and its version. Continue on the pin's backend, start a fresh run, or
continue with retrieval off to hand the transcript over (see
[Continuing a run](backends.md#continuing-a-run)).

### Declared purpose

`hrn run --purpose CATEGORY` declares what a run is for: `review`, `debug`,
`implement`, `refactor`, `test`, `research`, `plan` or `ops`. An invalid flag value
is refused with exit 2 before the run starts. The MCP `run` tool takes the same
optional `purpose` string, and each task in a CLI or MCP batch spec may set it;
invalid values are refused before any task runs. Flag and MCP/batch field values
are case-sensitive and must use the lowercase spellings above (`Review` is refused).

Without an explicit value, a `Purpose: <category>` header on the prompt's first
non-blank line supplies the declaration. The header label and category are
case-insensitive; a non-category value, including free text or `unknown`, is
ignored as a declaration and is never an error; that header line is also excluded
from keyword classification. The explicit value takes
precedence over the header. The effective declaration is saved in `task.purpose`;
no prompt text is added, removed or changed. Watch and reports use it before
falling back to the keyword classifier. An invalid stored `task.purpose` is ignored,
falling back to a valid header or the classifier.

A continued run (`hrn run --continue`, or the MCP `run` tool with `continue_run_id`)
keeps the earlier run's declared purpose when neither `--purpose`/`purpose` nor a
header names one, as it keeps the mode; a short follow-up such as "now fix what you
found" is therefore not re-classified. `hrn exec` takes the same optional `purpose`
on `start` (see [the exec protocol](exec-protocol.md#start-caller-exactly-one-the-first-line)).
A batch task's declared purpose shows in `hrn batch show`'s table, the MCP
`batch_show` text and the task list on the watch batch page.

### hrn exec

`hrn exec --protocol v1` lets a supervising program start, follow, cancel and record
one run per process without parsing terminal output. stdin and stdout carry JSON
lines: hrn sends `hello` first, then accepts one `start`, streams complete events,
and ends with `result`. Closing stdin or interrupting hrn cancels the run. See
[the v1 protocol](exec-protocol.md) for messages, capabilities, limits and exit codes.

### hrn update

`hrn update` moves every hrn on this computer to another build in one step, the routine
that used to be done by hand after a merge.

**Sources.** Without a flag it takes the newest GitHub release of `cristiangirlea/harness`:
the asset for this platform (`hrn_<version>_<os>_<arch>[.exe]`, as `.github/release.sh`
names them) and `SHA256SUMS`, and refuses to go on when the checksum does not match. A
`GITHUB_TOKEN` in the environment is sent to the API when present and never printed.
`--ref REF` runs `go install github.com/cristiangirlea/harness/cmd/hrn@REF` into a temporary
`GOBIN` (with `GOFLAGS` unset and `GOPROXY` as configured); `--main` is `--ref main`; both
need Go on `PATH` and say so otherwise. `--file PATH` takes a binary already built, say the
one a session built in a scratch folder. Whatever the source, the candidate is run once with
`version` and nothing else, after the checksum. The command prints `installed:` and
`candidate:` lines and stops with `already current` when every target names the same commit or
module version as the candidate; otherwise a target that already does is listed as `already
current` and left alone (no backup, its watch not restarted), and the others are updated.

**Targets.** The running executable (resolved through symlinks) and every `hrn` (`hrn.exe`) on
`PATH`, in `$GOPATH/bin` or `~/go/bin` and, on Windows, in `%LOCALAPPDATA%\hrn`, each listed
once with its version line. A file called hrn that does not answer `version` like hrn is left
alone.

**Backups.** Each target is renamed to `hrn-<build>-<yyyymmdd-hhmm>.prev[.exe]` beside it
(`<build>` is its short commit or module version, else `unknown`): Windows refuses to overwrite
a running binary but allows the rename. The candidate is copied in under the original name
with mode 0755 and run with `version`; when that fails or says something else, the backup goes
back and the target is reported as failed. The others are still done, and the exit code is 1
when any failed. `--dry-run` prints the plan and changes nothing.

**Rollback.** `hrn update --rollback` finds the newest `hrn-*.prev` beside each target and swaps
it back the same way, the current binary becoming `hrn-<build>-<stamp>.rolledback[.exe]`. A
target without a backup is reported as failed.

**The watch.** A running `hrn watch` started from one of the targets is stopped after the swap
and the new binary started detached with the same arguments, so `--lan`, `--addr` and
`--public` settings survive. Its output goes to the files the old one wrote to where the
system can tell (Linux), else to `watch-<port>.log` under `HRN_HOME`. The command prints
`watch restarted on <addr>` with the new pid. A watch whose binary could not be replaced is
left running. On macOS the watch is found through `ps`, which shows the program as it was typed;
a bare `hrn` is looked up on the `PATH` of `hrn update` itself.

**MCP servers.** Running `hrn mcp` processes are listed under "still on the old build until their
host restarts them", with pid, parent and start time, and never stopped: the host (Claude Code,
Codex) that started them owns them and respawns them on its next start.

### Remote development jobs

Set `HRN_SERVER_URL` to the control service HTTPS origin, `HRN_TENANT_TOKEN` to an
issued tenant token, and optionally `HRN_SERVER_CA_FILE` to a PEM CA file. HTTP
is accepted only for loopback fixtures. The tenant and project IDs are issued
by the server; `--base-ref` is a full Git commit registered for the project.
The origin may have a trailing `/`, but no credentials, other path, query
(including an empty `?`), or fragment, and its hostname may not be empty. The CA
path must name a regular file of at most 1 MiB; symlinks to regular files are
accepted. Devices, FIFOs and pipes are refused before they are opened, and on
Windows so are `\\.\`, `\\?\` and `\??\` paths and reserved device names such as
`NUL` or `COM1`.
Submission requires a stable idempotency key and a positive USD cost ceiling.
When `HRN_REMOTE_MAX_COST_USD` is set, a `--max-cost` above it is refused before
any request; unset means no client cap. The control service does not reserve
tenant budgets yet, so this and the server's `HRN_MAX_COST_USD_PER_JOB` (see
[server/README.md](../server/README.md)) are the spending control.
The prompt can be supplied as arguments or on stdin. Remote jobs currently
support only the read-only `api` backend. See [MCP](mcp.md#hrn-as-an-mcp-server)
for the corresponding tools.
Remote jobs refuse enabled retrieval options until the worker can honor their
version pin; use a local run for opted-in retrieval.
MCP servers and `allow_dangerous` are refused at submission and again by the
durable API run path; a stored job with either option fails with a clear result
error before any model call.

`hrn remote events --watch` prints events until it prints one that ends the job,
then exits with status 0, including when the job failed or was canceled. Terminal
event kinds are `succeeded`, `failed`, and `canceled`; `admission_timeout` and
`recovery_resolved` also end the watch when their `details.state` is terminal.
The watch reconnects from the last printed event ID if a stream ends before a
terminal event. Before streaming it reads the job's state (the request `hrn remote
show` makes): if the job has already ended, it prints the events after `--after`, if
any, and exits with status 0 without waiting. Ctrl-C ends the watch early.
Each event's data must be JSON of at most 1 MiB in total, however many `data:`
lines carry it; the space after a field's colon is optional. C1 control
characters (U+0080 to U+009F) and invalid UTF-8 are printed as `\u` escapes, so
the terminal never reads them as control sequences.

### Project knowledge

`hrn knowledge import|search|inspect|active|activate|disable|rollback request.json`
accepts one JSON request file and prints a JSON response. `import`, `activate`,
`disable`, and `rollback` require `HRN_OPERATOR_TOKEN`; read commands use
`HRN_TENANT_TOKEN` or the exact project's worker credential. Set
`HRN_SERVER_URL` and optionally `HRN_SERVER_CA_FILE` for the control service.
Credentials are never placed in request files.

An operator imports only reviewed sources with `origin: "reviewed_source"`,
`reviewed_by`, and `review_evidence`; model-generated candidates are rejected.
Each request names the exact tenant, project, backend, model, and full Git
revision. Imported content revisions and their version membership are
immutable. `active` returns the active and previous version pointers and their
generation. `activate`, `disable`, and `rollback` use `expected_generation` to
reject concurrent changes. `search` uses `query` and optionally `version_id`;
it returns an audited packet capped at 8192 bytes, including a valid empty or
no-match result. `inspect` reads a `version_id`. The server records each search
and ties its query and packet digests to the exact UTF-8 bytes. See
[the control service guide](../server/README.md#approved-project-knowledge) for
the protocol table and request examples.

### Budgets

Turns are a poor proxy for what a run costs: most of the spend is the context
re-sent every turn. Three ceilings, all optional and all off by default.

| Flag | Spec / MCP field | What it does |
|---|---|---|
| `--timeout 20m` | `timeout` | Ends the run when the deadline passes, on every backend. The result is `timeout`, not `canceled`, so a slow run is distinguishable from an interrupted one. |
| `--max-cost 2.50` | `max_cost_usd` | The api loop stops before a turn that would take its estimated cost past the limit. The result is `budget`, and the cost so far is still recorded. |
| `--task-budget 64000` | `task_budget` | Tells the api backend's model how many tokens it has for the whole task, so it paces itself and finishes rather than being cut off. Minimum 20000, checked before the run starts. |

The CLI backends cannot enforce a cost or token ceiling, so they say so once and
carry on; the timeout applies to them all the same. The cost ceiling is checked against
hrn's own estimate, so a model with no price in the table cannot be capped and the run
says so instead of pretending.

Agent CLIs and shell commands start in a Windows Job Object or a Unix process group.
Cancel or timeout kills that tree; CLI cleanup can take up to about 15 seconds while
stdout/stderr reads are bounded and remaining processes are checked (survivors or
unavailable Windows containment are reported). On Windows, a CLI's leftover background
processes end when the run ends, including after a normal exit, because the job is
closed. On Unix, background processes survive a normal exit and are killed only on
cancel or timeout; children can escape the group with setsid.

On Unix, Ctrl+C, SIGTERM and SIGHUP cancel runs. A process in the tree that reads the
terminal (such as a credential or host-key prompt) stops because the tree is not the
terminal's foreground process group.

On Windows, closing the console, logging off or shutting down reaches hrn as
SIGTERM. It has a few seconds to save the cancelled record while the job ends the
process tree. On Linux with procfs mounted with hidepid=2, a tree member owned by
another user (sudo or a setuid helper) is invisible to the check, so "gone" can be
optimistic.

Where a cost comes from depends on the backend. The claude and opencode CLIs report one
themselves; on a subscription that figure is what the run would cost at API list
prices, not a charge. codex, gemini and agy report none. The API returns token counts only,
so the api backend prices them from a table in `internal/agent/api.go`: Anthropic's
first-party list prices per model, including the newer models' own cache-read prices,
with cache writes at 1.25x input. A dated snapshot id such as
`claude-haiku-4-5-20251001` is priced as its model. A model missing from the table has
no estimate, and the run says so at the start rather than showing a cost of zero.
