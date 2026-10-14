# Command reference

[← README](../README.md)

## Commands

| Command | What it does |
|---|---|
| `hrn run [flags] <prompt>` | Run a task. Prompt from args, or stdin when omitted / `-`. |
| `hrn exec --protocol v1` | Run one recorded task over the supervisor JSONL protocol (see [hrn exec](#hrn-exec)). |
| `hrn backends` | Show which backends are usable and where they were found. |
| `hrn remote submit --tenant ID --project ID --base-ref COMMIT --key KEY --max-cost USD [--max-turns N] <prompt>` | Queue a read-only API job on a registered project (see [Remote development jobs](#remote-development-jobs)). |
| `hrn remote show --tenant ID --project ID JOB_ID` | Show a remote job's state and result. |
| `hrn remote events --tenant ID --project ID [--after EVENT_ID] [--watch] JOB_ID` | Read up to 100 events, or stream and resume from the last durable event ID. |
| `hrn remote cancel --tenant ID --project ID JOB_ID` | Request cancellation of a remote job. |
| `hrn knowledge import\|search\|inspect\|active\|activate\|disable\|rollback request.json` | Manage approved project knowledge through the control service (see [Project knowledge](#project-knowledge)). |
| `hrn retrieval fixture [--json]` | Compare bundled synthetic accounting samples without a model call; the fixture proves no real savings. |
| `hrn retrieval compare --manifest FILE --samples FILE [--baseline-preparation FILE] [--candidate-preparation FILE] [--reuse N] [--runs] [--json]` | Compare saved retrieval evidence and verify measured run references locally, without a model call. Match each run on the requested model in `task.json`; a run without a requested model matches only when its reported result model exactly equals the variant model. Text output shows a differing provider-reported model. |
| `hrn version` | Print the hrn version, the Go version, and the commit it was built from (a `go install` build shows its module version instead; Go does not stamp builds made inside a `git worktree`). |
| `hrn runs [-n N]` | List recent runs. |
| `hrn runs prune --older-than 30d [--keep N] [--dry-run]` | Remove old runs, batch records and eval records (see [Run records](run-records.md#run-records)). |
| `hrn show [--json] <id>` | Replay a recorded run. |
| `hrn show -f <id>` | Follow a run as it happens, until it ends (see [Watching runs live](watch.md#watching-runs-live)). |
| `hrn show --usage [--json] <id>` | A run's model calls, one row each: tokens, the context each carried, the cache reads so far, and compactions recorded or probable (see [Usage per turn](run-records.md#usage-per-turn)). |
| `hrn watch [--addr HOST:PORT] [--lan[=NAME]] [--public https://HOST --access-team TEAM.cloudflareaccess.com --access-aud TAG]` | Follow runs and batches live. Public mode keeps the loopback listener and verifies Cloudflare Access on every request; it cannot be combined with `--lan` (see [Behind Cloudflare Access](watch.md#behind-cloudflare-access)). |
| `hrn mcp` | Serve hrn as an MCP server on stdio (see [MCP](mcp.md#mcp)). |
| `hrn batch [flags] <tasks.json>` | Run tasks in parallel git worktrees with gates and retries (see [Batches](batches.md#batches)). |
| `hrn batch show [--json] <id>` | Reprint a batch's table, while it runs or long afterwards. |
| `hrn batch diff <id> <task-id>` | What one task changed, as a diff on stdout. |
| `hrn batch merge [--squash] [--rm] <id> [task-id…]` | Merge passed tasks; `--rm` removes each task's worktree and branch only when it merged in this call. |
| `hrn batch rm <id>` | Remove what a batch created: its branches, and its worktrees when they were fresh (a slot stays; see [Batches](batches.md#reading-and-landing-the-work)). |
| `hrn eval [flags] <eval.json>` | Run a task set across a matrix of backends, models and efforts (see [Evals](evaluations.md#evals)). |
| `hrn eval show <id>` | Reprint a saved eval. |
| `hrn route show` | List purpose routing rows, or `(empty)` (see [Routing by evidence](evaluations.md#routing-by-evidence)). |
| `hrn route set <purpose> [-b NAME] [-m MODEL] [--effort LEVEL]` | Replace one routing row by hand; accepts any nonempty subset of the three choices. |
| `hrn route from-eval <id> [--margin 0.05]` | Print and save the cheapest qualifying rows from saved eval results. |
| `hrn eval list [-n N]` | List saved evals, newest first: cells, pass rate, cost with the judge. |
| `hrn eval diff [--json] <a> <b>` | Two evals side by side, variant by variant and task by task. |
| `hrn report [--since 7d] [--by backend\|model\|dir\|purpose\|routed\|day] [--json]` | Tables over the finished runs, their tokens and context per turn, and the batches; `routed` groups by the source routing row (see [Reports](evaluations.md#reports)). |
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
`--retrieval off|optional|required` opts into project references; enabled modes
require `--retrieval-tenant`, `--retrieval-project`, `--retrieval-revision` and
an explicit `--model`. `--retrieval-version` pins a version; otherwise the
active version is resolved once and saved with the run.

### hrn exec

`hrn exec --protocol v1` lets a supervising program start, follow, cancel and record
one run per process without parsing terminal output. stdin and stdout carry JSON
lines: hrn sends `hello` first, then accepts one `start`, streams complete events,
and ends with `result`. Closing stdin or interrupting hrn cancels the run. See
[the v1 protocol](exec-protocol.md) for messages, capabilities, limits and exit codes.

### Remote development jobs

Set `HRN_SERVER_URL` to the control service HTTPS origin, `HRN_TENANT_TOKEN` to an
issued tenant token, and optionally `HRN_SERVER_CA_FILE` to a PEM CA file. HTTP
is accepted only for loopback fixtures. The tenant and project IDs are issued
by the server; `--base-ref` is a full Git commit registered for the project.
The origin may have a trailing `/`, but no credentials, other path, query
(including an empty `?`), or fragment. The CA path must name a regular file;
symlinks to regular files are accepted.
Submission requires a stable idempotency key and a positive USD cost ceiling.
The prompt can be supplied as arguments or on stdin. Remote jobs currently
support only the read-only `api` backend. See [MCP](mcp.md#hrn-as-an-mcp-server)
for the corresponding tools.
Remote jobs refuse enabled retrieval options until the worker can honor their
version pin; use a local run for opted-in retrieval.

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
prices, not a charge. codex and gemini report none. The API returns token counts only,
so the api backend prices them from a table in `internal/agent/api.go`: Anthropic's
first-party list prices per model, including the newer models' own cache-read prices,
with cache writes at 1.25x input. A dated snapshot id such as
`claude-haiku-4-5-20251001` is priced as its model. A model missing from the table has
no estimate, and the run says so at the start rather than showing a cost of zero.
