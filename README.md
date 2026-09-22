# hx — personal agent harness

One Go binary that runs coding tasks on **your own agent loop** (Anthropic API) or on
**agent CLIs you already pay for** (Claude Code, Codex, Gemini, opencode), and records
every run in the same format so runs can be replayed, compared and scored later.

It speaks **MCP** both ways: the api loop can use any stdio MCP server's tools, and
`hx mcp` turns hx into an MCP server so Claude Code (or any host) can delegate tasks to
any backend, several at a time.

`hx batch` is the orchestrator: N agents in parallel, each in its own git worktree on
its own branch, with build/test gates and retries on another backend.

**Zero dependencies.** Standard library only — `go.mod` has no `require` lines. The API
client (streaming SSE, retries, prompt caching, thinking round-trips) is ~400 lines in
`internal/agent/messages.go`; MCP (JSON-RPC 2.0 over stdio, client and server) is ~600
lines in `internal/mcp`. The CLI backends are optional programs hx calls if they're
installed, not build dependencies.

```
hx run -b opencode --mode readonly "find the bug in math.go"
→ glob **/math.go
  ✓ C:\...\hxdemo\math.go
→ read C:\...\hxdemo\math.go
  ✓ <path>...</path> (+8 lines)
Yes, `add()` has a bug: it subtracts `b` from `a` instead of adding them.
── ok · opencode · 3 turns · 6.5k in / 87 out (cache 11.9k read) · 8.5s ──
```

## Why Go

Answer quality comes from the model, not the harness language. What the harness
language decides is overhead and concurrency: a static ~7 MB binary with low idle memory,
cheap goroutines for parallel tool calls and (next) many agents at once, and no runtime
to install. Rust would shave a little memory at a large cost in iteration speed; Python
costs 3–5× the memory and needs an interpreter.

## Build

```bash
go build -o hx.exe ./cmd/hx
go test ./...
```

## Commands

| Command | What it does |
|---|---|
| `hx run [flags] <prompt>` | Run a task. Prompt from args, or stdin when omitted / `-`. |
| `hx backends` | Show which backends are usable and where they were found. |
| `hx runs [-n N]` | List recent runs. |
| `hx show [--json] <id>` | Replay a recorded run. |
| `hx mcp` | Serve hx as an MCP server on stdio (see [MCP](#mcp)). |
| `hx batch [flags] <tasks.json>` | Run tasks in parallel git worktrees with gates and retries (see [Batches](#batches)). |
| `hx batch rm <id>` | Remove the worktrees and branches a batch created. |
| `hx eval [flags] <eval.json>` | Run a task set across a matrix of backends, models and efforts (see [Evals](#evals)). |
| `hx eval show <id>` | Reprint a saved eval. |
| `hx report [--since 7d] [--by backend\|model\|dir] [--json]` | Tables over the finished runs and the batches (see [Reports](#reports)). |

Run flags: `-b/--backend`, `-m/--model`, `-C/--dir`, `--mode`, `--max-turns`,
`--effort` (api), `--json` (JSONL event stream for scripts), `--thinking`,
`--mcp FILE` (repeatable), `--no-instructions`, `--allow-dangerous`, and the budgets
below.

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
hx's own estimate, so a model with no price in the table cannot be capped and the run
says so instead of pretending.

## Backends

| Backend | Billing | How it runs |
|---|---|---|
| `api` | per token | The harness's own loop: Messages API (streaming, adaptive thinking, prompt caching, server-side refusal fallbacks on `claude-opus-5`) + client-side tools below + MCP tools. Needs `ANTHROPIC_API_KEY` (or an OAuth `ANTHROPIC_AUTH_TOKEN`); `ANTHROPIC_BASE_URL` overrides the endpoint. |
| `claude` | your Claude subscription | `claude -p --output-format stream-json`. Found on PATH, `~/.local/bin`, npm, or the Claude desktop app's bundled build (that build needs its own `claude /login` first). |
| `codex` | your ChatGPT subscription | `codex exec --json`. Not installed on this machine yet — event parsing is written against the documented format but untested. |
| `gemini` | your Google login | `gemini -o json` (single JSON result, no live stream). Untested end to end. |
| `opencode` | whatever opencode is configured with | `opencode run --format json`, prompt attached as a file. Verified. |

Default backend: `api` when credentials exist, otherwise `claude`.

### Permission modes

| Mode | api backend | claude | codex | gemini | opencode |
|---|---|---|---|---|---|
| `readonly` | read tools only (write tools not offered) | `plan` | `--sandbox read-only` | `default` | its config |
| `ask` (default) | reads auto; writes + commands prompt y/N | `default` | `--sandbox read-only` | `default` | its config |
| `auto` | reads + edits auto; commands prompt | `acceptEdits` | `--full-auto` | `auto_edit` | its config |
| `yolo` | everything | `bypassPermissions` | bypass sandbox | `yolo` | `--auto` |

When the prompt comes from stdin there is no terminal to ask, so anything needing
approval is denied.

### api tools

`read_file`, `list_files` (glob with `**`), `grep` (RE2), `write_file`, `edit_file`
(exact unique match, CRLF-aware), `run_command` (Git Bash when present, else PowerShell;
120s default timeout). All paths are confined to the workspace, symlinks included.
Read-only calls in one turn run in parallel; writes and commands run in order.

#### The command guard

`run_command` refuses five kinds of command line before the shell sees them:

| Rule | Refuses |
|---|---|
| `git-push` | any `git` command mentioning `push`, including `git -C dir push` — a batch commits on its own branch; what leaves the machine is the operator's call |
| `sudo` | `sudo` — a password prompt hangs a run with no terminal |
| `rm-root` | `rm -r` whose target is a root: `/`, `/*`, `~/`, `"$HOME"`, `C:\`, `C:/`, `/c/` |
| `pipe-to-shell` | `curl`/`wget` piped into a shell, however many stages the pipe has |
| `disk` | `mkfs`, `format X:`, `dd of=/dev/…` |

`rm-root` reads the arguments rather than matching one pattern, because the spelling
that matters is not the famous one: `rm -rf /` is refused by `rm` itself without
`--no-preserve-root`, while `rm -rf /*` and `rm -rf "$HOME"` are not.

The refusal comes back to the model as a failed tool call naming the rule, so the run
continues and the transcript says what was stopped. It is decided before any approval
prompt, so `ask` mode never asks for a yes it would then ignore. Matching is textual and
leans towards refusing too much: `echo "git push"` and `git commit -m "push later"` are
refused as well, which costs a turn and is the trade this makes on purpose.

This is a guard rail, not a sandbox — an agent that wants around a pattern can get
around it. What decides whether commands run at all is the [permission mode](#permission-modes);
this stops the plausible accident. `--allow-dangerous` (task field `allow_dangerous`,
also a batch spec/task field and an MCP `run` argument) lifts it for a whole run, and the
system prompt tells the model which way it is set; `yolo` on its own does not lift it.

Three things it does not cover, by design: the agent CLIs run their own shells under
their own permission systems (an unattended CLI run says so in its output), tools from
an MCP server are the server's business, and batch gate commands are not checked,
because the operator wrote those.

### Context management

A long run carries every tool result, and the cap is 30 KB per result, so the
conversation reaches hundreds of thousands of tokens. The api backend therefore asks
the server to clear old tool results itself (`clear_tool_uses_20250919`). hx still
uploads its whole history, which stays append-only; what the clearing changes is the
context the model is given, so it is the context window and the billed input that
shrink, not the request body. A clear invalidates the cached prefix from that point,
which is the trade: one cold turn for a smaller context afterwards. That balance has
not yet been measured against a real long run, so compare `cache_read_tokens` across
two runs before trusting it. If the API ever refuses the beta, the run says so and
retries once without it. `hx run --no-context-edits` turns it off for good, as do the
`no_context_edits` field on a batch spec or task and the MCP `run` argument.

### Continuing a run

The api backend saves its message history next to the transcript, so a finished run can
be picked up where it stopped:

```bash
hx run --continue 20260921-095628-f18a6d "now fix the failing test"
```

The follow-up inherits the earlier run's backend, workspace, mode and model unless you
pass those flags; the new run is recorded separately and its `task.json` says which run
it continues. A history that ends on a tool call the model never got an answer for is
not saved, because the API refuses to resume from one. Only the api backend saves a
conversation: the agent CLIs own their own session state, so continuing one of those
runs is refused with a clear message. The MCP `run` tool takes the same thing as
`continue_run_id`, and batch attempts save their conversations too, so a failed attempt
can be picked up by hand.

Thinking blocks belong to the model that produced them. Continuing on a different model
is allowed and warned about: the API serves the conversation without the earlier
thinking.

### Project instructions

The api loop reads `HX.md`, `AGENTS.md` and `CLAUDE.md` from the workspace root and
puts them in its system prompt, so it sees the same house rules the agent CLIs read
from their own files. Only the root is searched, the files are sent in that order, and
the total is capped at 32 KiB; the run says what it loaded and whether the cap cut
anything. A file that is a symlink out of the workspace is refused, like any other
path. `hx run --no-instructions` skips them, as do the `no_instructions` field on a
batch spec or task and the MCP `run` argument of the same name.

## MCP

### Using MCP servers as tools

`hx run --mcp .mcp.json ...` starts every stdio server in the file, offers its tools to
the model as `mcp__<server>__<tool>` (Claude Code's naming) and stops the servers when
the run ends. `~/.hx/mcp.json` (or `$HX_HOME/mcp.json`) is loaded whenever it exists;
`--mcp` files add to it, and a later file overrides an earlier server of the same name.
The format is Claude Code's, so existing `.mcp.json` files work unchanged:

```json
{"mcpServers": {"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
                           "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}}}}
```

`${VAR}` and `${VAR:-default}` expand from the environment. Entries with a `url` (HTTP
transports) are skipped: hx speaks stdio only. Servers run with the workspace as their
working directory and are told it is the root.

Permissions follow the mode table. A tool the server marks `readOnlyHint` is a read:
auto-approved and run in parallel. Anything else needs approval in `ask` and `auto` and
is automatic only in `yolo`; in `readonly` mode it is not offered at all. The hint is
trusted because you chose the server.

Per backend: `api` uses the servers directly; `claude` receives each file as
`--mcp-config`; codex, gemini and opencode keep their own MCP configuration.

The config paths are recorded in the run's `task.json`, so a run can be replayed with
the same tool set.

### hx as an MCP server

`hx mcp` serves hx on stdio. Register it once with Claude Code, from the project you
want it to work in by default:

```bash
claude mcp add hx -- E:\__Workdir\harness\hx.exe mcp
```

The default workspace is the server's working directory, or `--dir PATH` (an absolute
path; a relative one would resolve against the host's working directory, the very
thing the flag avoids) for hosts that do not start servers in the project folder.
Cursor (`.cursor/mcp.json`) and VS Code (`.vscode/mcp.json`) take the same command
and args.

Tools:

| Tool | Arguments | Returns |
|---|---|---|
| `run` | `prompt`, `backend`, `model`, `dir`, `mode`, `max_turns`, `effort`, `mcp`, `no_instructions`, `no_context_edits`, `allow_dangerous`, `continue_run_id`, `timeout`, `max_cost_usd`, `task_budget` | Blocks until the task finishes: final answer, summary line (status, tokens, cost, duration) and run id. Tool calls stream back as progress. |
| `runs` | `limit` | Recent runs, newest first. |
| `show` | `run_id`, `events` | Task, event transcript and result of one run. |
| `backends` | — | Which backends are usable here. |

Requests are served concurrently, so parallel tool calls from the host become parallel
agents, each recorded under `~/.hx/runs` like a CLI run. Cancelling the tool call cancels
the run. There is no terminal behind the server, so `ask` denies every write and
command: `run` defaults to `readonly`; use `auto` for edits and `yolo` for commands.

## Batches

`hx batch tasks.json` runs every task in the file against one repository, `parallel`
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

For each task hx creates branch `hx/<id>` and worktree `.hx/worktrees/<id>` from the
current `HEAD` (uncommitted changes in the main worktree are not included), runs the
agent there, then the gates in order, stopping at the first non-zero exit or after 15
minutes. Whatever the agent left is committed on the branch as `<id>: <prompt>`, pass or
fail, so a worktree can be inspected, merged or removed without losing work; if that
commit fails (no git identity, a pre-commit hook) the task ends as `error` and the work
stays uncommitted in the worktree. hx writes `.hx/.gitignore` so the worktrees never
show up in the repository's status.

A failed attempt is retried on each backend named in `retry`, in order. The same name
may repeat, and `"retry": ["claude"]` after `"backend": "claude"` is the common case: a
second try with the same model, told what went wrong. How the retry starts depends on
how the attempt failed.

| Previous attempt | Next attempt |
|---|---|
| ran to the end, a gate failed | continues in the same worktree, on top of the work already committed, with the failing gate command, its exit code and output appended to the prompt |
| failed, refused or was cut short | starts in a fresh worktree from the base commit, with one line saying how the previous attempt ended |

A continued attempt commits as `<id>: <prompt> (attempt N)`, so the branch reads as a
sequence of fixes. Each attempt records the prompt it was actually sent and whether it
continued, which `hx batch --json` prints.

`hx batch` refuses to start when tracked files in the repository are modified: every
worktree branches from `HEAD`, so that work would not reach the agents and the batch
would quietly be testing something else. Commit or stash first, or pass `--allow-dirty`
(spec field `allow_dirty`). Untracked files are not counted.

Nothing can answer approval prompts in a batch, so `mode` defaults to `auto` (file edits
allowed, no commands). Use `yolo` when the agent must build or run tests itself; the
gates run either way. Backend names, gate commands, durations and task budgets are all
checked before anything runs.

The budgets above are spec fields too (`timeout`, `max_cost_usd`, `task_budget`), and
they apply per attempt rather than per task, so a retry gets its own allowance. A task
may set its own value or change the spec's, but zero means inherit, so it cannot remove
a limit the spec set.

The record goes to `~/.hx/batches/<id>.json`, written before the first task starts and
again at the end; `--json` prints it, `-v` streams every agent's tool calls.
`hx batch rm <id>` removes the worktrees and branches of a batch (it refuses a worktree
with uncommitted changes); the record and the run logs stay. Ctrl+C cancels running
agents and skips the rest. Exit status is 1 unless every task passed.

## Reports

`hx report` reads every run and batch record and prints two tables.

```
runs by backend, since 2026-09-14 17:40 (7d)
backend  runs  ok  ok%  med dur  med turns  total cost  med cost  cache
claude      6   5  83%    1m2s          9      ~$1.03     $0.17    96%
api         3   3 100%     48s          6       $0.42     $0.14    71%
all         9   8  89%     55s          8      ~$1.45     $0.15    90%

batches by repository, since 2026-09-14 17:40 (7d)
repository              batches  tasks  pass%  med attempts  total cost
E:\__Workdir\JobPilotC        1      3   100%             1      ~$1.03
all                           1      3   100%             1      ~$1.03
```

Runs group by backend, or by `--by model` (the model that answered) or `--by dir` (the
workspace). Columns: runs, how many ended `ok`, median duration, median turns, total and
median cost, and the cache share (cache-read tokens over every input token, cache writes included;
`-` when the backend reported no token counts). A `~` marks a cost that is the api backend's estimate.
Batches group by repository; their cost sums every attempt, passed or not. Runs without
a result (still running or killed), batches with a task still running, and records that
cannot be read are counted under the tables and left out of them.
`--since` takes a window such as `7d`, `36h` or `90m`; `--json` prints the same rows as
JSON for scripts.

## Evals

An eval is a batch with two more dimensions: a matrix of variants to compare and a
number of repeats. Every cell is an ordinary recorded run, scored by the same gates, so
the question "which backend, model and effort for this kind of task" gets an answer with
numbers behind it.

```json
{
  "parallel": 3, "mode": "yolo",
  "gate": ["go build ./... && go test ./..."],
  "matrix": [
    {"name": "opus-high", "backend": "api", "model": "claude-opus-5", "effort": "high"},
    {"name": "opus-low",  "backend": "api", "model": "claude-opus-5", "effort": "low"},
    {"name": "claude",    "backend": "claude"}
  ],
  "repeats": 3,
  "timeout": "20m", "max_cost_usd": 3,
  "tasks": [{"id": "vet", "prompt": "Run go vet and fix every finding."}]
}
```

A variant's `name` defaults to `backend[-model][-effort]`. Each cell becomes a batch
task with the id `<evalid>.<task>.<variant>.r<n>`, so branches from different evals never
collide. Retries are off for every cell on purpose: an eval measures what one attempt
does, and a retried attempt would score the harness rather than the backend. Everything
else, gates and mode and the budgets, carries through from the spec.

`hx eval eval.json` prints what each variant did and how each task scored under it:

```
  variant                pass  attempts  median cost median time   turns   context
  sonnet                   0%         1        $0.15        13s       2    101061
  haiku                  100%         1        $0.05        14s       2     71814

  task                     sonnet      haiku
  greet                       0/1        1/1
── eval cb867 · batch 20260922-164017-1bbd7c · 2 cells · 14s ──
```

That run is real: asked for a file containing exactly `hx rocks`, Sonnet wrote `hx rocks.`
and failed the gate while Haiku passed at a third of the cost. The run records are still
there to say why, which is the point of keeping them.

The record goes to `~/.hx/evals/<id>.json`; `--json` prints it, `hx eval show <id>`
reprints it, `-v` streams every agent's tool calls. The worktrees are removed once the
record is safely written, unless `--keep`. A failing variant is data, not an error, so
the exit status is 0 whenever the eval itself ran.

Task ids and variant names take letters, digits, `_` and `-`: dots separate the parts of
a cell id. `retry` is refused in an eval spec, because a retried cell would score the
harness rather than the backend. The `context` column counts prompt tokens and cache
reads together, which is what the model was given rather than what was billed at full
rate.

The `judge` key is reserved for scoring a diff against a rubric with a model. Nothing
reads it yet: gates are the only scorer in this version, which is why a task's gate has
to express what "done" means, and a spec that fills it in is told so at the start.

## Run records

`~/.hx/runs/<id>/` (or `$HX_HOME/runs`):

- `task.json` — prompt, dir, backend, model, mode, MCP config paths
- `events.jsonl` — text, thinking, tool calls, tool results, info, errors (timestamped)
- `messages.json` — the api backend's message history, for `hx run --continue`. It
  repeats the tool results the transcript already holds, so an api run costs roughly
  twice the disk of its transcript; nothing prunes `~/.hx` yet
- `result.json` — status (`ok|error|refused|max_turns|timeout|budget|canceled`), final answer, turns,
  tokens, cache, cost (`cost_is_estimate` for the api backend), duration

## Layout

```
cmd/hx/            CLI + terminal renderer
internal/agent/    Backend interface, api loop + stdlib Messages client, CLI adapters
internal/tools/    workspace-confined tool runtime + MCP tool adapter
internal/mcp/      MCP over stdio: JSON-RPC codec, Client, Server, config loader
internal/hxmcp/    hx's own MCP server (run, runs, show, backends)
internal/batch/    orchestrator: parallel worktrees, gates, retries, batch records
internal/report/   aggregation over run and batch records (hx report; reused by eval)
internal/runlog/   run recording
```

## Roadmap

The `agent.Backend` contract (Task → Events → Result) is what the layers build on.
`hx mcp` (a host session plans, hx runs) and `hx batch` (parallel worktrees, gates,
retries) are the orchestrator; the eval layer is what remains.

1. **Eval** — run a task set × backends × models repeatedly, score results
   (deterministic checks first, LLM judge only where needed), compare over time from
   the run and batch records.
2. **Batch follow-ups** — feed a failed attempt's gate output into the retry prompt;
   `hx batch merge` for passed branches.
3. **MCP follow-ups** — Streamable HTTP transport for remote servers; MCP elicitation so
   a host can answer `ask`-mode approvals instead of hx denying them.
