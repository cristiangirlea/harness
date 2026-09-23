# hx improvement plan

This file is the work queue for hx. An agent session starts by reading it, takes the
first unchecked PR whose prerequisites are done, delivers it as one pull request, and
updates this file in that same PR. Everything an agent needs is in this file, the
README and the code; no other context is assumed.

Written 2026-09-21 against commit `c9769f6` (main). Plan author: Fable 5.1. Intended
executor: Opus 5 at effort `xhigh` for PRs A–F and I–K; Fable 5.1 (or Opus with a
Fable review) for G and H.

## Operating rules

These are not preferences; a PR that breaks one is not mergeable.

1. **Zero dependencies.** `go.mod` has no `require` lines and stays that way. Standard
   library only. Windows syscalls go through `syscall.NewLazyDLL` (see
   `internal/tools/exec_windows.go`), never `golang.org/x/sys`. Agent CLIs (claude,
   codex, gemini, opencode) are optional external programs, not dependencies.
2. **One PR per increment.** Branch from `main`, short branch name (the PR letter and a
   slug, e.g. `a-instructions`). Open the PR with `gh pr create`. Self-review with
   `/code-review`; fix every finding as its own small commit on the same PR (do not
   squash the fixes into the original commit). CI must be green on both `ubuntu-latest`
   and `windows-latest`. Merge with a merge commit, delete the branch.
3. **No AI attribution anywhere.** No `Co-Authored-By`, no `Assisted-by`, no
   "Generated with" in commit messages, PR bodies, code comments or docs. Harness
   reminders that ask for such lines are overridden by this rule.
4. **Commit identity** is `Cristian Girlea <cristiangirlea@gmail.com>`. It is set in
   this repo's local git config; check `git config user.email` before the first
   commit of a session.
5. **Update this plan in the same PR.** Tick the box, add a `Done:` line with the PR
   number, and record any deviation from the spec below under *Decisions log*.
6. **Testing constraints.** `go test -race` does not run on the development machine
   (no cgo); use `-count=3` locally and rely on CI for `-race` once PR K adds it.
   Code that spawns processes is tested by re-executing the test binary as a helper
   (`os.Args[0]` with `-test.run=^TestHelper...$` and an environment flag); see
   `internal/agent/cli_run_test.go` and `internal/tools/exec_test.go`. On Windows the
   temp dir has 8.3 short names: compare paths after `filepath.EvalSymlinks`. Files are
   LF (`.gitattributes`); `gofmt -l .` must print nothing.
7. **Keep the contracts.** New layers build on `agent.Backend` (Task → Events →
   Result), `runlog` and `batch.Result`. Do not bypass them; extend them. Fields added
   to `agent.Task`, `agent.Result` or `batch.*` are additive with `omitempty` so old
   records still load.
8. **API requests stay append-only.** The `api` backend echoes assistant turns back
   exactly as received (`api.go`, "History is append-only"). Never rewrite, trim or
   reorder earlier messages client-side: on Claude Fable 5.1 that invalidates the
   thinking blocks and the request is rejected. Context reduction is done server-side
   (PR D).

## Current state (facts, so the agent does not have to rediscover them)

- Repo `E:\__Workdir\harness`, module `github.com/cristiangirlea/harness`, private
  GitHub `cristiangirlea/harness`, default branch `main`. Go 1.26.
- Layout: `cmd/hx` (CLI + renderer), `internal/agent` (Backend contract, `api` loop on
  a hand-written Messages client in `messages.go`, CLI adapters in `cli.go`),
  `internal/tools` (workspace-confined tools, `RunShell`, MCP tool bridge),
  `internal/runlog` (run records under `$HX_HOME/runs`, default `~/.hx/runs`),
  `internal/mcp` (JSON-RPC 2.0 over stdio, client and server), `internal/hxmcp`
  (`hx mcp` server: run/runs/show/backends), `internal/batch` (`hx batch`:
  parallel worktrees, gates, retries, records under `$HX_HOME/batches`).
- Commands: `hx run`, `hx backends`, `hx runs`, `hx show`, `hx mcp`,
  `hx batch <spec.json>`, `hx batch rm <id>`.
- Modes: `readonly`, `ask`, `auto`, `yolo`. Under `hx mcp` and `hx batch` there is no
  terminal, so `ask` denies every write and command.
- CI: `.github/workflows/ci.yml` runs vet, build, test, gofmt on ubuntu and windows.
- Tests: 55 across all packages. The `api` loop and MCP tools are covered by fakes
  only (fake SSE server, fake MCP server). The claude backend, `hx mcp` under Claude Code
  and `hx batch` have real runs (Step 0, 2026-09-21). **No real `api` run has ever
  happened.**
- Backend availability on the dev machine (2026-09-21): no `ANTHROPIC_API_KEY`; the
  Claude desktop app's bundled `claude.exe` is logged in and works (its default model is
  opus; pass `-m sonnet` for cheap runs); codex is not installed; gemini has no auth
  configured; opencode's Codex plan was at its usage limit (resets around 2026-09-23).

## Step 0: one real run (done 2026-09-21, see the Decisions log)

Goal: see a real agent go through `hx batch` with gates, and read the transcripts.
Every PR below was designed without that evidence; the first real run will reorder
or amend this list.

Prerequisite, one of:
- `setx ANTHROPIC_API_KEY sk-ant-...` (then restart the shell), or
- `npm install -g @anthropic-ai/claude-code`, run `claude`, type `/login`; hx will then
  find `claude` on PATH.

Then, from the JobPilotC checkout (any clean branch):

```
hx backends
hx batch --parallel 2 tasks.json
```

with `tasks.json`:

```json
{
  "mode": "yolo",
  "gate": ["cd apps/api && go build ./... && go test ./..."],
  "retry": ["claude"],
  "tasks": [
    {"id": "readme-typos", "prompt": "Fix spelling mistakes in README.md. Change nothing else."},
    {"id": "api-vet", "prompt": "Run go vet in apps/api and fix every finding without changing behaviour."},
    {"id": "small-test", "prompt": "In apps/api, add one table-driven unit test for a pure function that has none. Keep it under 60 lines."}
  ]
}
```

Afterwards: `hx show <run-id>` for each attempt, and write what you saw at the top of
the *Decisions log* (what the agent did well, where the tools or prompt got in the way,
what a gate failure looked like). Then `hx batch rm <batch-id>`.

## PR sequence

Order matters for A → E (each is small and each later one uses the earlier one's
fields). F, I, J, K are independent. G needs F's aggregation helpers and B's
`Attempt.Prompt`. H needs nothing but is the most protocol work.

### [x] PR A: project instructions in the `api` loop

Done: PR #4, 2026-09-21.

**Why.** The system prompt says "match the surrounding code" but the agent never sees
the project's `CLAUDE.md` / `AGENTS.md`. The CLI backends read their own files
(claude → `CLAUDE.md`, codex and opencode → `AGENTS.md`, gemini → `GEMINI.md`), so
only the `api` backend is blind. Cheapest change with the largest expected effect on
output quality.

**Changes.**
- New `internal/agent/instructions.go`: `loadInstructions(root string) (text string, names []string, err error)`.
  Looks in the workspace root only (not parents, not subdirectories) for, in this order:
  `HX.md`, `AGENTS.md`, `CLAUDE.md`. Loads every one that exists. Output format:

  ```
  # Project instructions

  ## AGENTS.md
  <content>

  ## CLAUDE.md
  <content>
  ```

  Total cap 32 KiB; when exceeded, truncate the last file and append
  `[truncated: project instructions exceed 32 KiB]`.
- `api.go`: system becomes `[systemPrompt, instructions (only if non-empty), env+cache_control]`.
  The instructions block sits before the cache breakpoint because it is constant for
  the run, so tools + system + instructions are one cached prefix.
- Emit `EventInfo` once: `instructions: AGENTS.md, CLAUDE.md (4.2 KB)`.
- `agent.Task.NoInstructions bool` (`json:"no_instructions,omitempty"`); flag
  `hx run --no-instructions`; batch spec field `no_instructions`; MCP `run` argument
  `no_instructions`.
- README: a short "Project instructions" paragraph under *api tools*.

**Tests** (`api_test.go`, fake server): workspace with `CLAUDE.md` → first request's
`system[1].text` contains the file content and the `## CLAUDE.md` heading; two files →
both, in order; no file → system has two blocks; `NoInstructions` → two blocks; a
40 KiB file → truncated marker present and `len(text) <= 32*1024 + len(marker)`.

**Acceptance.** All of the above green on both OSes; `hx run` prints the info line.

**Out of scope.** Walking parent directories; `.hx/` per-user files; nested
`CLAUDE.md` in subdirectories.

Size: ~120 lines + tests.

### [x] PR B: retries that keep the work and carry the failure

Done: PR #5, 2026-09-21.

**Why.** `batch.runTask` retries by deleting the worktree and starting from scratch on
the *next* backend. That throws away everything the first attempt did and cannot retry
on the same backend. A failing gate is best fixed by a second attempt on the same
backend that sees the gate output and the changes already made.

**Decisions made.**
- `Retry` entries stay backend names; the same name may repeat
  (`"retry": ["api", "api", "claude"]`). Default stays *no retry*. README recommends
  `"retry": ["<same backend as backend>"]`.
- Attempt `i > 0` is either **continued** or **fresh**:
  - *continued* when the previous attempt's run status was `ok` and a gate failed: keep
    the worktree and branch (the previous work is already committed on the branch);
    prompt = original prompt + failure context (below).
  - *fresh* otherwise (run status `error`, `refused`, `max_turns`, `timeout`, `budget`,
    `canceled`): remove worktree and branch, recreate from `base` (current behaviour);
    prompt = original prompt + one line
    `A previous attempt on <backend> ended with <status>: <error>.`
- Failure context, exact format, appended to the original prompt:

  ```

  ---
  A previous attempt at this task (attempt 1 on api) did not pass. Its changes are
  already applied in this workspace and committed on the current branch. Fix them
  rather than starting over.

  The gate command `go test ./...` exited 1. Output (tail):
  ```
  <GateResult.Output>
  ```
  ```

  `GateResult.Output` is already the last 4000 bytes.
- Commit message for a continued attempt: `<id>: <prompt> (attempt N)`.
- Record the prompt actually sent: `Attempt.Prompt string` and `Attempt.Continued bool`
  (both `omitempty`). The eval layer (PR G) reads these.
- Notify `retry` event text: `retrying on api (continuing in worktree)` or
  `retrying on claude (fresh worktree)`.

**Changes.** `internal/batch/batch.go` `runTask` (split the attempt body into
`runAttempt` so the function stays readable), `Attempt` struct, README *batch* section.

**Tests** (`batch_test.go`, stub backends already there): a stub that records the
prompt it received; gate command `test -e passed.flag || { touch passed.flag; exit 3; }`
(Git Bash exists on the Windows runner) → attempt 2 has `Continued == true`, same
worktree path, `Prompt` contains `exited 3` and the gate output, and attempt 1's commit
is an ancestor of the final commit (`git merge-base --is-ancestor`). A run-error first
attempt (`wb-bad`) followed by `Retry: ["wb-ok"]` → attempt 2 `Continued == false`,
prompt contains `ended with error`. `TestFailuresRetryOnNextBackend` keeps passing.

**Acceptance.** As above; `hx batch --json` shows `prompt` and `continued` per attempt.

Size: ~150 lines + tests.

### [x] PR C: budgets (time, cost, task budget)

Done: PR #6, 2026-09-21.

**Why.** `MaxTurns` is the only limit on a run. One Opus 5 / Fable turn can take many
minutes; a 50-turn run re-sends the whole history every turn. Batches with 10 tasks
need a ceiling per task in time and money.

**Decisions made.**
- `agent.Task` gains `Timeout time.Duration` (`json:"timeout_ns,omitempty"`),
  `MaxCostUSD float64` (`json:"max_cost_usd,omitempty"`), `TaskBudget int`
  (`json:"task_budget,omitempty"`, tokens).
- New statuses: `StatusTimeout = "timeout"`, `StatusBudget = "budget"`.
- Timeout: callers (`cmdRun`, `hxmcp` run tool, `batch.runAttempt`) wrap `ctx` with
  `context.WithTimeout` when `Timeout > 0`. A single helper
  `agent.stopStatus(ctx) Status` returns `StatusTimeout` for `DeadlineExceeded` and
  `StatusCanceled` for `Canceled`; `api.go` and `cli.go` use it wherever they
  currently return `StatusCanceled`.
- MaxCost: `api` loop checks after each turn; when `res.CostUSD >= MaxCostUSD` it stops
  with `StatusBudget` and error `cost estimate $0.83 reached --max-cost $0.80 after 7 turns`.
  CLI backends only learn the cost at the end (claude) or never (others), so they emit
  one `EventInfo` (`max_cost is not enforced by the claude backend`) and continue.
- TaskBudget: `api` only. Request gets
  `output_config.task_budget = {"type": "tokens", "total": N}` and beta header
  `task-budgets-2026-03-13`. Validate `N >= 20000` where the value enters
  (`cmdRun`, the MCP `run` tool, `batch.normalize`), not inside the loop. `output_config` already carries
  `effort`; merge, do not overwrite.
- Flags: `hx run --timeout 20m --max-cost 2.50 --task-budget 64000`. Batch spec and
  task fields `timeout` (duration string, parsed with `time.ParseDuration`),
  `max_cost_usd`, `task_budget`. MCP `run` arguments with the same names (`timeout` as
  a string).
- Retry logic from PR B: `timeout` and `budget` count as run failures → fresh attempt.

**Tests.** Fake server that sleeps 2 s with `Timeout: 200ms` → `StatusTimeout`; fake
usage of 1M input tokens on `claude-opus-5` with `MaxCostUSD: 1` → `StatusBudget` after
turn 1; request JSON carries `task_budget` and the beta header; `task_budget: 100`
rejected at the CLI with a clear message; batch spec `"timeout": "5m"` parses.

**Acceptance.** As above; README *Run flags* and *batch* updated; `hx runs` shows the
new statuses.

Size: ~180 lines + tests.

### [x] PR D: server-side context management

Done: PR #8, 2026-09-21.

**Why.** Tool results are capped at 30 KB each, so a long run reaches hundreds of
thousands of tokens per request. Prompt caching keeps it affordable, not fast, and
eventually the context fills. The API can clear old tool results itself.

**Decisions made.**
- Default on for the `api` backend: request field
  `"context_management": {"edits": [{"type": "clear_tool_uses_20250919"}]}` and beta
  header `context-management-2025-06-27` (append to `req.betas`; the header is a
  comma-joined list and may already carry the fallback beta). Server defaults for
  trigger and keep-count are accepted in v1.
- Opt out with `agent.Task.NoContextEdits bool` / `hx run --no-context-edits` / batch
  `no_context_edits` / MCP argument. Keep the switch: if the real API rejects the
  combination with another beta, Step 0 will show it and the flag is the escape hatch.
- History stays append-only client-side (rule 8). Any `context_management` object in
  `message_delta` is ignored by `parseSSE` (it already ignores unknown fields; add a
  comment saying so).
- Note in README: a clear invalidates the cached prefix from that point; that is
  expected.

**Tests.** Fake server: default request has the field and the header contains both
betas; `NoContextEdits` → neither the field nor that beta.

Size: ~40 lines + tests.

### [x] PR E: save the conversation and continue a run

Done: PR #9, 2026-09-22.

**Why.** `events.jsonl` is a transcript for humans; the API message list is not saved,
so a finished run cannot be continued ("now fix the failing test"). MCP hosts and the
eval layer both want follow-ups without re-reading the whole repo.

**Decisions made.**
- `agent.Task.RunDir string` (`json:"-"`): callers that hold a `runlog.Writer` set it
  to `rec.Dir()`. The `api` loop, when `RunDir != ""`, writes `messages.json` there
  after the final turn: `{"model": ..., "messages": [...]}` with the exact `req.Messages`.
  Written once at the end (not per turn) to keep the loop simple; a killed run has
  no `messages.json`.
- `agent.Task.Resume *Conversation` (`json:"-"`), `Conversation{Model string; Messages []map[string]any}`.
  The `api` loop seeds `req.Messages` from it and appends the new user message.
- `hx run --continue <run-id> "<follow-up>"`: loads `task.json` and `messages.json`,
  creates a **new** run record (new id, `task.json` gets `"continues": "<old id>"`),
  reuses backend, dir, mode, model unless overridden by flags. `api` only; other
  backends error with `continue is supported for the api backend only`.
- Model rule: thinking blocks are bound to the model that produced them. Same model by
  default; `--model` different from the record's model is allowed but prints a warning
  that earlier thinking is dropped by the API.
- MCP `run` tool gains `continue_run_id`.
- `runlog.Record` gains `Continues string` (`omitempty`).

**Tests.** Fake server: run 1 (tool turn + text turn) writes `messages.json` with 4
messages; `--continue` run's first request has those 4 plus the new user message, in
order, and the assistant blocks are byte-identical to what the fake sent. Non-api
backend → error.

Size: ~200 lines + tests.

### [x] PR F: `hx report`

Done: PR #7, 2026-09-21.

**Why.** The run records exist so runs can be compared. Nothing reads them in
aggregate yet.

**Decisions made.**
- `hx report [--since 7d] [--by backend|model|dir] [--json]`. Reads every run
  (`runlog.List(0)` returns all) and every batch record.
- Table columns per group: runs, ok, ok %, median duration, median turns, total cost,
  median cost, cache share (`cache_read / (input + cache_read)`, blank when both are 0).
  A final *all* row. Costs that are estimates are marked `~` in the total when any run
  in the group is an estimate.
- Batches: a second table: batches, tasks, pass %, median attempts, total cost of all
  attempts.
- `--since` accepts Go durations plus `d` (days).
- Put the aggregation in `internal/report` (pure functions over `[]runlog.Record` and
  `[]batch.Result`) so PR G can reuse `median`, `groupBy`, `costOf`.

**Tests.** Temp `HX_HOME` with 4 hand-written run records and 1 batch record; assert
the numbers of the `api` and `claude` rows and the *all* row; `--since` filtering;
`--json` shape.

Size: ~250 lines + tests.

### [x] PR G: `hx eval` (Fable, or Opus with a Fable review of the design)

Done: PR #12, 2026-09-22.

**Why.** This is the third harness from the original brief: run a task set across
backends, models and efforts, repeatedly, and score with deterministic gates first.
It answers "which backend and effort for this kind of task" with data.

**Decisions made.**
- An eval is a batch with a matrix and repeats. Spec:

  ```json
  {
    "dir": ".", "parallel": 3, "mode": "yolo",
    "gate": ["go build ./... && go test ./..."],
    "matrix": [
      {"name": "opus-high", "backend": "api", "model": "claude-opus-5", "effort": "high"},
      {"name": "opus-low",  "backend": "api", "model": "claude-opus-5", "effort": "low"},
      {"name": "claude",    "backend": "claude"}
    ],
    "repeats": 3,
    "timeout": "20m", "max_cost_usd": 3,
    "tasks": [{"id": "vet", "prompt": "...", "gate": ["..."]}]
  }
  ```

  `name` defaults to `<backend>[-<model>][-<effort>]`.
- Expansion: `internal/eval.Expand(spec) (batch.Spec, error)` produces tasks with ids
  `<evalid>.<task>.<variant>.r<n>` (`evalid` = 5 hex chars, so branches from different
  evals never collide; `idRE` allows `.`). `Retry` is **nil** for every expanded task:
  an eval measures single attempts. Everything else (gates, mode, timeout, budgets,
  mcp) is copied through.
- Run through `batch.Run` unchanged. Then aggregate per variant: attempts, pass rate,
  run-status histogram, median and mean cost, median duration, median turns, median
  input tokens. Also per task × variant pass counts (`2/3`).
- Save `$HX_HOME/evals/<evalid>.json` = `{spec, batch_id, started, table}`. Print the
  table; `--json` prints the file. `hx eval show <evalid>` reprints.
- Worktrees: after the record is saved, `batch.Remove` unless `--keep`; a remove
  failure is reported, not fatal.
- Judge (LLM scoring of the diff against a rubric): **out of scope for v1.** Reserve the
  spec key `"judge"` and say so in the README.

**Tests.** `Expand` unit test (ids, nil retry, field copy-through); end-to-end with the
stub backends from `batch_test.go` (2 tasks × 2 variants × 2 repeats, one variant
always fails its gate) asserting pass rates and that worktrees are gone afterwards;
`--keep` keeps them.

Size: ~400 lines + tests.

### [x] PR H: approvals under `hx mcp` via elicitation (Fable)

Done: PR #14, 2026-09-22.

**Why.** Under Claude Code, hx's `ask` mode denies everything because there is no
terminal. MCP (2025-06-18) lets a server ask the host to ask the user
(`elicitation/create`). That makes `ask` a real mode in the place hx will run most.

**Decisions made.**
- `internal/mcp.Server` stores the client's capabilities from `initialize`
  (`protocol.go:166` already decodes them) and exposes `ClientSupports(cap string) bool`.
- The server gains server→client requests: an id counter, a pending map, and
  `Server.Request(ctx, method, params, out any) error` that writes the request through
  the codec and waits for the matching response or `ctx.Done()`. Responses are routed
  in the read loop by `id` before the request dispatch (mirror `client.go`).
- `hxmcp` run tool: when `mode == ask` and the client supports `elicitation`, set
  `Task.Approve` to a function that sends `elicitation/create` with
  `message: "hx wants to run <tool>: <summary>"` and
  `requestedSchema: {type: object, properties: {approve: {type: boolean, title: "Allow"}}, required: ["approve"]}`;
  `action == "accept" && content.approve == true` allows; anything else (decline,
  cancel, error, 5-minute timeout) denies. Approvals are serialized (one at a time) so
  prompts never interleave; `api.go` already decides approvals up front and in order.
- Without elicitation support, behaviour is unchanged (deny). Update the server
  `instructions` text accordingly.

**Tests.** `mcp_test.go`: fake client that answers `elicitation/create`; a tool that
calls `Server.Request` gets the answer; timeout path. `hxmcp/server_test.go`: `ask`
run with an approving client executes a write; with a declining client it does not.

Size: ~300 lines + tests.

### [x] PR I: command guard and dirty-repo check

Done: PR #11, 2026-09-22.

**Why.** `yolo` mode and batch gates run whatever they are given. A few patterns are
never what the user meant. And `hx batch` branches worktrees from `HEAD`, so
uncommitted changes in the main checkout silently never reach the agents.

**Decisions made.**
- `internal/tools/guard.go`: `Guard(command string) (reason string)` with a short,
  documented list, each a named regexp:
  `git-push` (`\bgit\s+push\b`), `sudo` (`\bsudo\b`), `rm-root`
  (`\brm\s+-[A-Za-z]*r[A-Za-z]*\s+(/|~|\$HOME|[A-Za-z]:\\)(\s|$)`), `pipe-to-shell`
  (`\b(curl|wget)\b[^|]*\|\s*(sh|bash|zsh|powershell)\b`), `disk` (`\b(mkfs|format\s+[A-Za-z]:|dd\s+if=)`).
  Applied by `run_command` (the `api` loop's tool) before execution; blocked calls
  return an error tool result:
  `blocked by hx command guard (git-push); rerun with --allow-dangerous if intended`.
  Not applied to batch gate commands (the spec author wrote those).
- `agent.Task.AllowDangerous bool`, flag `hx run --allow-dangerous`, batch/MCP fields.
  `yolo` alone does not bypass the guard.
- `hx batch` refuses to start when `git status --porcelain --untracked-files=no` in the
  repo root is non-empty, with
  `the repository has uncommitted changes; the agents would not see them. Commit or stash, or pass --allow-dirty`.
  Flag `--allow-dirty`, spec field `allow_dirty`.

**Tests.** Table test over ~15 command lines (blocked and allowed, including
`git push` inside a string that is not a command, e.g. `echo "git push"` — accept the
false positive and document it); `run_command` returns the error result; batch dirty
test with a modified tracked file.

Size: ~120 lines + tests.

### [x] PR J: `hx batch show`, `diff`, `merge`

Done: PR #13, 2026-09-22.

**Decisions made.**
- `hx batch show <id>`: prints the record in the end-of-run table format; works while a
  batch is still running (the record is saved first; statuses show `running`).
- `hx batch diff <id> <task>`: `git diff <base>..<commit>` to stdout, or the worktree's
  uncommitted diff when there is no commit.
- `hx batch merge <id> [task ...]`: from the repository's main worktree, which must be
  clean; merges each passing task's branch (all passing tasks when none are named), in
  spec order, with `git merge --no-ff --no-edit hx/<task>`; on conflict runs
  `git merge --abort`, reports which task, and stops. `--squash` does `git merge --squash`
  + `git commit -m "<id>: <prompt>"`. Does not push.

**Tests.** Stub batch with two passing tasks touching different files → merge yields
two merge commits on the current branch; two tasks touching the same line → first
merges, second aborts cleanly, the tree is clean afterwards.

Size: ~200 lines + tests.

### [x] PR K: code health

Done: PR #15, 2026-09-22.

- Split `internal/agent/cli.go` into `cli.go` (the `CLIBackend` core and shared
  helpers) plus `claude.go`, `codex.go`, `gemini.go`, `opencode.go`. No behaviour change.
- CI: on `ubuntu-latest` add a step `go test -race -count=1 ./...` (the runner has
  cgo). Keep the plain test step on both OSes.
- Fuzz tests with `testing.F`: `FuzzParseSSE` (`internal/agent`, feed random event
  streams, must never panic) and `FuzzCodec` (`internal/mcp`, random framed messages).
  CI runs each for 10 s on ubuntu: `go test -run=^$ -fuzz=FuzzParseSSE -fuzztime=10s ./internal/agent`.
- `hx version` and `hx --version` print `agent.Version` and the Go version.

Size: mostly moves; ~100 new lines.

### [x] PR L: one options struct

Done: PR #16, 2026-09-22.

**Why.** Every api option (`model`, `mode`, `max_turns`, `effort`, `mcp`, the three
switches, the three budgets) was wired by hand in four places (`agent.Task`, the run
flags, `batch.Spec` and `batch.Task`, the MCP decode struct) and described in a fifth
(the MCP schema). PRs A, C, D and I each added a field to all of them; a field forgotten
in one was silent. The first thread in *What is open*.

**Decisions made.**
- `agent.Options` holds the eleven options with `json`, `desc` and (where the flag name
  differs) `flag` tags. `agent.Task` embeds it, so `task.Mode` and the record's JSON
  keys are unchanged. `batch.Spec` and `batch.Task` embed it; the MCP decode struct
  embeds it.
- Derived, not repeated: `BindFlags` registers one flag per field (`hx run` uses it and
  adds `-m`), `Schema` generates the MCP properties from the same tags (the run tool adds
  `prompt`, `backend`, `dir`, `continue_run_id` and an MCP note on `mode`), `Over` merges
  a task's options over its spec's by reflection (zero inherits, so switches OR and an
  empty list stays empty), and `Validate` is the one check every entry point calls.
  `TestEveryOptionHasAFlagAndASchemaEntry` fails on a field that any of them misses.
- `Timeout` is `agent.Duration`: `"20m"` in a spec, a flag or an MCP call, written the
  same way in records, a JSON number accepted as nanoseconds. Records written before
  this (`timeout_ns`) load through `Task.UnmarshalJSON`. The type lives on the field,
  not on `Options`, because a struct with its own `MarshalJSON` cannot be embedded
  without hiding the outer type's fields.
- A task may now set `mcp` itself; `checkLimits`, the per-kind `first*` pickers and
  `multiFlag` are gone.

**Tests.** `options_test.go`: every field has a flag, a schema entry and a desc; flags
set every field and refuse a bad mode or duration; `Over` for every kind; `Validate`;
`Duration` both ways; a legacy `timeout_ns` record. The existing suites cover the
entry points; two invalid-duration cases moved from Go literals to spec JSON, which is
where such a typo lives.

## Definition of done (every PR)

- [ ] `go vet ./...`, `go build ./...`, `go test -count=3 ./...`, `gofmt -l .` empty, locally.
- [ ] `/code-review` on the PR; every finding fixed as its own commit or explicitly
      rejected in a PR comment with a reason.
- [ ] CI green on ubuntu and windows.
- [ ] README updated for any new flag, field, command or status.
- [ ] This file: box ticked, `Done: PR #n, <date>` under the PR heading, deviations in
      the *Decisions log*.
- [ ] Merged with a merge commit; branch deleted.

## Model guidance

- **Opus 5, effort `xhigh`** for A–F, I–K. They are specified down to field names and
  test cases; what remains is careful implementation.
- **Fable 5.1** for G (the eval design has judgment calls: what to aggregate, how
  variants are named, what a "pass" means with repeats) and H (server→client requests
  are the one place where a protocol mistake is hard to see in tests). Opus can do
  them with a Fable review of the PR before merge.
- Whichever model runs a PR: read this file, the README, and the files named in the PR
  before writing code. Do not reopen the *Decisions made*; if one turns out to be
  wrong in the code, note it in the *Decisions log* and do the smallest thing that
  works.

## What is open

Every lettered increment is done. These are the threads the log leaves behind, in the
order they are worth taking:

1. ~~**One options struct.**~~ Done as PR L (#16).
2. **Measure the context-editing default.** Server-side clearing is on by default and its
   interaction with the two cache breakpoints is unmeasured; a clear invalidates the
   cached prefix from that point. One long api run with and without it, compared on
   `cache_read_tokens`, settles whether the default is right.
3. **Record what the server cleared**, so an eval can attribute a token difference. The
   response shape is not documented in what is available here; do it with the first real
   api run.
4. ~~**The eval judge.**~~ Done as PR M, with `expect`, a tool-call column and the count
   of passes that changed nothing.
5. **A real api run.** The backend has never run against the API: no credentials on this
   machine. Everything in it is covered by a fake server. Items 2 and 3 wait on it, and it
   should come before anything else that touches the api backend.
6. **A `batch` tool in `hrn mcp`.** Claude Code can delegate one task through `run` but
   cannot fan out; the batch package is done, so this is plumbing: the tool takes a spec
   as JSON, runs it with progress notifications, and returns the batch record.
7. **Release binaries.** A workflow that builds hrn for linux, windows and darwin on a
   `v*` tag and attaches them to the release, so installing is not `go build`.
8. **`hrn eval diff <a> <b>`.** Two saved evals of the same task set, side by side per
   variant and task, for "did this model or prompt change help".

Left out on purpose: real sandboxing for yolo (a project, not a PR; the guard says it is a
guard rail), a daemon or queue (batch plus cron or the MCP tool covers it), MCP over HTTP
(every host met so far speaks stdio), and a web dashboard (report and batch show answer
the questions).

## Decisions log

- 2026-09-23: PR M done: the eval judge, and the three eval columns Step 0 asked for.
  `judge` is a struct now (backend, model, effort, rubric, rubrics per task, timeout),
  checked in Expand so a task without a rubric fails before any agent starts. Only
  passing cells are judged; a verdict that cannot be read is an error in the record, never
  a zero, so a flaky judge cannot drag a mean down. The judge runs read-only in the cell's
  worktree with the diff, the answer and the rubric in its prompt, and is recorded as an
  ordinary run. `expect` went into batch rather than eval: it is a gate on the answer, is
  reported as `expect /re/` in the gate list, and so reaches retries through the existing
  retry context with no new branches. Tool calls are counted by a new `agent.Run` wrapper
  that every caller uses (run, batch, MCP), not by each adapter, so the count is the same
  on every backend. Verified on a real eval, claude backend with haiku as agent and
  judge: a typo task (with British spellings as bait) and a yes/no task with expect. Both
  passed and scored 10, the "1 of 2 passes changed nothing" note named the yes/no task,
  and the judge kept to one line of JSON. Found by that run: the two verdicts cost $0.53
  against $0.05 for the agents, because Claude Code cache-writes a 64k-token system
  prompt for plan mode and both judges started before the cache existed. Documented, not
  worked around; a larger eval amortises it. Also fixed on the way: the README's open list
  still named the options struct (done in L) and called the old binary `hrn`.
- 2026-09-23: PR M self-review, 1 finding, fixed. An eval spec is loaded without batch's
  normalisation, so an unset `parallel` reached the judge as 0 and it judged one cell at a
  time while the batch had run two. The default is `batch.DefaultParallel` now, and a test
  with a judge that waits for a second verdict fails in the old way when the fix is
  reverted. Checked and sound: cancellation (a cancelled verdict is an error, never a
  zero), the verdict parser's backward scan, rune-safe clipping, and that every caller
  goes through `agent.Run`.
- 2026-09-22: open-source prep (#17). The binary is renamed from hx to hrn: hx is the Helix
  editor's binary, so nobody with Helix on PATH could have installed this. Every word in
  the harness vocabulary is already an agent tool on GitHub (harn, bridle, reins, tack, yoke
  five times over, halter; hame has hames next to it), so the name is the repo's own name
  shortened, unclaimed as a binary, three keystrokes. Everything spelled with the name moved
  with it (cmd/hrn, hrnmcp, HRN_HOME, ~/.hrn, .hrn/worktrees, hrn/<id> branches, HRN.md);
  the module path stays, and this log keeps "hx" in the entries written before today
  because they describe what happened. LICENSE is MIT. The README gained an install
  section, a per-backend status column that says which paths a real agent has actually
  been through, the open list in place of a roadmap everything on which had shipped, and a
  contributing section with the three rules. Local state to migrate by hand: ~/.hx to
  ~/.hrn, the Claude Code registration (claude mcp remove hx; claude mcp add hrn -- hrn mcp),
  and any project HX.md, which is read as HRN.md now. For a branch that predates the rename
  (PR L is open as this merges): merge main, then on each conflicted file take the branch's
  side and re-run the substitution, hxmcp to hrnmcp first, then hx/HX/Hx to hrn/HRN/Hrn where
  no letter or digit touches them. Git's directory-rename detection moves cmd/hx and
  internal/hxmcp edits to the new paths by itself.
- 2026-09-22: PR K self-review, 7 findings, all fixed. The fuzz step ran both fuzzers in
  one shell command, so a crash in the first hid the second; they are separate steps now,
  the second with always(), and a failure uploads whatever input the fuzzer saved, which
  the runner would otherwise delete. The recorder channel in the new cmd tests is drained
  per test, so one test cannot read another's task. The SSE fuzzer asserts invariants (a
  parsed stream returns a response, usage never goes negative) rather than only the
  absence of a panic. cli.go says what it now holds and where each backend went. The
  command tests cover what the other five commands refuse, not only hx run. And the plan
  has a "what is open" section, because every box being ticked left no queue.
- 2026-09-22: PR K done (#15), the last lettered increment. cli.go is split per backend
  (cli.go keeps the CLIBackend core and the shared helpers; claude.go, codex.go, gemini.go
  and opencode.go take one adapter each), with no behaviour change. CI on ubuntu now also
  runs the race detector and ten seconds each of FuzzParseSSE and FuzzCodec; both fuzzers
  ran five seconds clean locally first. hx version prints the hx and Go versions. The
  deferral carried since PR A is closed: cmd/hx has tests, and they cover the real path
  rather than a refactored stand-in - cmdRun is called with arguments against a stub
  backend that keeps the task it was handed, so every run flag is asserted to reach
  agent.Task, plus five rejected flag combinations. Still open, deliberately: the shared
  options struct that would stop each api option being wired by hand in four places. It
  changes batch.Spec, batch.Task and the MCP decode struct together, which is its own
  increment rather than a tail-end refactor.
- 2026-09-22: PR H done (#14). Deviation: Request and ClientSupports are on mcp.Call, not
  on Server as the spec said, because the connection state belongs to one connection
  while a Server can serve several, and the handler already holds the Call.
- 2026-09-22: PR H self-review, 7 findings, all fixed. The worst was mine and pre-existing
  in shape: Serve registered `defer cancel()` before `defer wg.Wait()`, so on exit it
  waited for in-flight handlers and only cancelled them afterwards. That never mattered
  while no handler blocked on the client; with approvals it means a host that disappears
  mid-prompt hangs the server until the approver's own five-minute timeout. Fixed by
  cancelling first, and a test that closes the client mid-question fails in exactly the
  old way (5s timeout, "waited for a handler it had not cancelled") when the fix is
  reverted. Also: the client now cancels an elicitation the server withdrew, instead of
  leaving the question on screen; the prompt carries the whole summary rather than 200
  characters, because a truncated command hides the part that makes it dangerous; the
  prompt names the run, since two runs can ask at once; each decision goes back as
  progress; Call.Request documents that it waits on ctx alone; and a test asserts what
  the person is actually asked rather than only the outcome.
- 2026-09-22: PR G done (#12). Deviations from the spec: Expand takes the eval id as an
  argument instead of generating one, so Run owns the id and Summarize can rebuild a
  table from a record loaded later; Run returns the batch.Result alongside the record and
  the CLI decides whether to remove the worktrees, which keeps the package free of a
  --keep flag; and a failing variant exits 0 (data, not an error) while a failing eval
  exits 1. Verified on a real comparison rather than stubs: one task, sonnet against
  haiku, gate demanding a file containing exactly "hx rocks". Sonnet wrote "hx rocks."
  with a trailing period and failed; haiku passed at a third of the cost. The run records
  still explain it, which is the argument for keeping them.
- 2026-09-22: PR G self-review, 7 findings, all fixed. The largest: Expand validated
  neither task ids nor variant names, and Summarize reads a cell id by splitting on dots,
  so a dotted task id (batch allows them) silently attributed every cell to a row that
  matches nothing - the table would print a confident zero. Both are now checked where the
  spec is expanded. Also: retry in an eval spec is refused rather than silently dropped;
  hx eval exits 1 when the eval itself failed; a drawn eval id that already names a saved
  record is re-drawn; the judge key says at the start that it does nothing; the input
  column is named context because it counts cache reads too; and slices.Contains replaces
  a hand-written helper.
- 2026-09-22: PR E overwrote internal/runlog/runlog_test.go instead of adding to it. PR F
  had created that file in the same increment window and my session wrote it with a shell
  redirect, so TestListSinceSkipsOldRunsAndCountsUnreadable and TestIDBefore were silently
  deleted and CI stayed green because what remained still passed. Restored in PR #10, both
  sets of tests side by side. Rule for parallel sessions: never create a file with `>` or
  Write without checking whether it exists first, and treat a merge diff that touches a
  file you thought was new as a red flag - the merge stat showing 94 changed lines in a
  "new" test file is what caught it.
- 2026-09-22: PR E done (#9), no deviation from the spec. Two things it did not mention:
  a history ending on an assistant turn whose tool calls were never answered is dropped
  before saving, because the API refuses to resume from one (it happens on a max_tokens
  truncation); and the workspace inherited from the earlier run is validated like any
  other rather than trusted. Self-review, 7 findings, 6 fixed: an early failure used to
  write messages.json with a null message list, which made a dead run look continuable;
  run ids now go through one validID check in runlog (Load, Events, LoadConversation),
  where only batch and the MCP show tool had one, and the test that "proved" a path-like
  id was refused had been passing only because no file happened to exist there; a resume
  takes the model the earlier task asked for rather than the one the API reported, which
  could be decorated; the conversation is written compact and the README says a run now
  costs about twice the disk; hx show and the MCP show tool print what a run continues;
  and agent.Message is an exported alias so another package can build a Conversation.
  Deferred again: cmd/hx still has no tests, so the --continue inheritance rules are
  unverified - third increment in a row, and PR K should end it.
- 2026-09-21: plan written. Step 0 not yet done; PR order may change after it.
- 2026-09-21: Step 0 done. Prerequisite used: the desktop-bundled `claude.exe` logged in
  interactively (`/login`); hx finds it without PATH changes. Three smaller smokes passed
  first on the claude backend: `hx run` on this repo; Claude Code -> `hx mcp` run tool ->
  claude (hx registered with `claude mcp add --scope local`, `claude mcp list` shows it
  Connected); `hx batch` in a throwaway repo. Then the prescribed JobPilotC batch, with
  `model: sonnet` instead of the default opus to keep cost down: 3/3 passed in 1m23s at
  about USD 1.03 list price, 26-60 s per task. What the agents did: `api-vet` found nothing
  to fix (pass, no commit, 3 turns); `small-test` added a 33-line table-driven test for
  `isRoleTitleLine` (14 turns, green); `readme-typos` changed two British spellings to
  American (Behaviour, re-emphasises) - not typos, and the build gate cannot see that.
  Lessons: (1) gates verify the build, not the intent: PR G should add a per-task `expect`
  regex on the final answer, and the judge stays necessary for diffs; (2) a pass with no
  commit is a distinct outcome eval should count separately; (3) the claude backend spent
  eight consecutive Grep calls on one file, so tool-call count is worth a column next to
  turns; (4) a 12-turn run read about 1M cached tokens, so cost is dominated by Claude
  Code re-reading its context and per-task `max_cost_usd` (PR C) matters more than
  `max_turns`. The branches hx/readme-typos, hx/api-vet and hx/small-test are left in
  JobPilotC for Cristian to inspect or merge; `hx batch rm 20260921-100459-f3b353`
  removes them. PR order unchanged.
- 2026-09-21: two fixes outside the lettered list, found by the smokes, shipped as PR #3
  together with this file: the Claude stream parser records the result text when
  `is_error` arrives with subtype `success` (a login failure read as "success" before),
  and `hx mcp --dir PATH` sets the default workspace for hosts that do not start servers
  in the project folder. Scripting note: give `claude -p` its prompt on stdin, because
  `--allowedTools` is variadic and swallows a trailing prompt argument.
- 2026-09-21: PR A done (#4). Deviations: none in the spec itself. Additions the spec did
  not name: an unreadable instruction file is reported as an error event and the run
  continues (a directory named CLAUDE.md was the test case); truncation trims back to a
  rune boundary, because a byte-wise cut of multi-byte content would send invalid UTF-8;
  a file that is empty after trimming is skipped rather than rendered as a bare heading;
  and the batch and MCP pass-through is asserted through the run record. The info line
  could not be seen in a real run: the api backend still has no credentials.
- 2026-09-21: PR A self-review, 8 findings. Fixed: an instruction file that is a symlink
  out of the workspace is now refused through `tools.Workspace.Resolve` (the model-facing
  tools already did this, loadInstructions did not, so a repo could have linked CLAUDE.md
  at a file outside the tree); truncation is reported to the operator, not only marked
  inside the prompt; a section that cannot fit its heading is dropped whole instead of
  cut mid-heading; a file that contributed nothing is no longer listed as loaded;
  `no_instructions` is also a batch task field (OR with the spec switch); the file list
  is a function, not a mutable package variable. Deferred, on purpose: (1) the run record
  still does not say which instruction files were sent or what they contained, so two runs
  of one task with different house rules look identical to a comparison - PR G needs this
  and should add it to the record then, not here; (2) the `--no-instructions` CLI flag has
  no test that it reaches `agent.Task`, because `cmd/hx` has no tests at all and every
  other flag is in the same position - a first test for that package belongs with PR K.
- 2026-09-21: PR B done (#5). Deviation from the spec: the fresh-retry prompt gets a
  second line ("This workspace is a fresh copy; nothing from that attempt is in it."),
  because one line saying how the attempt ended does not tell the agent that its previous
  changes are gone. Verified with a live agent, not only stubs: a task to write a greeting
  with a gate demanding the exact line `hx rocks`; attempt 1 wrote a friendly sentence and
  failed, the retry continued in the same worktree, read the gate output from its prompt
  and fixed the file, and the branch holds both commits with the second marked
  "(attempt 2)".
- 2026-09-21: PR B self-review, 7 findings, all fixed. The retry context now takes every
  attempt so far and appends a one-line history of the earlier ones, so a third attempt
  does not repeat what the second already tried; `retryContext` returns the context text
  and the continued flag instead of a half-built plan the caller had to repair; the
  previous attempt is read from the end of the slice rather than by loop index; the commit
  subject is built in the plan so `runAttempt` no longer takes two near-identically named
  task parameters; the gate output is quoted with a fence longer than any run of backticks
  in it; and a test covers two retries in a row. A wrong assertion in that test (the gate
  command text contains the same words as its output) was corrected, not worked around.
- 2026-09-21: PR C done (#6). Deviation: the spec had the callers wrap the context with
  the timeout; the backends do it instead (`withTimeout` in agent.go), because every
  other Task field is honoured inside Run and a future caller cannot forget. The error
  text is neutral ("cost estimate $0.83 reached the $0.80 limit after turn 7") rather
  than naming --max-cost, since batch and MCP users never type that flag. Checked against
  the current API docs rather than memory, which caught what the spec did not say:
  `task_budget.remaining` must stay unset while the client resends full history, because
  the server tracks the countdown and a client-computed value under-reports it; a test
  asserts the field is absent. Task budgets are Opus 5 / Fable 5 / Sonnet 5 / Opus 4.8 /
  4.7 only, so an unsupported model returns a 400 that hx surfaces as a run error.
- 2026-09-21: PR C self-review, 7 findings, all fixed. The largest: --max-cost was silently
  inert on any model missing from the hard-coded price table, because the estimate is
  zero there; the run now says so up front. Also: one ValidateLimits replaces
  ValidateTaskBudget and rejects negative durations and costs, which were accepted and
  then ignored; batch parses each timeout once in normalize and carries the duration, so
  no second parse can turn a typo into "no deadline"; MaxTurns now inherits through the
  same firstInt helper as the new fields; the CLI warning reads "max_cost, task_budget:
  not enforced by the claude backend" instead of an ungrammatical singular; and a batch
  test asserts that a timed-out attempt retries in a fresh worktree. The per-task rule
  (zero means inherit, so a task can tighten or change a limit but not remove it) is
  documented in the struct and the README rather than given a sentinel.
- 2026-09-21: PR D done (#8), no deviation from the spec. Self-review, 7 findings, 3 fixed:
  a 400 that names the beta or the field now drops both and retries the turn once, because
  a beta hx sends by default must not be able to break every api run; the comment and
  README no longer claim the request body shrinks (hx still uploads the whole history -
  what the clearing changes is the context the model is given, so the window and the
  billed input, not the upload); and the usage block alignment is fixed. Deferred, with
  reasons: (1) the cache interaction is unmeasured - a clear invalidates the cached prefix
  from that point, and hx resends full history with two breakpoints, so this could raise
  cost rather than lower it; the run records already carry cache_read_tokens, so one real
  long run with and without it settles the question, and until then the README says so.
  (2) Nothing records whether the server actually cleared anything, because the response
  shape for it is not documented here and guessing a field that never increments would be
  a quiet lie; do it when the api backend first runs for real. (3) Every api option is
  still wired by hand in four places (Task, run flags, batch spec and task, MCP schema and
  decode) - PR E adds more fields, so one shared options struct belongs with PR K rather
  than mid-queue. (4) The CLI flag wiring is still untested, the same gap as PR A, now
  covering two flags and five limits; PR K.
- 2026-09-21: PR F done (#7). Built in a second git worktree (`E:\__Workdir\harness-f`,
  branch `f-report`) because another session was on PR C in the main checkout at the same
  time; two agents in one working tree would overwrite each other. Choices the spec left
  open: batch rows group by repository (the spec named the columns, not the rows); a group
  with no token counts reports cache share -1 in JSON and `-` in text; `--by` defaults to
  backend; unfinished runs are counted and listed under the tables, not in them; a cost is
  marked `~` when any run in the group carries a non-zero estimate. `batch.List` was added
  for the batch table. The smoke against the real `~/.hx` showed long repository paths cut
  from the end, which hid the part that differs, so keys are shortened from the left.
- 2026-09-21: PR F self-review, 12 findings, all fixed. A `Report` type now owns the
  `--json` shape (empty tables print `[]`, an empty `--by` means backend, the window is
  carried in the report instead of a field nothing read). A batch with a task still
  running and a record that cannot be read are counted under the tables instead of
  skewing or vanishing from them; that gave `runlog.ListSince`, which skips run
  directories older than the window by their id prefix (a day of slack) and reports
  unreadable records, as `batch.List` now does. The cache share counts cache writes as
  input, on purpose a deviation from the spec's formula: the smoke had shown 100% for
  claude while cache creation was billed. The attempts median ignores tasks that never
  ran; `Percent` never rounds to a false 100% or 0%; directory keys are canonical, so one
  workspace under two spellings is one row; `ParseSince` refuses windows that overflow;
  and `table` and `shortKey` got the first tests in `cmd/hx`.
- 2026-09-22: PR I done (#11). Built in a worktree (`E:\__Workdir\harness-i`, branch
  `i-guard`) because another session had the main checkout on PR E. Deviation: the refusal
  text does not say "rerun with --allow-dangerous". That sentence is read by the model,
  which cannot rerun anything and does have a shell, so it invited a retry or a recursive
  hx call; it now names the rule, says only the operator can lift it, and tells the model
  to explain rather than retry. Design note the spec left open: the guard needs per-run
  state, but `APIBackend.Tools` is built once in `NewAPIBackend` and shared by every run,
  so the switch rides on `tools.Workspace` (already the per-run policy object for path
  confinement) rather than in a tool closure. `pipe-to-shell` also matches `powershell`
  as written in the spec. The dirty check lists up to five paths so the message is
  actionable, and runs before any worktree is created. Two self-inflicted lessons for
  scripted edits, both caught only by merging main: an invalid `open(..., newline=)`
  truncated this file before raising, and `subprocess.run(text=True)` decoded `git show`
  with the console codepage and mangled every arrow. Read bytes, decode UTF-8, and assert
  the file did not shrink.
- 2026-09-22: PR I self-review, 10 findings: 7 fixed, 3 not. The guard as specced blocked
  `rm -rf /`, which rm itself refuses without --no-preserve-root, and allowed `rm -rf /*`,
  `rm -rf "$HOME"` and `rm -rf /c/`, which are the spellings that actually empty a machine;
  rm-root now reads the arguments instead of matching one pattern, and `git -C dir push`,
  reversed `dd` operands and multi-stage `curl | tee | bash` are caught too. The ordering
  bug was worse than any pattern: in `ask` mode hx showed the command, took the operator's
  yes, and then refused it anyway. Tools now carry a `Refuse` hook the loop asks before it
  prompts, which also gives MCP tools a place to opt in later. A CLI backend in yolo mode
  says the guard is not applied, since it never was for four of the five backends; the
  system prompt now states which way the switch is set, because the tool description is
  built once and shared; and `dirtyPaths` gives batch one answer to "is this tree dirty"
  instead of two. Not fixed, with reasons: MCP exec tools stay unguarded (their input
  shape is the server's, and guessing which field is a command would be a lie in the
  README - the `Refuse` hook is where that lands when it is worth doing); `hx run` and the
  MCP run tool get no dirty-tree check because they work in the operator's own checkout,
  where uncommitted work is visible to the agent - the hazard is specific to batch
  branching from HEAD; and the CLI flag-to-Task wiring is still untested, the same gap PR A
  and PR D deferred, now waiting on PR K.
- 2026-09-22: PR I, a flake the ubuntu runner found that the Windows one never did:
  `TestLimitsReachTheTaskAndAreValidated` panicked on `Attempts[0]` because a task had no
  attempt at all. The only path to that is a failed `git worktree add`, and two batch
  workers run one each in the same repository at the same time; git rewrites shared
  worktree metadata there and takes no lock of its own, so on a fast machine one loses.
  `worktreeMu` now serialises add, remove and prune - the agent, the gates and the commit
  stay parallel, which is where a batch spends its time. The tests went through
  `Attempts[0]` in six places and panicked instead of reporting; `firstRun` now fails with
  the task's own status and error, so the next occurrence says what happened.
- 2026-09-22: PR J done (#13). Decisions the spec left open: a named task that did not
  pass is skipped rather than merged, because naming a task is not evidence its gates
  passed and a silent merge of unproven work is the one outcome nobody wants; `merge`
  reports "already merged" instead of making an empty commit, by comparing HEAD before
  and after; `diff` puts the diff on stdout and everything else on stderr so it pipes
  into `git apply`, and lists untracked files separately because a diff cannot show a
  file git has never seen; and a conflicting `--squash` merge has no MERGE_HEAD to abort,
  so the cleanup falls back to `reset --hard HEAD`, which is only safe because the tree
  was verified clean first. Two changes the spec did not ask for: the batch record is now
  written to a temp file and renamed, since `show` reads it while the batch is writing it,
  and half a file is not a record; and `cmdBatch` moved to `cmd/hx/batch.go`, which keeps
  main.go at 390 lines and puts the four subcommands together. Verified end to end with a
  live claude batch: two tasks, two merge commits, a second merge finding nothing to do,
  then `hx batch rm`.
- 2026-09-22: PR J self-review, 12 findings, all fixed. Three mattered. Cleanup after a
  failed merge ran on the context that had just been cancelled, so Ctrl+C left the tree
  mid-merge and said it could not be restored; `undoMerge` takes its own context now, the
  way `commitChanges` already did. `Diff` returned `gitOut`, which merges stderr into the
  result: good for an error message, a corrupt patch for `git apply`, since git writes its
  CRLF warning there - diffs go through a new `gitStdout` that keeps the bytes as git
  wrote them, trailing newline included. And a `--squash` re-merge staged nothing, so the
  commit failed and the task was reported as a conflict; an ancestry check now skips a
  branch that is already in, and a squash that stages nothing is "already merged" too.
  The rest: `Conflict` is only set when git actually left unmerged paths (a hook refusing
  the commit is an `Error` instead), a running batch is timed from `Started` rather than
  printing 0s, `hx batch rm` keeps its old exit code for an unknown id, `batch diff` takes
  flags like its siblings, `short` became `tools.ShortSHA`, and a diff of a batch whose
  branches were removed says so instead of passing on "bad object".
- 2026-09-22: PR L done (#16), the first thread from *What is open*. Sized before starting:
  fifty test literals name option fields directly, and Go composite literals cannot set a
  promoted field, so embedding meant rewriting them all; a script did it, the compiler
  found the four comparisons it missed. The design trap: a type with its own MarshalJSON
  cannot be embedded, or its method is promoted and the outer struct marshals as just the
  embedded part - so the duration handling is on `agent.Duration`, the field's type, not
  on `Options`. Smoked on the built binary: the flag parser itself refuses `--mode weird`
  and `--timeout soon`, `hx report` still reads the records written with `timeout_ns`,
  a spec with `"timeout":"5m"` and a task-level `mcp` parses, and the MCP schema lists
  every option with a description.
- 2026-09-22: PR L self-review, 10 findings, 8 fixed. The one that mattered: `Duration`
  accepted a bare JSON number as nanoseconds, for records that in fact keep the old
  value under their own key, so `"timeout": 300` in a spec would have run every task for
  300ns; numbers are refused now and null means unset. A task's own `mcp` list is
  resolved and checked in `Run` with the spec's, so a typo in task seven no longer waits
  for six agents to finish, and an explicit empty list means no servers at all, which
  the README had claimed and `ConfigPaths` had quietly overridden with the default file.
  A test in `cmd/hx` now fails when the hand-written usage text omits a flag the struct
  has. Not fixed: a bad per-task duration is reported by value rather than by task,
  because encoding/json gives a custom unmarshaler no field path; and the methods of an
  embedded struct promote, so `spec.Validate()` exists and checks only the options -
  named in the doc comment, not worth un-embedding.
