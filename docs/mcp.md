# MCP integration

[← README](../README.md)

## MCP

### Using MCP servers as tools

`hrn run --mcp .mcp.json ...` starts every stdio server in the file, offers its tools to
the model as `mcp__<server>__<tool>` (Claude Code's naming) and stops the servers when
the run ends. `~/.hrn/mcp.json` (or `$HRN_HOME/mcp.json`) is loaded whenever it exists;
`--mcp` files add to it, and a later file overrides an earlier server of the same name.
The format is Claude Code's, so existing `.mcp.json` files work unchanged:

```json
{"mcpServers": {"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
                           "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}}}}
```

`${VAR}` and `${VAR:-default}` expand from the environment, where hrn's own secrets
(`HRN_DATABASE_URL`, `HRN_*_TOKEN`) read as unset. Entries with a `url` (HTTP
transports) are skipped: hrn speaks stdio only. Servers run with the workspace as their
working directory and are told it is the root.

Permissions follow the mode table. A tool the server marks `readOnlyHint` is a read:
auto-approved and run in parallel. Anything else needs approval in `ask` and `auto` and
is automatic only in `yolo`; in `readonly` mode it is not offered at all. The hint is
trusted because you chose the server.

Per backend: `api` uses the servers directly; `claude` receives each file as
`--mcp-config`; codex, gemini and opencode keep their own MCP configuration.

The config paths are recorded in the run's `task.json`, so a run can be replayed with
the same tool set.

### hrn as an MCP server

`hrn mcp` serves hrn on stdio. Register it once with Claude Code, from the project you
want it to work in by default:

```bash
claude mcp add hrn -- hrn mcp
```

The default workspace is the server's working directory, or `--dir PATH` (an absolute
path; a relative one would resolve against the host's working directory, the very
thing the flag avoids) for hosts that do not start servers in the project folder.
Cursor (`.cursor/mcp.json`) and VS Code (`.vscode/mcp.json`) take the same command
and args.

`hrn mcp --retrieval-profile FILE` serves a reviewed project profile with a fixed
workspace, backend, model, revision, knowledge version and source hashes. It
checks the revision and source contents before each retrieval-enabled run.
It refuses to start with a disabled profile backend.
This server exposes `run`, `runs`, `show` and `backends`, and accepts fresh runs
only; a baseline may set `retrieval: "off"`. It cannot be combined with
`--allow-dangerous-runs`. The profile contains no credentials.

The server refuses `allow_dangerous` on `run` and `batch`, and leaves it out of `run`'s
arguments, unless it was started with `--allow-dangerous-runs`: the host that calls it is
an agent, and lifting [the guard](permissions.md#the-guard-for-every-backend) is yours to decide.

Tools:

| Tool | Arguments | Returns |
|---|---|---|
| `run` | `prompt`, `purpose`, `backend`, `model`, `dir`, `mode`, `max_turns`, `effort`, `mcp`, `no_instructions`, `no_context_edits`, `lean`, `skills`, `compact_window`, `append_system_prompt`, `no_skills`, `retrieval`, `retrieval_tenant`, `retrieval_project`, `retrieval_revision`, `retrieval_version`, `allow_dangerous` (with `--allow-dangerous-runs` only), `continue_run_id`, `fresh` (with `continue_run_id`: a new session with a hand-over, see [backends](backends.md#a-fresh-hand-over)), `timeout`, `max_cost_usd`, `task_budget`, `no_switch` | Blocks until the task finishes: final answer, summary line (status, tokens, cost, duration) and run id. Tool calls stream back as progress. A run carried on at a usage limit answers with the run that carried it on, and `limited_run_id`. |
| `runs` | `limit` | Recent runs, newest first. |
| `show` | `run_id`, `events` | Task, event transcript and result of one run. |
| `flow` | `id`, `level` | A run or batch execution tree with recorded reasons and next steps; level defaults to `technical` (`simple`, `technical` or `debug`). Text over 64 KiB is cut, and then the structured tree is left out with `truncated: true`; use a lower level or `hrn flow --json`. |
| `backends` | — | Availability and remembered limits, plus per-backend `enabled` and `default` booleans. The top-level `default` string names the backend selected for a fresh run. |
| `batch` | `spec` | Runs a batch spec, the same JSON as a batch file, and blocks until every task is done: one line per task (status, backend, attempts, the gate that failed, branch and commit) and the batch id. Task outcomes stream back as progress. |
| `batch_show` | `batch_id`, `task`, `limit` | No id: recent batches. An id: its tasks. An id and a task: that task's diff and its untracked files. |
| `batch_merge` | `batch_id`, `tasks`, `squash` | Merges the passing branches into the repository's current branch, stopping at the first conflict with the tree put back. |
| `remote_submit` | `tenant_id`, `project_id`, `prompt`, `base_ref`, `idempotency_key`, `max_cost_usd`, optional `max_turns` | Queue an asynchronous job and return its durable ID. |
| `remote_status` | `tenant_id`, `project_id`, `job_id` | Current job state and result. |
| `remote_events` | `tenant_id`, `project_id`, `job_id`, optional `after` | Up to 100 events after a durable event ID. |
| `remote_cancel` | `tenant_id`, `project_id`, `job_id` | Request job cancellation. |

The `run` tool's `backend` enum contains only enabled registered names, in preference
order, when the server starts. Restart the server to refresh the advertised enum
after changing preferences. Every call reads the current file and refuses a disabled
backend as a tool error, even if it appeared in an older enum. A malformed preference
is also a tool error. In `backends`, the per-backend default marker identifies the
configured default, or the selected default when none is configured; an unavailable
configured default can differ from the top-level selected default. See
[Backend preferences](backends.md#choosing-which-backends-hrn-may-use).

The `run` tool and each task in a `batch` spec accept optional `purpose`: `review`,
`debug`, `implement`, `refactor`, `test`, `research`, `plan` or `ops`. Invalid values
are tool errors; without one, a valid leading `Purpose: <category>` header is used.
The declaration is saved without changing the prompt, and batch task summaries
include it when set. See [Declared purpose](commands.md#declared-purpose) for
header handling and classifier fallback.

The remote tools use `HRN_SERVER_URL` for the control service HTTPS origin,
`HRN_TENANT_TOKEN` for its issued tenant credential, and optional
`HRN_SERVER_CA_FILE` for a PEM CA file. HTTP is accepted only for loopback
fixtures. The origin may have a trailing `/`, but no credentials, other path,
query (including an empty `?`), or fragment. The CA path must name a regular
file; symlinks to regular files are accepted. `remote_submit` accepts only
read-only `api` jobs on registered projects, with a full commit, positive cost
ceiling, and idempotency key. When `HRN_REMOTE_MAX_COST_USD` is set on the MCP
server, a `max_cost_usd` above it is refused before any request; a fresh
idempotency key queues a new job, and tenant budgets are not reserved yet, so
set this cap for hosts you do not fully trust.
Unknown and unsupported input fields are refused.

Requests are served concurrently, so parallel tool calls from the host become parallel
agents, each recorded under `~/.hrn/runs` like a CLI run. Cancelling the tool call cancels
the run. `run` defaults to `readonly`; `auto` allows file edits and `yolo` also allows
commands.

There is no terminal behind the server, so `ask` mode asks the host instead: each write
or command goes back to the person through MCP elicitation, one question at a time, and
a run waits up to five minutes for each answer. The prompt names the run and carries the
whole command or path being approved, because a truncated one can hide the part that
matters, and the decision goes back to the host as progress. Anything other than a clear
yes is a no, including a declined prompt, a cancelled one, an error, or a host that does
not support elicitation at all. Two runs in ask mode at the same time ask independently,
which is why the prompt says which run it is from. That makes `ask` a real mode in the
place hrn runs most.

The batch tools let a host fan work out without leaving the conversation: write a spec,
call `batch`, read the diffs with `batch_show`, and land what passed with `batch_merge`.
A spec's relative `dir` is taken from the server's workspace. Unknown fields are refused
rather than ignored, because the spec is written by a model and a misspelt `gates` would
otherwise run with no gate. The structured result is a short summary rather than the
whole record, since some hosts, Claude Code among them, show it to the model in place of
the text. Worktrees stay after a merge; `hrn batch rm <id>` removes them.

Progress messages from `run` and `batch`, final answers and errors in the `run`
result, event text, tool names and tool-call arguments in `show` (run replay), and
errors in the `batch` and `batch_merge` results replace terminal control characters
(C0 except newline and tab, DEL and C1) with U+FFFD, using the same filter as batch
summary errors and attempt notes. Structured results filter the corresponding fields too, including the run
result in `show`. Tool-call arguments filter nested string values and object keys;
invalid JSON becomes a filtered JSON string. Multiline bodies retain newlines and tabs;
saved records are unchanged. Tool names in `run` progress are collapsed to one line
and truncated at 80 characters.
