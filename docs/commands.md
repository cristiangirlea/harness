# Command reference

[← README](../README.md)

## Commands

| Command | What it does |
|---|---|
| `hrn run [flags] <prompt>` | Run a task. Prompt from args, or stdin when omitted / `-`. |
| `hrn exec --protocol v1` | Run one recorded task over the supervisor JSONL protocol (see [hrn exec](#hrn-exec)). |
| `hrn backends` | Show which backends are usable and where they were found. |
| `hrn version` | Print the hrn version, the Go version, and the commit it was built from (a `go install` build shows its module version instead; Go does not stamp builds made inside a `git worktree`). |
| `hrn runs [-n N]` | List recent runs. |
| `hrn runs prune --older-than 30d [--keep N] [--dry-run]` | Remove old runs, batch records and eval records (see [Run records](run-records.md#run-records)). |
| `hrn show [--json] <id>` | Replay a recorded run. |
| `hrn show -f <id>` | Follow a run as it happens, until it ends (see [Watching runs live](watch.md#watching-runs-live)). |
| `hrn watch [--addr HOST:PORT] [--lan[=NAME]]` | A local page that follows runs and batches live (see [Watching runs live](watch.md#watching-runs-live)). |
| `hrn mcp` | Serve hrn as an MCP server on stdio (see [MCP](mcp.md#mcp)). |
| `hrn batch [flags] <tasks.json>` | Run tasks in parallel git worktrees with gates and retries (see [Batches](batches.md#batches)). |
| `hrn batch show [--json] <id>` | Reprint a batch's table, while it runs or long afterwards. |
| `hrn batch diff <id> <task-id>` | What one task changed, as a diff on stdout. |
| `hrn batch merge [--squash] [--rm] <id> [task-id…]` | Merge passed tasks; `--rm` removes each task's worktree and branch only when it merged in this call. |
| `hrn batch rm <id>` | Remove what a batch created: its branches, and its worktrees when they were fresh (a slot stays; see [Batches](batches.md#reading-and-landing-the-work)). |
| `hrn eval [flags] <eval.json>` | Run a task set across a matrix of backends, models and efforts (see [Evals](evaluations.md#evals)). |
| `hrn eval show <id>` | Reprint a saved eval. |
| `hrn eval list [-n N]` | List saved evals, newest first: cells, pass rate, cost with the judge. |
| `hrn eval diff [--json] <a> <b>` | Two evals side by side, variant by variant and task by task. |
| `hrn report [--since 7d] [--by backend\|model\|dir\|purpose] [--json]` | Tables over the finished runs and the batches (see [Reports](evaluations.md#reports)). |
| `hrn gc [--apply] [--grace 1h] [--idle 24h] [--min-idle 2h] [--free-below SIZE] [PATH…]` | Remove worktrees whose work is merged, with their build output, and build caches (`CACHEDIR.TAG`) idle past `--idle`, or past `--min-idle` on a drive below `--free-below` (see [Clearing merged worktrees](batches.md#clearing-merged-worktrees) and [Build caches](batches.md#build-caches)). |
| `hrn ws take [--repo DIR] [--branch NAME] [--base REF] [--for TEXT] [--max N] [--no-fetch]` | Lease a reusable worktree of the repository and print only its path, on `--branch` or detached at origin's default branch (see [Reusable workspaces](workspaces.md#take)). |
| `hrn ws free [PATH]` | Give the slot back once its work is pushed: HEAD detached, lease removed (see [Free](workspaces.md#free)). |
| `hrn ws list [--repo DIR] [--json]` | The slots: who holds each, since when, its branch and what is in it (see [List](workspaces.md#list)). |

Run flags: `-b/--backend`, `-m/--model`, `-C/--dir`, `--mode`, `--max-turns`,
`--effort` (api, codex and claude: low, medium, high, xhigh, max; unset uses the
backend default; gemini and opencode report it as not enforced),
`--json` (JSONL event stream for scripts), `--thinking`,
`--mcp FILE` (repeatable), `--no-instructions`, `--lean` (claude: only the run's
`--mcp` servers, your settings, rules and skills still load; see [Lean claude workers](backends.md#lean-claude-workers)),
`--compact-window N` (claude: compact at N tokens, 100000 to 1000000, or `auto`; see
[Compaction window](backends.md#compaction-window)),
`--allow-dangerous`, `--no-switch` (see
[When a plan's usage limit is reached](backends.md#when-a-plans-usage-limit-is-reached)), and the
budgets below.

### hrn exec

`hrn exec --protocol v1` lets a supervising program start, follow, cancel and record
one run per process without parsing terminal output. stdin and stdout carry JSON
lines: hrn sends `hello` first, then accepts one `start`, streams complete events,
and ends with `result`. Closing stdin or interrupting hrn cancels the run. See
[the v1 protocol](exec-protocol.md) for messages, capabilities, limits and exit codes.

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
prices, not a charge. codex and gemini report none. The API returns token counts only,
so the api backend prices them from a table in `internal/agent/api.go`: Anthropic's
first-party list prices per model, including the newer models' own cache-read prices,
with cache writes at 1.25x input. A dated snapshot id such as
`claude-haiku-4-5-20251001` is priced as its model. A model missing from the table has
no estimate, and the run says so at the start rather than showing a cost of zero.
