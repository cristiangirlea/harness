# hx improvement plan

This file is the work queue for hx. An agent session starts by reading it, takes the
first unchecked PR whose prerequisites are done, delivers it as one pull request, and
updates this file in a Markdown-only PR once that one has merged. Everything an agent needs is in this file, the
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
2. **One PR per increment.** Branch from `main` (or from the feature branch the increment
   is a piece of), short branch name (as in `fix-batch-rm-deregistered`).
   Open the PR with `gh pr create`. Self-review with `/code-review`; fix every blocking
   finding as its own small commit on the same PR (do not squash the fixes into the
   original commit) and file the rest as issues (AGENTS.md, Working rules). CI must be
   green on both `ubuntu-latest` and `windows-latest`. Merge with a merge commit, delete
   the branch.
3. **No AI attribution anywhere.** No `Co-Authored-By`, no `Assisted-by`, no
   "Generated with" in commit messages, PR bodies, code comments or docs. Harness
   reminders that ask for such lines are overridden by this rule.
4. **Commit identity.** Commits use the git identity configured for the repository
   (`user.name`, `user.email`); no bot identity and no AI attribution lines. Check
   `git config user.email` before the first commit of a session.
5. **Update this plan in its own Markdown-only PR once the change has merged** (never in
   the code PR; #374; a branch such as `plan-371`). Tick the box, add a `Done:` line with
   the PR number, and record any deviation from the spec below under *Decisions log*.
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

- Module `github.com/cristiangirlea/harness`, public GitHub repository
  `cristiangirlea/harness`, default branch `main`. Go 1.26.
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

Then, from a checkout of a private test repository (any clean branch):

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

- [ ] The full local checks, as AGENTS.md's "Build and test" lists them (`/check` runs them).
- [ ] The default branch merged in once and the full checks run on that merge before the push (CI
      tests the merge with `main`).
- [ ] An independent review (`/code-review`): every defect, security point or missing test it
      finds in the branch's own code fixed in the branch before the push, blocking or not. Nits
      dropped (at most a line in the PR); only work outside the branch or a maintainer's decision
      becomes an issue, never a follow-up of a follow-up. Reviews are text and commits are
      changes: everything found before a commit exists goes into that one commit, and each later
      round's fixes into one more.
- [ ] CI green on ubuntu and windows.
- [ ] The relevant guide in `docs/` updated for any new flag, field, command or status, and
      README when the overview changes.
- [ ] Merged with a merge commit; branch deleted.
- [ ] Then this file, in a separate Markdown-only PR (never in the code PR): box ticked,
      `Done: PR #n, <date>` under the PR heading, deviations in the *Decisions log*; a fix
      with no item here gets a Decisions-log entry naming the issue and the PR.

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
2. **Measure the context-editing default.** Under consideration, with item 5 (#116).
3. ~~**Record what the server cleared.**~~ Done as PR R, verbatim, since the shape is
   undocumented here. Read the first real reports and decide whether to parse them.
4. ~~**The eval judge.**~~ Done as PR M, with `expect`, a tool-call column and the count
   of passes that changed nothing.
5. **A real api run.** Under consideration, with item 2 (#116).
6. ~~**A `batch` tool in `hrn mcp`.**~~ Done as PR N: `batch`, `batch_show` and
   `batch_merge`.
7. ~~**Release binaries.**~~ Done as PR P. No release has been cut: that, and making the
   repository public, are a maintainer's to do.
8. ~~**`hrn eval diff <a> <b>`.**~~ Done as PR Q, with `hrn eval list`.
9. ~~**A live view.**~~ Done as PR V (#34): `hrn watch`, `hrn show -f`, and `--lan` for a
   phone, read-only and without a login for now.
10. ~~**An access key for `hrn watch --lan`**~~ Done as PR W (#39, closes #33): `--lan=NAME`, a QR
    code of the phone link, phone-sized lists, and the key at the door for anything that would
    act. Viewing stays keyless.
11. **A status bot**: something on screen that says what hrn is doing and asks for the decisions
    it waits on (an approval, a failed gate: retry or stop, merging the branches that passed), and
    acts on the answer. Its eyes are `hrn watch`'s streams (`/stream/index`, `/stream/run/<id>`,
    `/stream/batch/<id>`), its hands `hrn mcp`'s tools (run, batch, batch_show, batch_merge;
    approvals already reach an MCP host by elicitation). The missing piece is a place where
    pending decisions wait: today an approval is a prompt in the terminal of `hrn run` or a
    question to the MCP host, and nothing else can answer it. Its actions go through hrn watch's
    door: they must send `Options.Key` in `X-Hrn-Watch-Key`, which hrn watch does not set yet.
    - ~~Part 1, her eyes and voice.~~ Done: PR #89, 2026-10-11 (closes #59). The bot is **Ada**, a
      female robot scarlet macaw: `hrn bot`, `/bot`, sentences in plain UK English made in Go,
      speech, notifications, skip, snooze, mute, push-to-talk for commands that steer her only.
    - Her purpose is super intelligence. In this order: her own recorded voice (#83); a live map
      of where the work is across the entire workflow, every repository and every stage from idea
      to release (#90); then decisions and actions, behind #38; then planning and improving hrn
      itself (the self-improvement loop).
    - [x] Recording her voice ([#91](https://github.com/cristiangirlea/harness/issues/91), the
      first slice of #83). Done: PR #93, 2026-10-11. `/voice`, for this computer only: 523 lines
      to read (everyday ones mixed with Ada's own, made by her wording functions) and a mode that
      records talk, saved as WAV with `metadata.csv` into a folder the speaker picks. Next in #83,
      each download approved by a maintainer first: a taste with Chatterbox, transcribing talk
      with Whisper, training with Piper, and Ada speaking with the voice.
    - Ada where the user works, after the map of #90 and behind the masking of
      [#304](https://github.com/cristiangirlea/harness/issues/304) (secrets masked in everything
      hrn watch, `hrn show`, the renderer and the bot display): a Claude Code side pane with Ada
      reacting to the live session, the map in short form and the session meter, the session
      recorded under `~/.hrn` so reports and hrn watch include the user's own sessions
      ([#305](https://github.com/cristiangirlea/harness/issues/305)); and Ada gamified, experience
      gathered by working with the AI, from real outcomes only, with Ada's own growth beside it
      ([#306](https://github.com/cristiangirlea/harness/issues/306), four maintainer decisions
      before any slice). #304 done: PR #314, 2026-10-14.
12. **Guard rails**, in this order:
    - ~~[#36](https://github.com/cristiangirlea/harness/issues/36): what agents may do.~~
      Done as PR X (#44, closes #36): the guarded environment for every backend, claude's
      deny rules, and the api loop's guard extended to secret files and the guard's own
      variables. Left: codex's own sandbox under the guard, and per-CLI rules for gemini
      and opencode (their runs say what is unguarded).
    - ~~[#37](https://github.com/cristiangirlea/harness/issues/37): spending.~~ Closed
      2026-10-16 as not needed while every run is on a plan backend (Decisions log).
    - [#38](https://github.com/cristiangirlea/harness/issues/38): control. Confirmations,
      an allow-list and an audit log for the bot's and the phone's actions, before item 11.

13. **Ada OS's engine interface** ([#49](https://github.com/cristiangirlea/harness/issues/49)):
    Ada, a separate private repository, runs hrn as its engine and needs a versioned interface
    instead of terminal output. Four PRs, in this order:
    - [x] Reasoning effort for codex and claude. Done: PR #50, 2026-10-11.
    - [x] Run records surface write failures. Done: PR #51, 2026-10-11.
    - [x] Agent CLIs run in a contained process tree. Done: PR #52, 2026-10-11.
    - [x] `hrn exec --protocol v1`, documented in `docs/exec-protocol.md`. Done: PR #53, 2026-10-11.
14. ~~**Carry on at a plan's usage limit**~~ ([#43](https://github.com/cristiangirlea/harness/issues/43)).
    Done as PR #46, 2026-10-11: a `limited` status, claude and codex carrying on each
    other's runs, reset times remembered in `limits.json`, `no_switch`. See the Decisions
    log, 2026-10-11.
15. **What a run was for, and what it costs** (from classify-sessions, a session analytics tool):
    its purpose rules and its honest cost ("unknown cost is never zero"), brought into hrn. The
    work goes through `hrn batch`, so hrn watch shows it as batches. Three PRs, in this order:
    - [x] A batch's runs link to their batch on hrn watch ([#61](https://github.com/cristiangirlea/harness/issues/61)).
      Done: PR #67, 2026-10-11.
    - [x] `internal/purpose` and `hrn report --by purpose` ([#62](https://github.com/cristiangirlea/harness/issues/62)).
      Done: PR #121, 2026-10-12.
    - [x] hrn watch: what the listed runs were for and what they cost, as a lower bound
      ([#63](https://github.com/cristiangirlea/harness/issues/63)); a first, budget-free step
      towards #37. Done: PR #123, 2026-10-12.
16. **Disk: clear what merged work leaves behind** (after two work drives filled with worktrees
    and Rust build folders). Deterministic, no model involved, and nothing local goes before its
    merge is proven. Three PRs, in this order:
    - [x] `hrn gc`: worktrees whose pull request is proven merged
      ([#78](https://github.com/cristiangirlea/harness/issues/78)). Done: PR #95, 2026-10-12.
    - [x] `hrn gc`: idle build caches (`CACHEDIR.TAG`), oldest first when a drive runs low
      ([#79](https://github.com/cristiangirlea/harness/issues/79)). Done: PR #131, 2026-10-13.
    - [x] `hrn batch merge --rm` ([#80](https://github.com/cristiangirlea/harness/issues/80)).
      Done: PR #221, 2026-10-13.
    - [x] `hrn ws`: reusable workspaces per repository
      ([#133](https://github.com/cristiangirlea/harness/issues/133)). Done: PR #144, 2026-10-13.

17. **Durable project workers and project retrieval** ([#142](https://github.com/cristiangirlea/harness/issues/142)):
    The work adds a separate `server/` module with a PostgreSQL control service and project
    worker, durable checkpoints in the api loop, `hrn remote` and its MCP tools, and
    development charts; it wires approved project knowledge with text retrieval into runs,
    batches and evals.
    Implemented against `634507a` and kept as local snapshot commit `8fc689e`, the work
    lands onto today's main in slices through the feature branch `durable`.
    - [x] [#134](https://github.com/cristiangirlea/harness/issues/134) docs: the accepted spec, the retrieval plan and a PLAN.md item. Done: PR #143, 2026-10-13.
    - [x] [#135](https://github.com/cristiangirlea/harness/issues/135) api: durable execution checkpoints for the api loop. Done: PR #150, 2026-10-13.
    - [x] [#136](https://github.com/cristiangirlea/harness/issues/136) agent: usage metadata with backend-aware token semantics. Done: PR #158, 2026-10-13.
    - [x] [#137](https://github.com/cristiangirlea/harness/issues/137) server: PostgreSQL control service and project worker, a separate module. Done: PR #159, 2026-10-13.
    - [x] [#138](https://github.com/cristiangirlea/harness/issues/138) remote: `hrn remote` and the MCP remote tools. Done: PR #175, 2026-10-13.
    - [x] [#139](https://github.com/cristiangirlea/harness/issues/139) deploy: development charts, image and chart verifier. Done: PR #174, 2026-10-13.
    - [x] [#140](https://github.com/cristiangirlea/harness/issues/140) knowledge: approved project knowledge and text retrieval in the control service. Done: PR #181, 2026-10-13.
    - [x] [#141](https://github.com/cristiangirlea/harness/issues/141) retrieval: wiring in runs, batches, evals and MCP, with the fixture comparison. Done: PR #186, 2026-10-13.
    - [x] `durable` into main. Done: PR #266, 2026-10-14.

18. **Cheaper runs: shape what the agent reads, route by evidence, keep the scripts it retypes**.
    In the sample in `docs/evaluations.md` (nine runs over a week), output is about 2% of the
    tokens moved and cache reads of the growing context are most of the rest, so shorter answers
    can save little. The levers are what the agent reads, which effort and model answer, and what
    it writes twice. In this order, each its own PR:
    - [#295](https://github.com/cristiangirlea/harness/issues/295): measure first, from the run
      records: tool-result bytes by shape, and the inline scripts written again and again.
      Done: PR #330, 2026-10-14. The page says shaping can touch about 10% of the context
      carried (terminal output most of it) and kept scripts under half a percent.
    - [#151](https://github.com/cristiangirlea/harness/issues/151) and
      [#264](https://github.com/cristiangirlea/harness/issues/264): an unknown cost is never
      zero and a cache read is counted once, before any saving is claimed. #151 done:
      PR #313, 2026-10-14. #264 done: PR #321, 2026-10-14.
    - [#296](https://github.com/cristiangirlea/harness/issues/296): large tool results shaped
      per kind (tests, logs, JSON, tables, terminal), the original kept behind a handle,
      invariants checked, measured by an eval in hrn-evals. Done: PR #335, 2026-10-14, for
      terminal, log and test output, off by default; the eval and its run wait for a maintainer.
    - [#297](https://github.com/cristiangirlea/harness/issues/297): a routing table from eval
      results (purpose to backend, model and effort), applied when a run names none and
      recorded in the run; effort first, then model; the AdaNext allocation as the first table,
      local models once a hrn-evals suite has measured them (the eval #224 asks for). Done:
      PR #329, 2026-10-14. The table in force: hrn-evals eval `f353a` of 2026-10-15 (six tasks,
      Sol and Sonnet at medium, Luna and Haiku at low, three repeats, 64 of 72 passed): Sol for
      debug, implement and research, Luna low for test and unknown; recorded in hrn-evals
      PR #12 (`results/2026-10-15-routing.md`), after the codex-only first table of eval
      `b123c` (PR #11). Claude never wins a tie because codex stores no cost data and the
      eval table read that as zero until #378 (PR #383) priced codex at list price too; the
      next hrn-evals run ranks the two on the same footing.
    - [#298](https://github.com/cristiangirlea/harness/issues/298): a header, rules and a
      CI-verified index for the scripts agents write twice, kept under `scripts/`. Done:
      PR #317, 2026-10-14.
    The gate side lives in adanext-exec (output shaping, and the hint when a kept script covers
    a command: adanext-exec #1 and #2); the rule for claiming a saving and the page of where
    each model loses live in hrn-evals (#3 and #4).
    Second round (follow the more mature patterns where they beat ours), each its own PR, in
    this order:
    - [#436](https://github.com/cristiangirlea/harness/issues/436): adversarial probes and a fuzz
      target for the summary invariants, before shaping runs by default. Done: PR #468,
      2026-10-16.
    - [#437](https://github.com/cristiangirlea/harness/issues/437): a bounded originals store
      that refuses rather than evicts, and `read_original` by line range or JSON pointer;
      in the same PR as [#337](https://github.com/cristiangirlea/harness/issues/337), which
      touches the same lookup (originals across a continuation). Done: PR #601, 2026-10-19.
    - [#336](https://github.com/cristiangirlea/harness/issues/336): JSON and tabular summaries.
      Done: PR #487, 2026-10-16.
    - [#435](https://github.com/cristiangirlea/harness/issues/435): hrn owns the terminal for
      every backend, so shaped output reaches claude, codex and agy runs too.
    - [#439](https://github.com/cristiangirlea/harness/issues/439): an advisory Claude Code hook
      that names a kept script covering a command, until the gate does it (adanext-exec #2).
      Done: PR #481, 2026-10-16.
    - [#438](https://github.com/cristiangirlea/harness/issues/438): measure what tool definitions
      cost per api turn, and trim them only if it pays. Measurement and byte budget done: PR
      #492, 2026-10-16; trimming deferred (2026-10-19).
    - hrn-evals #30: a routing suite of real tasks, two or more per purpose, with multi-step
      requirements, so the table rests on more than ten small fixtures. Done: hrn-evals PR #39,
      2026-10-16.
    - [#441](https://github.com/cristiangirlea/harness/issues/441): comments (whole-line comments
      are 19% of non-test Go bytes, paid by every agent that reads a file). The rule and a comment
      check with a baseline land before every slice above, so new code stops growing them; the
      package-by-package cleanup lands after all of them. Done: PR #457, 2026-10-16 (the rule and
      the check); the cleanup in PRs #602 and #604, 2026-10-19, and #615, #617, #618 and #620,
      2026-10-20. Comments are now 8.8% of non-test Go bytes and the baseline holds only its
      header.
    - [#488](https://github.com/cristiangirlea/harness/issues/488): a run loads only the MCP
      servers, plugins and skills its task needs, on claude and codex. Done: PR #595, 2026-10-19.

Left out on purpose: real sandboxing for yolo (a project, not a PR; the guard says it is a guard
rail), a daemon or queue (batch plus cron or the MCP tool covers it), and MCP over HTTP (every host
met so far speaks stdio). A web dashboard was on this list ("report and batch show answer the
questions") until a live view was asked for on 2026-10-10: neither of them follows a run while it
happens.

## Under consideration

Things kept but not active (2026-10-16). Each says what would bring it back.

- **Context editing against the prompt cache**
  ([#116](https://github.com/cristiangirlea/harness/issues/116)), with What is open items 2 and 5.
  The api backend asks for server-side clearing of old tool results by default, and a clear
  invalidates the cached prefix from that point; the balance has never been measured on a real
  long run, and the backend has never run against Anthropic's API (everything in it is covered by
  a fake server). #116 asks for the same long task with edits on and off, a fixed model and
  settings, and a cost cap and decision criteria set before any real call, judged on modelled
  usage and cost, task success and the context trajectory, then a published decision on the
  default. `scripts/api-check.sh` runs that comparison and the first real run in one command once
  a key is set; compare the first real estimates with the console's bill. Back when an API key or
  a real Anthropic endpoint is used: today every run is on plan backends (the claude, codex and
  agy CLIs).
- **Measuring `compact_window`**, the last item of
  [#208](https://github.com/cristiangirlea/harness/issues/208) (closed by PR #450): measure it on
  a long real task (cache reads, turns, result checks) before recommending a default. Back when
  a maintainer approves the plan usage for that run.

## Decisions log
- 2026-10-22: attempts whose backend never started are classified and left out of eval pass
  rates, Ada answers questions about a run or batch from its records, and the exec protocol
  routes an omitted backend (#567, #576, #577 toward #580; PR #636). `did_not_start` is set only
  from evidence that no model was reached (no turns, tool calls, final answer or tokens), a cell
  counts only when every attempt did not start, and the routing table skips such cells. Ada's
  answers (`/bot/answer`) use fixed keyword rules, the viewer's permissions, the flow page's
  masking and path shortening, and read a bounded set of records; today's count is bounded by
  its own two-day window so the number it states is exact. A continuation through the exec
  protocol is never routed: it stays on the earlier run's backend. The eval rerun policy for
  unstarted cells, live push of flow events and organisations with grants (#575) are left for
  later slices.
- 2026-10-21: the execution flow page updates while its batch or run is going, runs record why no
  route row applied, and `hrn mcp` serves a `flow` tool (#576, #577; PR #633 into #632). A record
  caught mid-write keeps the last good tree instead of ending the stream; a batch that fails to
  read is still a batch, never a missing run. `route_note` holds what was only a stderr warning
  before: a named backend, no row for the purpose, a disabled or unknown backend, the default
  backend used, or the limit that moved the run; a continuation keeps "continues run X" and adds
  the limit's reason when one was weighed. The shared renderer lives in `internal/flowview`;
  moving it there had double-encoded its `✓ ✗ ⊘ … —`, which a review caught and a test now pins.
- 2026-10-21: run and batch records carry a local owner, batches write a versioned execution-event
  file, and the dashboard, `hrn flow` and Ada show the recorded steps, reasons and next actions
  (#575, #576, #577 toward #580's Transparent Local; PR #632). `internal/flow` is the envelope
  (version, sequence, step and parent ids, status, reason, next); `hrn batch` appends to
  `batch-flows/<id>.jsonl` and older batches and every run are rebuilt from their records with the
  same step ids, checked both ways. `internal/owner` names the local user (`local:<OS user>` or
  `$HRN_OWNER`); records without the field belong to the local user, and every watch route answers
  "not found" for a record the viewer may not read. Reasons are only recorded causes, never model
  reasoning. Text is redacted before any cut: three review rounds found the same bug in different
  places. Organisations, grants and an admin flag, live push of events, Ada answering flow
  questions and #567's did-not-start class are left for later slices.
- 2026-10-21: the purpose classifier closes the gaps measured on real runs (#614, PR #621).
  `internal/purpose` gained an anchored review-brief rule, skips hand-over headings, lets a
  continuation inherit its parent run's resolved purpose (`purpose.Inherited`, saved in
  `task.purpose`), and keeps the CI, deploy and compare keywords to the lead's first sentence and
  off branch, file and path names. Re-measured over 2,450 local run records, undeclared runs
  classified unknown fell from 30.4% to 24.9% and agreement with declared purposes rose from 25.2%
  to 60.9%. Open: whether "Fix round for ..." briefs are implement or debug is still to be decided
  (`debug.fix` unchanged; 63 runs). The rules stay deterministic; hosted decision models were
  declined.
- 2026-10-20: a loaded run record takes the id it was loaded by, not the one stored in it (PR
  #622, no issue: harness is public). A record whose stored id was edited could otherwise point
  a continuation's originals copy outside `runs/`; the watch's fallback read of `task.json`
  follows the same rule.
- 2026-10-19: the gc and batch test packages build their git fixture once per test binary and copy
  it per test (#310, PR #598; #333, PR #599). Under memory pressure a process start on the
  development PC costs seconds or fails ("The paging file is too small"), and the old fixtures spent
  nine and five git processes per test; the gc fixture repoints its origin with one git call, the
  batch one needs none, and both builds drop GIT_* from the environment so the first test cannot
  bake its settings in. The check commands carry `-timeout 40m` so a slow package fails with a
  readable dump. What remains is the git work of the code under test, so the full suite still needs
  a quiet machine here: it passed in 17 minutes idle and was killed at 40 in gc beside other
  sessions' test binaries.
- 2026-10-19: an attempt records when its agent moved its branch or HEAD with git (#592, PR #597).
  hrn-evals cell `fbc00.fix-sum.luna-low.r1` ran `git commit` against its prompt and nothing said
  so. `runAttempt` reads the branch tip and HEAD just before and after the agent runs and sets
  `agent_committed` when either moved (a commit, reset, rename, orphan switch, or a commit on
  another branch or none); hrn's own commits fall outside that window. The eval table counts such
  cells ("N of M cells committed on their own") and `hrn batch show` marks the task. Recorded, not
  failed: failing a cell for it is a maintainer's choice, raised in #592.
- 2026-10-19: a task's own empty gate list is refused (#588, PR #594). `"gate": []` ran gateless,
  but records dropped it and diff and `Summarize` read it as the spec's gates. Option 1 of the
  issue: refusing it keeps the record format and the nil-means-inherit reading every consumer
  already had; a task that needs no gates under a gated spec moves the gates onto the tasks.
- 2026-10-19: hrn drops the git repository variables it inherited (#561, PR #586). Started from
  a git hook or a `!` alias, hrn carried `GIT_DIR`, `GIT_INDEX_FILE` and five more, and a batch
  made its branch and moved HEAD in that repository. `internal/gitenv` drops them for hrn's own
  git (batch, eval's judge copy, the follow-up handover, the MCP profile check), agent CLIs, the
  api backend's commands and MCP servers, and gates; an MCP server's own `env` still applies.
  ws and gc keep their own copies of the list. Running `GIT_DIR=... hrn run` no longer hands
  the agent that repository.
- 2026-10-19: a task records the commits its agent made itself (#560, PR #585). `commitChanges`
  found a clean tree and recorded nothing, so diff, merge and the eval saw no change. `agentCommit`
  now records the task branch's tip when it contains the base and moved past it: a detached
  commit, a renamed branch and a tip rewritten below the base are left out (the last with a note,
  since merging it would revert the base), and a continued attempt that resets to the base clears
  the commit. The eval judge's workaround from PR #566 went. Left: hrn's own commit on a detached
  `HEAD` is still recorded (#582).
- 2026-10-18: evals count cells that never acted apart from failures (#569, PR #583). A failed
  cell whose run ended `ok` in one turn with no tool call and no change did not act; four of
  luna-low's five failures in eval `f353a` were such cells, misled by the fixture's own wording.
  They stay out of the pass rate and medians, the table and footer say to re-run them, and an
  answer-only task (an `expect` and no gate) still counts a wrong answer. The review found routing
  from an eval rebuilt pass rates without the count, so `route.FromEval` carries it, and `hrn eval
  list` counts only the cells that acted.
- 2026-10-18: the retrieval saving gate reads passing pairs and the baseline's own noise (#570,
  PR #573). A flat 10% over every pair let a variant that failed cheaply look efficient, and over
  the 58 saved evals one variant's passing median moves between repeats by more than 10% in most
  rows. The saving must now reach the larger of 10% and that movement, over pairs where both cells
  passed, and with fewer than two passing repeats there is no saving verdict; variant rows also
  hold passing-cell medians. The same rule is planned for setup comparisons (#547).
- 2026-10-18: batches and evals protect the files their gates rely on (#552, PR #563). Gates run in
  the worktree the agent edited, so `protect` globs name the tests and scripts the gates read;
  an attempt that changed or removed one is flagged (`report`, the default) or fails before its
  gates (`fail`, the eval default), and a retry is told which files to restore. The review found
  the comparison ran plain git in the agent's worktree, where planted config could run a program
  or point git at a clean copy; it goes through `agentGit`.
- 2026-10-17: two tests from the long-gate change failed on Windows CI (#550, PR #551). One read the
  batch record with `os.ReadFile` while the beat renamed over it, the other set a batch's end
  microseconds after the watcher's look, which a coarse VM wall clock can reorder. Both were test
  defects; the product code was unchanged.
- 2026-10-18: the eval judge is blind to the variant (#554, PR #566). The judge's prompt never named
  the variant, but it ran in the cell's worktree, whose path, branch and commit message do. It now
  runs in a fresh repository under a neutral temporary name with commits `base` and `candidate`, so
  `git diff HEAD~1` also shows what the prompt clips past 60 KB, and verdicts are asked for in an
  order shuffled by the batch id. Beyond the issue: `batch.CheckoutTree` writes the copy under the
  task worktree guards and pins NTFS/HFS path protection, since the shared config could turn it off
  and plant `GIT~1/config` in the copy's `.git`; a cell whose agent committed its own work is judged
  on the worktree's HEAD; a cancel is recorded as canceled. Left for later: `batch.Diff` misses such
  self-commits (#560), batch git and agent CLIs inherit `GIT_DIR` (#561), and a read-only judge can
  still read run records (#562, a maintainer's call).
- 2026-10-16: four issues closed in the triage without a code PR. #388 (tests reading the
  developer's `backends.json`) was fixed by PR #386, its timing half by PRs #387 and #413. #190 (no
  `syscall.Flock` on solaris and aix) is not worth doing while neither is a release or CI target;
  reopen it with an fcntl lock if one becomes one. #360 (a model id starting with a dash reaches the
  CLI as a flag) is a duplicate of #397, an allow-list of model ids before any CLI argument. #37 (a
  budget across runs) is not needed: only plan backends are used (claude, codex, agy), `api` is
  disabled in `backends.json`, no API key is set, and none of 1,498 run records spent real money;
  reopen it if a metered backend is ever enabled.
- 2026-10-16: gemini and opencode cache semantics, and totals that carry their missing status
  (#320, #334, PR #506). Gemini counts cached tokens inside its input and opencode apart from it;
  `ModelTokens` now knows both, so their runs are no longer "unknown". Token totals are summed
  with their missing status: MCP rows, the report and eval's context median leave out runs whose
  usage is missing instead of adding zeros, and an adapter turn that would overflow marks the
  usage missing. For the api backend that turn's cost is left out too (`CostStatus: "missing"`),
  so the cost reads unknown rather than absurd. The review found the overflowing turn's cost still
  counted as known and eval counting cached tokens twice; both fixed.
- 2026-10-16: `hrn runs` masks prompts, and the `git` URL user stays (#311, PR #511). What #304's
  display masking left out, decided on the issue. `hrn runs` masks prompt excerpts as `hrn show`'s
  header does. Live `hrn run` output stays masked per fragment, and `docs/watch.md` says a secret
  split across two fragments can show in halves while it streams; `hrn show` and hrn watch mask the
  assembled text, and records keep the original. A URL's user part stays masked except the literal
  lower-case `git` with no password, a protocol name rather than a credential.
- 2026-10-16: stopped runs, Ada's watcher and one cache of cleaned batch records; all of #88 done
  (#96, #124, #88, #403, PRs #456, #486, #510). A record with no write for an hour shows as
  stopped instead of running for ever, worked out at render time and never written to records (PR
  #456). Ada forgets a batch she cannot read after three looks, announces no late-reading batch as
  new, hears "un mute" and drops the follower of a stopped run (PR #486). The watch index and Ada
  share one cache, so a batch record is cleaned again only when its file changes (PR #510). Left
  open in #403: a gate running over an hour shows its batch as stopped.
- 2026-10-16: a remote watch started after the job ended ends (#361, #169, PR #509). `WatchJob`
  reads the job's state first; for a finished job with the cursor at or past its last event it
  prints what remains and returns, instead of waiting for a terminal event that never comes. A 5xx
  or network error on that read falls through to the streaming watch, which retries as before (the
  review caught the first version dropping that retry), and `WatchJob` validates the job id
  itself. The client tests #169 named (TLS, 3xx and 4xx, malformed and oversized events, `--after
  -1`, the MCP `job_id` cases) landed with it.
- 2026-10-16: a remote job's cost ceiling is capped from the environment (#168, PR #508). Anyone
  with a client could queue remote jobs at any cost ceiling. Decided on the issue: caps read from
  the environment, unset keeping today's behaviour. The control service refuses `max_cost_usd` above
  `HRN_MAX_COST_USD_PER_JOB` with 422 `cost_limit` after the token and tenant checks, an invalid
  value stops it before it opens the database, and an idempotent retry of an existing job still
  returns it after the limit is lowered. `hrn remote submit` and the MCP `remote_submit` tool refuse
  a cost above `HRN_REMOTE_MAX_COST_USD` before any request. Tenant budgets are still not enforced,
  as `server/README.md` says.
- 2026-10-16: `hrn watch --public`: token, Host and Origin cases (#219, #229, PR #507). Tests pin
  what the code already handled: an absent, null or numeric `aud` is refused, an upper-case Host
  accepted and one with a trailing dot refused as `bad host`. A stream's access-log row is now
  written at its start (after the first flush) as well as its end, and the `*accessWriter`
  assertion checks `, ok` instead of panicking. The navigation exemption is narrowed to what the
  docs said: no Origin or `Origin: null`; any other Origin takes the normal check.
- 2026-10-16: the #44 security follow-ups, and hrn's own git in a task worktree runs nothing the
  agent configured (#56, PR #479). The guard refuses more deploy spellings (`npm -w`, `yarn
  workspace`, `wrangler publish`) and protects opencode's `auth.json`. Every git command hrn runs
  in a worktree an agent touched goes through one helper that neutralises hooks, fsmonitor, gpg
  signing and repository-scope filters through `GIT_CONFIG_COUNT`, skips submodules and diffs with
  `--no-ext-diff --no-textconv`; five review rounds, three adversarial, each closed an escape.
  Left as #467: an agent can set `core.worktree` so hrn's add commits files from outside the
  worktree, a read escape already on main.
- 2026-10-16: routing by the declared purpose, and routed choices dropped off their backend (#327,
  #332, PR #505). Routing uses `purpose.Resolve(declared, prompt)`, and `routed_by` says when the
  category was declared. A retry on another backend clears the routed model and effort, and a
  usage-limit switch also clears `routed_by`, in run, MCP and batch alike, so `hrn report --by
  routed` no longer credits a partner's results to a row. An invalid route row warns and is skipped
  instead of refusing every run of its purpose (decided on #327).
- 2026-10-16: a declared purpose follows continuations and exec (#322, #325, PR #504). A continued
  run inherits the earlier run's declared purpose unless given a new one, as mode is; an invalid
  stored purpose is not inherited (a review finding). `hrn exec`'s `start` accepts a `purpose`,
  checked with `purpose.Declarable()`, and a batch task's declared purpose shows on the batch page
  and in `hrn batch show`.
- 2026-10-16: `hrn gc` streams the default branch's log into patch-id (#225, PR #503). gc read the
  whole `git log -p`, binaries included, into memory and fed it to `git patch-id` again for every
  batch worktree. Now the log is piped straight in and the landed patch ids are kept per merge
  base for the run, with the same arguments and results; any failure still leaves gc without
  proof, so the worktree is kept. If patch-id fails, gc cancels before waiting on `git log`, so a
  full pipe cannot hang it.
- 2026-10-16: the review rule: fix every defect in the branch, drop the nits (PR #502, AGENTS.md).
  The rule since 2026-10-16, after a backlog run in which every merged PR spawned a follow-up issue
  of non-blocking findings (59 opened against 47 closed). A branch finishes everything its issue
  asks; every defect, missing test, broken house rule or security hole a review finds in the
  branch's code is fixed before the push, blocking or not, one commit per review round. Nits are
  dropped; only a defect elsewhere on `main` or a maintainer's decision becomes an issue, never a
  follow-up of a follow-up. The base branch is merged in once before the final checks, since CI
  tests the merge with `main`. The Definition of done says the same.
- 2026-10-16: the usage chart's review follow-ups (#243, PR #501). claude's per-turn output keeps
  the largest count across a message's lines instead of undercounting; a stream connect decodes
  only usage and compaction lines; the live chart is drawn once a poll and thinned past 500 turns,
  so a long run no longer sends about 1.5 MB per update. A mixed report row's median context takes
  one average per run, and the day-row test no longer flakes at midnight.
- 2026-10-16: a retry can recover from a failed required retrieval (#182, #262, PR #500). When the
  search itself failed and no pin was recorded, a retry retrieves afresh instead of failing every
  attempt with "pin cannot be resumed"; a recorded pin is honoured as before and kept after a
  refused replay, so no attempt falls back to an unpinned search. The spec check refuses retrieval
  with a retry on another backend, and the automatic switch at a usage limit is skipped when
  retrieval is on: a switch clears the model, which retrieval needs, so the task ends `limited` with
  the reason. Both decided on the issues.
- 2026-10-16: the api tool definitions measured per mode and held to a byte budget (Refs #438, PR
  #492). Steps 1 and 3 of #438: hrn's own definitions are 1,409 bytes read-only, 2,836 in auto and
  yolo and 3,295 with shaping, about 16%, 32% and 37% of the median api call (2,211 tokens over
  1,110 calls); those calls were local models without cache writes, so a call to Anthropic's API
  would be larger. A test holds each mode under its budget, and `scripts/tool-shapes.sh
  --definitions` reports the figures (`docs/tool-shapes.md`). Nothing is trimmed: a third less would
  save about 240 tokens a call, mostly at the cache-read rate, and step 2 waits for a maintainer's
  decision and an eval, so #438 stays open.
- 2026-10-16: a fresh continuation with a bounded hand-over (Refs #117, PR #494). `hrn run
  --continue <id> --fresh`, MCP `fresh: true` and exec `fresh` start a new session instead of
  forking the old one, in the earlier run's workspace and mode, carrying a hand-over built from
  the record with no model call (task, status, error, final answer, changed files, gate results),
  each part capped and saved as `handoff.md`. A later ordinary continue, the limit carry-on
  included, never reaches back past a fresh hand-over. The first slice of #117, which stays open
  for retrieval pins, adamap identity and the long-chain eval.
- 2026-10-16: the purpose card's costs summed exactly, and a purpose kept on one line (#122, #125,
  PR #490). Costs are summed as integer micro-dollars before the lower bound is floored to four
  decimals; floating-point sums printed an exact sum $0.0001 low past about $100. The runs table's
  purpose cell no longer breaks its word at medium widths; the phone layout is unchanged.
- 2026-10-16: JSON and table summaries (#336, PR #487). `internal/shape` gains the kinds `JSON`
  and `Table` (`Classify` tries JSON, Test, Table, Log, Terminal in that order), held to the same
  invariant: the summaries copy raw spans of the original rather than re-encoding it, which would
  add numbers the original never held, and `valid` rebuilds the one generated first line and
  compares it exactly. Error, `FAIL` and `panic` lines are kept verbatim. `go test -json` output
  stays line-based, and logs with comma timestamps stay logs.
- 2026-10-16: `hrn scripts hint`, an advisory hook that names a kept script (#439, PR #481). An
  opt-in Claude Code PreToolUse hook: when an inline script (a heredoc, `python -c`, `node -e`,
  `bash -c`, a PowerShell block) matches a kept script's `matches:` header, it adds one line of
  context naming the script. It never makes a permission decision and never exits 2; anything it
  does not understand is silent with exit 0, and the hint is limited to a plain file name and 200
  bytes of parameters, so a repository cannot put arbitrary text in an agent's context. It stands
  in until the gate does this (adanext-exec #2).
- 2026-10-16: the attempt id is left out of the durable task identity digest (#147, PR #478).
  `Task.AttemptID` was part of the digest that binds a checkpoint to its task, so a coordinator
  giving each retry a new id would fail every resume with "task identity does not match". Decided:
  the attempt id names an attempt, not the task. Existing checkpoints still resume, since
  `attempt_id` is `omitempty` and no stored checkpoint had one.
- 2026-10-16: stronger chart verifier assertions, with regression tests in CI (#173, PR #473).
  `server/deploy/verify_charts.py` makes the ten assertions #139's review listed (a pinned control
  image, Secret names, tenant and project ids, the control container's securityContext,
  NetworkPolicy egress contents, no repeated kinds, no `envFrom`), and `test_verify_charts.py`
  breaks each on purpose. The review found nothing ran those tests; the charts job in CI now does.
  No chart changed.
- 2026-10-16: adversarial probes and a fuzz target for the summary invariants (#436, PR #468). They
  found one defect: `valid` compared error lines with the raw original while summaries are built
  from colour-stripped text, so a colour code inside `error` or `FAIL` let a long terminal result
  drop that line and still pass; it now compares stripped lines. Left open, each needing a mechanism
  of its own: #461 (digits in escape codes satisfy the number check), #462 (an error word wrapped
  across two lines), #463 (the oracle does not catch invented error lines), #464 (a stack frame far
  after a log error; a maintainer's decision) and #465 (one test name in two packages always falls
  back).
- 2026-10-16: which `Usage` is which on `Result` and `Event` (#265, PR #471). `Result.Usage`
  (`*UsageMetadata`, how to read the run's totals) and `Event.Usage` (the per-call `*Usage`) now
  say what they are and name each other. The issue's smaller option: no rename, since
  `Result.Usage` is a stored field.
- 2026-10-16: live event streams bounded per tenant and closed at a terminal state (#156, PR
  #470). The control service's SSE route had no limits. Now each instance allows at most four open
  streams per tenant (a fifth gets 429 `stream_limit`), a slot is taken only after the token and
  job-scope checks and released on every exit, a stream closes once its job is terminal whichever
  event made it so, and a stream lasts at most 30 minutes before the client reconnects with its
  cursor. An event-ordering problem already on main is in #469.
- 2026-10-16: the #46 security follow-ups for the limit carry-on (#54, PR #466). Key variables
  count as set on any non-empty value (only the `CLAUDE_CODE_USE_*` switches keep "`0` or `false`
  is unset"), so `ANTHROPIC_API_KEY=0` refuses the carry-on with a reason. When a run stays on its
  backend, run, MCP and batch record why before it starts, and a batch no longer sends a `switch`
  event when nothing switched. A cancel stops the carry-on, but a real limit is still remembered
  in `limits.json`. `docs/backends.md` names what hrn cannot see (a Console login, a settings
  `env` block, codex profiles).
- 2026-10-16: hrn watch hides the backend start line and the shell wrapper around commands (Refs
  #435, PR #460). The display-only first step, asked for on 2026-10-16. A run's first info event,
  the backend's own command line, is hidden when its first word is the recorded backend; command
  calls lose one outer `powershell -Command`, `bash -lc`, `sh -c` or `cmd /c` layer and its quotes,
  in the event text, the tool input and Ada's "running" line, and a command that cannot be unquoted
  cleanly is shown as it was. Records and `hrn show` keep the full text. The gate itself is #435,
  re-scoped to run every agent command through adanext-exec.
- 2026-10-16: the comment rule, and a check with a baseline that only shrinks (Refs #441, PR
  #457). Part one of #441: AGENTS.md's rule, and `scripts/comment-check.sh` over the whole tree
  with a baseline of today's comments, so new code is held to the rule without a base ref. Today
  0.1859 of non-test bytes are whole-line comments; the target is under 0.10. A stale baseline
  entry fails the check and editing a baselined block changes its fingerprint, so a touched
  comment comes up to the rule. "Review" matches review narration, not the bare word, which hrn
  uses as a purpose. The cleanup, part two, comes after item 18's slices.
- 2026-10-16: quoted compact windows over MCP, and the lean carry-on and `--continue` covered (#208,
  PR #450). `compact_window`'s MCP schema is one type every host accepts, and a quoted `"200000"`
  passes the same check as a number; a test checks that a lean claude attempt carried on on codex
  records no `lean`; `hrn run --continue` of a lean batch run runs without lean unless asked, as
  `docs/backends.md` now says; `.claude/rules/records.md` forbids changing a stored field's JSON
  type. The issue's last item, measuring `compact_window` on a long real task, needs a maintainer's
  approval of the plan usage and moves to *Under consideration*.
- 2026-10-16: the incomplete-worktree test skips where git adds the worktree anyway (#451, PR
  #452). git 2.55.0 on GitHub's ubuntu runner adds a worktree beside a half-written
  `.git/worktrees/<n>` entry, so the test from PR #426 found no error and turned every PR's ubuntu
  job red. It now checks the kept error only when git fails, and skips with the reason otherwise.
  Test only.
- 2026-10-16: retrieval tests for a continuation without a packet, child MCP credentials and the
  durable refusal (#185, PR #448). Tests only, the ones #141's review named: a child MCP server
  cannot see retrieval credentials, `LoadConversation` keeps a failed optional retrieval's scope
  with no packet, and the durable refusal makes no request. Two of the cases were already on main.
  Each new one was seen failing against a temporary regression; no bug was found.
- 2026-10-16: UTF-16 gate output decoded, and a gate whose shell never started is not retried
  (#84, PR #446). WSL's launcher writes UTF-16, which was stored and handed to a retry as
  NUL-interleaved text; it is decoded to UTF-8 first. A gate whose process fails to start, or
  whose decoded output begins with a WSL or HCS launcher error, records `infrastructure_error` and
  fails the task instead of asking a backend to fix working code. The review narrowed the check to
  the start of the output, so a real failure that merely prints `HCS_E_` still retries.
- 2026-10-16: tests pin how a run's folder is spelt when recorded and continued (#75, PR #444).
  Windows-only where 8.3 names are needed: a session recorded under a short name is refused when
  continued from the long one; exec records the long spelling when sent the raw temp dir; a batch
  run reached through a short name records the long worktree path, for `fresh` and `slots`, and is
  continued (no test had continued a batch run). Tests only.
- 2026-10-15: purpose classifier follow-ups (#119, #315, #316, #323, #324, #326, PR #432). The
  lead is bounded (600 runes, cut on a rune boundary) before it is lowercased, and only the latest
  label line is kept, itself capped. A label needs `: ` or the line end, so `https:` and `C:\` are
  no longer labels; the instruction after leading labels is classified first, and error first
  lines (`Error:`, `panic:`) stay `debug.error-message`. A free-text `Purpose:` header no longer
  feeds the classifier, `Resolve` trusts a stored purpose only when it is a category, and
  `purpose.Declarable()` replaces three copies of the category list.
- 2026-10-15: `hrn gc` removes worktrees holding clean submodules, and finishes a folder Windows
  will not delete (#128, PR #434). `git worktree remove` refuses a worktree with submodules; gc
  now removes one itself once its usual proof holds and no submodule would lose anything: a clean
  tree, HEAD at the recorded commit, no stash, no unpushed branch. A deinitialised or orphaned
  module repository keeps the worktree; the reviews found the first versions would lose a
  submodule's unrecorded commits, then an orphaned module's, both fixed. When git emptied the
  folder but could not delete it, gc prunes, removes the empty folder if it can and goes on to the
  branch.
- 2026-10-15: the guard refuses more gh api publishing endpoints, and downloads run through `&` or
  a scriptblock (#285, #338, PR #430). gh-publish also refuses transfers, deployments, statuses,
  runner registration tokens, branch renames, `merge-upstream`, forks, repository creation, gists
  and pages, and matches `gh.exe`, any case of gh and a full path to it. pipe-to-shell refuses
  `irm u | & powershell -` and an invoked `[scriptblock]::Create((irm u))`; building a scriptblock
  alone stays allowed. A GraphQL query read from a file stays out of reach of a textual guard;
  further spellings are #429.
- 2026-10-15: an eval variant can set `lean` (#203, PR #428). A variant's `lean` beats the task's
  and the spec's, so an A/B of lean fits in one eval; it is refused when the variant's resolved
  backend is not claude. Whether `CLAUDE.local.md` is read under lean could not be checked without
  real Claude runs, so `docs/backends.md` says it was not checked and how to check it.
- 2026-10-15: `hrn remote events --watch` ends at the event that ends the job (#165, PR #414). The
  client reconnected whenever a stream ended cleanly, so the watch never exited. It now returns
  with status 0 after printing a `succeeded`, `failed` or `canceled` event, or an
  `admission_timeout` or `recovery_resolved` whose `details.state` is one of those;
  `recovery_required` waits for a person and is not terminal. The plain `events` command and
  `ReadEvents` read to the end as before. A watch started after the job ended was left as #361 (PR
  #509); the server-side close is #156 (PR #470).
- 2026-10-15: a git command is retried once when a slot being created makes it fail (#189, PR
  #426). While `git worktree add` creates a slot, a git command that lists worktrees can die on
  the missing `commondir`; `internal/gitretry` re-runs it once after a short pause, for the batch
  and ws commands outside the create lock, which all fail before changing anything. `worktree add
  -b` is not retried: git creates the branch before dying, so a retry could only fail with
  "already exists" (the review reproduced this on git 2.49). Retried commands run with `LC_ALL=C`.
- 2026-10-15: the knowledge import screen checks every field for keys and tokens (#176, #179, PR
  #424). Private key headers (OPENSSH, EC, ENCRYPTED, generic) and token shapes (`sk-ant-`,
  `ghp_`, `github_pat_`, `AKIA`, JWTs) are refused in every text field, and `.env` matches only as
  a path segment or a `.env.<suffix>` file; a refusal names the field and the pattern, never the
  text. Operator review stays the control, as `server/README.md` says. The smaller points: an
  `Attempt` above int32 refused, multi-word exact matching, `main.go.` matching `main.go`, and a
  schema-2 database told to migrate. The review found a query could build about 17 MB of
  multi-word candidates; capped at 8 words and 256 candidates.
- 2026-10-15: dead CA conditionals dropped and a module download layer added (#171, PR #422). The
  project-worker chart's three `secrets.caName` guards are gone, since the value is already
  required; the server Dockerfile runs `go mod download` right after copying `go.mod`, so
  `server/` changes reuse a cached layer. `verify_charts.py` checks both, and that an empty
  `caName` fails to render.
- 2026-10-15: certain durable failures end without recovery, and every call is recorded (#145, #146,
  #148, PR #420). A definite model rejection (a 4xx other than 408, 409 or 429) saves a
  `model_rejected` checkpoint and ends as an error without `recovery_required`; a context-management
  400 retries once in durable mode as it does locally. Every tool result in the durable transcript
  has its call event, emitted after the saved `tool_intent`. The docs say the checkpoint store is
  the trust boundary for budget, deadline and start, and that a restored, already-paid response may
  finish its tools over the ceiling. #147 waited for a maintainer's call (PR #478).
- 2026-10-15: the continue context warning reaches exec and every new run's record (#102, #104,
  #108, PR #418). Over 150,000 tokens of carried context, exec's `accepted` frame carries the
  warning in its notes, and `followup.Plan.Apply` records it once as an info event in the new run
  on every path, the limit carry-on included. A transcript hand-over to another backend starts
  with no warning. The review found it printed twice in plain mode; it now shows once, and with
  `--json` still goes to stderr.
- 2026-10-15: the recording page's review follow-ups (#92, PR #416). The microphone is released
  after Stop, download mode gets a `metadata.csv` button, Stop posts the last partial block
  (waiting at most 400 ms) so a take ends exactly, unknown `metadata.csv` rows are kept, and
  several races around retakes and mode switches are closed. Item 5, a headless browser check in
  CI, moves to #415.
- 2026-10-15: the shaping test's slow shell start (#380, #401, PRs #387, #413).
  `TestRunCommandShapesLargeOutput` gave every case 2 seconds, and on a loaded development PC
  PowerShell took longer to start. The success and failure cases got 60 seconds (PR #387); the
  timeout case then failed before printing, so it gets 10 seconds while the command still sleeps 60
  (PR #413). The failures depend on load, so neither could be shown failing on demand;
  `TestRunShellTimeoutKillsTheTree`, with 3 seconds, is the next likely flake.
- 2026-10-15: the remote server origin built from the parsed URL, and the CA file read through a
  checked handle (#282, PR #408). A query, fragment or `%2F` in the server URL can no longer reach
  request paths, a scoped IPv6 host stays escaped, and an empty host is refused.
  `HRN_SERVER_CA_FILE` is opened once and checked on the handle (a regular file, at most 1 MiB),
  so a swap between check and read no longer works; on unix a FIFO is refused instead of blocking.
- 2026-10-15: a durable task with `allow_dangerous` is refused on the run path too (#276, PR
  #407). `APIBackend.Run` refuses it before the client, the workspace, any tool or any checkpoint,
  so a stored job that never passed `validateSubmission` ends as one `failed` result rather than
  `recovery_required`, and the worker goes on to the next job.
- 2026-10-15: a killed gate in a few words for the retry prompt and the watch badge (#396, PR
  #402). A killed gate is reported in three forms: `Outcome()`, with the operator's advice, for
  the batch tool, `batch_show`, the `-v` event and the badge's title; `Brief()` (`was killed after
  15m0s by gate_timeout`) for the retry prompt, which cannot act on the advice; and a short badge
  (`killed · gate_timeout 15m0s`), since badges never wrap. A gate that could not start after a
  cancellation is `canceled` too.
- 2026-10-15: an unsupported retrieval submission says so, and the 422 is tested through the route
  (#275, #274, PR #391). `validateSubmission` now says "retrieval is not supported for durable
  jobs", still `ErrUnsupported`, without echoing the value. A PostgreSQL route test checks that
  `allow_dangerous`, retrieval and a retrieval value holding a newline and markup each get 422
  `unsupported`; it skips locally, first ran in CI's `server-postgres` job, and the merge waited
  for it.
- 2026-10-15: `hrn-server` refuses plain HTTP outside a loopback IP literal (#263, PR #390). A
  network `HRN_LISTEN` without TLS let bearer tokens cross the network unencrypted. Without TLS
  files it must now be in `127.0.0.0/8`, `::1` or an IPv4-mapped loopback, checked before the
  database opens; `localhost`, `0.0.0.0` and host names are refused. This follows
  `internal/remote`, the stricter client (the worker still accepts `localhost`). The control chart
  always sets TLS, so nothing deployed changes.
- 2026-10-15: tests run against a temporary `HRN_HOME`, never the user's (#379, #381, PR #386).
  Since PR #371 added `~/.hrn/backends.json`, tests that set no `HRN_HOME` read the user's file, and
  with only claude and agy enabled four tests, and so every full local check, failed; CI has no such
  file. All nine packages that can reach the home get a TestMain that sets a fresh temporary one,
  with a guard test that fails if it is not under the temp directory.
- 2026-10-15: the guard refuses PowerShell's long download names and `iex` given a download (#267,
  PR #375). pipe-to-shell knew only `curl`, `wget`, `irm` and `iwr` piped into an evaluator. It
  now knows `Invoke-RestMethod`, `Invoke-WebRequest` and the WebClient download, and refuses `iex`
  or `Invoke-Expression` given a download as its argument, in parentheses or `$(...)`, with or
  without `-Command`; `iex $script` stays allowed. The call operator and scriptblock spellings
  followed as #338 (PR #430).
- 2026-10-15: a killed gate is reported in three forms (#396, PR #402). `Outcome()` with the
  operator's advice for the batch tool, `batch_show`, the `-v` event and the watch badge's title;
  `Brief()` (`was killed after 15m0s by gate_timeout`) for the retry prompt, which cannot act on
  the advice; a short badge (`killed · gate_timeout 15m0s`) on the watch page, since badges never
  wrap. A gate that could not start after a cancellation is `canceled` too.
- 2026-10-15: a gate killed by gate_timeout says so (#395, PR #399). A gate hrn killed at its
  per-gate limit, or at a cancellation, was `exited -1` everywhere, and a retry hunted its output
  for a failing test; five batches on 2026-10-14 failed that way while the code was fine. Decided:
  `GateResult.killed` ("timeout" with the `timeout` that applied, or "canceled"), additive, and one
  `Outcome()` every printer uses: `killed after 15m0s (gate_timeout; set "gate_timeout" in the spec
  to allow more)` or `canceled`. The retry prompt says the gate did not fail and that only the
  operator can raise the limit. Whether to skip the retry after a timeout is left to a maintainer in
  #395; the review's smaller follow-ups are #396.
- 2026-10-15: a stream that ends on a codex reconnect notice loses its usage (#348, PR #393).
  A `turn.completed` that reported usage, then a notice, then the end of the stream left the
  usage `reported` although the last turn's tokens never were. Decided: the end-of-stream step
  marks it `missing` as `turn.failed` does, keeps the counters so far and clears the estimate.
- 2026-10-15: a stream that ends on a codex reconnect notice writes an error event (#347, PR
  #392). With exit 0 `fail()` writes none, so the transcript held only the dim info line.
  Decided: `CLIBackend.end`, an optional step run once on a clean exit after the stream, told
  whether the stream wrote an error event so it never repeats one (a `turn.failed` after the
  notice already reports it); codex's reports the pending notice. A generic rule in the run
  loop was dropped: claude, gemini and agy also end in error on a clean exit without an event,
  and their transcripts were not this slice's.
- 2026-10-15: list-price estimates for the codex models (#378, PR #383). A second price
  table (OpenAI's list prices, source and date in the file) and an estimate computed in
  the codex stream after each turn with reported usage, cleared on every missing path;
  `cost_is_estimate` and the `~` mark as for the api backend. Every cost hrn shows is a
  list-price estimate, never a bill. The first attempt wired it through `finalize`, which
  switches a CLI's stream parsing off: the review caught it, and a Run-level test with a
  helper-process stream now covers the wiring.
- 2026-10-14: a reconnect notice keeps the error before it (#349, PR #350). #343 stored the notice
  as the pending error even over a usage limit, and the completed turn cleared it. Decided: the
  notice is stored only when nothing else is, so an earlier error stays the run's error and its
  `limited` status.
- 2026-10-14: A Backends card on hrn watch (#346, PR #373). Every viewer sees backend availability, preferences and remembered limits; loopback and Access-verified public viewers can save preferences using a fresh startup key. Changes atomically replace backends.json for later runs, while LAN viewers remain read-only and running MCP servers keep their backend enum until their host restarts them.
- 2026-10-14: codex usage totals cannot wrap (#152, PR #370). Check each turn's
  counters before adding them and mark usage missing on overflow, retaining the
  last representable totals. Later turns cannot restore reported usage.
- 2026-10-14: Backend preferences (#345, PR #371). `backends.json` stores the enabled names in preference order and an optional default, with no record changes and unchanged selection when absent. Runs, MCP, batch retries, evals and supervisor execution respect the preference; routing skips disabled rows and carry-on uses an enabled installed partner.
- 2026-10-14: An agy backend (#344, PR #369). Antigravity's CLI receives the prompt
  as stream-json on stdin, maps modes/model/effort, and records streamed text, result
  usage and conversation IDs using a parser per run. Continuation uses `--conversation`
  in place through the existing session hook; no usage-limit wording is guessed.
- 2026-10-14: watch matches quoted program path delimiters (#237, PR #363). Only the
  matching quote ends a quoted program path, so apostrophes in double-quoted Windows
  folder names survive the scan, including JSON-escaped text. In single quotes a
  double quote that no backslash escapes still ends the scan, since in JSON text it
  closes the string. Unquoted delimiters and the line-end and input-end bounds stay.
- 2026-10-14: index stream sends later changes (#66, PR #362). Test a new run
  appearing in a later replace on the same connection, after reading the first
  fragment, with a short poll interval and a generous deadline. Suppressing every
  replace after the first makes the test fail; production code is unchanged.
- 2026-10-14: CI skips a Markdown-only change (#341, PR #353). Requested: a change touching only
  documentation Markdown (an allowlist: top-level `*.md` but AGENTS.md, `docs/`, the server READMEs)
  runs no build or test. The required `test` jobs still report, skipping their steps, so the
  ruleset's checks pass.
- 2026-10-14: a codex reconnect notice is not the run's error (#342, PR #343). `Reconnecting...
  n/5` is the CLI retrying its model stream: an `info` event that stands as the error only until
  the turn completes, so a run whose work finished is `ok` with its usage, and a stream that
  ends on the notice still reports it.
- 2026-10-14: a take stopped by its context says so (#257, PR #339). The git commands
  before the create lock run under the take's context, so on a loaded machine its deadline
  could kill one and the take reported a broken repository. Decided: `ws.Take` wraps any
  error with the context's once the context has ended, keeping both for `errors.Is`/`As`.
- 2026-10-14: purpose reads past labels and context (#118, PR #319). Leading label
  lines are skipped, a final label contributes its value, and the second non-blank
  paragraph is tried only when the first matches no rule, with the cap applied to each.
  This classifies hrn's batch briefs without changing rules-v1 or letting later
  paragraphs override the task.
- 2026-10-14: a `.dockerignore` for the server image (#170, PR #303). `docker build -f server/Dockerfile .`
  sent the whole repository, untracked files under `server/` included. Decided: an allowlist of
  `go.mod`, `internal/` and `server/` without `server/deploy`, with secret-looking files kept out.
- 2026-10-14: the server's status codes for an unknown tenant or project (#157, PR #302).
  Decided: CreateProject returns 404 for an unknown tenant and ScalingDemand returns 404 for an unknown project; other storage errors retain 500 and 503.
- 2026-10-14: retrieval compare matches the requested model (#184, PR #301).
  Decided: use the task model when recorded; a run without a requested model matches
  only when its reported result model equals the variant model. Show a different
  provider-reported model in the text comparison.
- 2026-10-14: a durable stream that fails part-way keeps its usage (#149, PR #300).
  Decided: count the partial message's usage before returning the durable failure; keep the pending model operation and recovery-required status.
- 2026-10-14: grep and list_files in a workspace reached through a junction (#55, PR #294).
  Decided: walk the resolved root while reporting paths under Root; keep inner junctions untraversed.
- 2026-10-14: public watch's local-use and tunnel address follow-up (#218, PR #261).
  Decided: document that its loopback Host is refused, use a separate plain watch for
  local browsing and `hrn bot`, print the tunnel's loopback target at start, and explain
  a 403 on the bot's existing-watch probe with `--addr` guidance.
- 2026-10-14: issue #217 (PR #260), follow-up to #212: public watch checks Access
  signing keys before serving, reports later fetch failures once per failure streak,
  and shares unlocked, rate-limited certificate fetches across requests.
- 2026-10-14: public watch navigation exemption (#228, PR #259). Decided: only GET/HEAD
  navigations with `Sec-Fetch-Dest: document` bypass the Origin and cross-site check;
  embedded and destination-less navigations follow the normal rule.
- 2026-10-14: the guard refuses gh api calls to publishing endpoints (#210, PR #293).
  Decided: refuse the named REST paths under any method, including harmless GETs, so implicit
  POSTs from fields or --input cannot slip through; refuse GraphQL mutations in the line.
  Ordinary issue, pull request, comment, label and reaction POSTs remain allowed.
- 2026-10-14: a raw `gh api` POST asks first (#206, PR #292). A field (`-f`, `-F`, `--field`,
  `--raw-field`, `--input`) with no method makes `gh api` POST, which can create a release, a tag or
  a deploy key. Decided: in this repository's Claude settings such a call asks first; hrn runs hold
  no GitHub token at all, and #210 adds the guard's refusals as a second layer.
- 2026-10-14: a retrieval packet's entries stay within the text cap (#177, PR #291).
  Decided: count only rendered entry fields (ID, Content, Source, Evidence) against the 8192-byte
  text cap; Key and Identifiers keep the import limits. Require each entry ID in Text's reference label.
- 2026-10-14: gc names a Unix drive by its mount point (#232, PR #290). Decided:
  keep the device number as the grouping key and use the mount point only in messages.
- 2026-10-14: hrn remote checks its server URL and CA file more strictly (#167, PR #289).
  Decided: reject empty queries, opaque and encoded paths, and non-regular CA files before reading; allow symlinks to regular CA files.
- 2026-10-14: hrn remote validates options as its MCP tool does (#166, PR #288). Decided: shared
  validation rejects non-finite max cost before either submission sends a request.
- 2026-10-14: the server refuses mcp and allow_dangerous at submission (#154, PR #287).
  Decided: reject either durable-job option with ErrUnsupported before the worker takes a slot;
  empty mcp lists and allow_dangerous false remain valid.
- 2026-10-14: the batch merge tests save records through the batch package (#215, PR #280).
  Decided: expose the ownership claim and a save wrapper so fixtures use the same value and record writer as Run.
- 2026-10-14: tests give an exited helper's lease its start identity (#242, PR #286).
  Decided: hold the helper alive while reading its identity, then record it with the pid so reuse cannot hold the lease.
- 2026-10-14: the billing-refusal test's deadline (#283, PR #284). Its refusal cases had 500 ms,
  which also had to cover starting the helper; a busy PC timed out first. Decided: 2 s for every
  case; a refusal that failed to stop the CLI still ends as a timeout and fails the test.
- 2026-10-14: gc's idle age and the clock's resolution (#245, PR #278). A cache written moments
  before could read as not yet idle, failing `TestGCTakesTheIdleLimitAsGiven` on Windows CI.
  Decided: an age below zero counts as zero, so `--idle 0` takes any cache; the test writes its
  cache a second in the past, which still shows a 1 ns limit is honoured.
- 2026-10-14: the guard reads a quoted separator as text (#222, PR #277). Decided:
  replace only separators inside quotes, leaving quoted text visible so #223 holds, and
  refuse when a rule matches either the line as written or that copy, so a misread quote
  (`"$(curl ... | sh)"`, an apostrophe in a comment) cannot hide a real pipe.
- 2026-10-14: five slots per repository, ten at most (#256, PR #270).
  Decided: keep a small pool of reusable worktrees, about five, that may grow to ten; reuse them rather than create and delete them.
- 2026-10-14: claude runs refuse per-token billing (#202, PR #269). Decided: when Claude
  Code's init line names an `apiKeySource` other than `none`, stop the worker as soon as
  that init line arrives and end the run with an error naming the source. Claude Code
  may already have sent its first request. An absent field keeps the older CLI behaviour;
  there is no override.
- 2026-10-14: the guard matches Windows program names in any case (#211, PR #268).
  Decided: scope case-insensitive names in disk, pipe-to-shell and sudo; refuse iex and Invoke-Expression sinks too.
- 2026-10-13: issue #233 (PR #241): hrn watch shows a run's workspace by name (`repoOf`,
  as the index does) and a batch's repository too, the whole path as a hover title for this
  computer only. Text from a run's events, its prompt and result, and a batch's tasks and
  gates, and the index's rows show the workspace (a task's worktree) as `.`, home as `~`, and programs in system
  folders by file name, matched only at a path boundary, after secrets are blanked. Records
  and `hrn show` keep the full paths, and a run's purpose comes from its prompt as recorded, as
  `hrn report` has it, not from the shortened text.
- 2026-10-14: the server checks the token before reading a body ([#155](https://github.com/cristiangirlea/harness/issues/155), PR #273).
  Decided: use the path project for submission; worker routes read only the project ID before strict JSON decoding.
- 2026-10-14: hrn remote stops on an oversized event line (#164, PR #271). Decided: a line over
  the 1 MiB scanner limit is terminal for Watch, with the limit named and bufio.ErrTooLong wrapped.
- 2026-10-14: durable project workers and project retrieval reach main ([#142](https://github.com/cristiangirlea/harness/issues/142), PR #266; item 17's last box).
  Eight slices (PRs #143, #150, #158, #159, #175, #174, #181, #186), each reviewed with a security
  read and merged through its pull request into `durable`; main merged in once at db7dbe6 with
  five conflicts resolved: AGENTS.md's table plus a `server/` row; item 17 and the slice entries
  folded into this log, with main's lone 2026-10-13 entry moved below its 2026-10-14 ones to
  keep the dates in order; both sides' MCP guide text; both sides' usage tests; main's batch code
  with the three retrieval hooks. The final review found two documentation gaps, fixed: the
  retrieval plan's evidence pointer and `original_prompt` in the records guide. Follow-ups: a
  backend switch under retrieval (#262), plain HTTP on a network listen address (#263), codex
  cache reads counted twice outside ModelTokens (#264), two types named Usage (#265). Open
  gates, as the docs say: a separate untrusted executor, native CLI sessions under durable
  execution, a live cluster, backup and restore, soak, the retrieval pilot on real runs.
- 2026-10-14: a bounded branch probe in `removeTask` (#216, PR #255). Its `show-ref` ran with no
  timeout, unlike the one-minute git calls around it. Decided: the same minute, and the probe
  (`for-each-ref`, which answers a missing branch with no output) says whether it answered;
  a branch counts as removed only when it was there and a probe answered that it is gone.
- 2026-10-14: batch merge names the commit to undo to (#214, #216, PR #254).
  Decided: CLI and MCP capture HEAD before merging and print that full commit when at least
  one task merges, including before a later conflict or when --rm removes nothing.
  The reflog note appears only when --rm removes task work.
- 2026-10-13: retrieval wiring in runs, batches, evals and MCP ([#141](https://github.com/cristiangirlea/harness/issues/141), PR #186; slice 8 of #142).
  The snapshot's wiring on today's code: the packet is prepared and saved before a backend
  starts, pins are validated on continuation, a same-scope batch retry retrieves again with
  failure evidence, eval variants and the comparison manifest, `hrn retrieval fixture|compare`,
  a project-scoped MCP server, and every `HRN_*_TOKEN` stripped from children. The review found
  one defect, a rejected pin swallowed by followup, now a refusal behind
  `runlog.ErrNoConversation`. CI's PostgreSQL job caught what no local run could: the client's
  frame-marker check refused every real packet, since the service writes `[Reference ...]`
  labels into the text; the check is now structural, one label pair per entry and no outer
  frame marker, and the test packets carry the labels. Follow-ups: a retry after failed
  required retrieval (#182), a cross-backend continuation's pin (#183), compare with resolved
  model names (#184), tests the review named (#185).
- 2026-10-13: approved project knowledge and text retrieval in the control service ([#140](https://github.com/cristiangirlea/harness/issues/140), PR #181; slice 7 of #142).
  The snapshot's contracts, migration 003, service, routes, bounded client search and
  `hrn knowledge`; the parts slice 4 cut from store.go and http.go return, and the
  `Options.Retrieval` check waits for #141. The review's security read found no defect in the
  service; three small ones closed: the credential's length is checked before any knowledge
  query, a storage error during authorization answers 503 rather than 404, and content carrying
  the packet frame markers is refused at import. Follow-ups: the narrow deny list (#176), packet
  entries not tied to the capped text (#177), the GIN pre-filter over every tenant (#178), four
  smaller points (#179), tests the review named (#180).
- 2026-10-13: development charts, image and chart verifier ([#139](https://github.com/cristiangirlea/harness/issues/139), PR #174; slice 6 of #142).
  The snapshot's three charts, fixtures, verifier and Dockerfile, trailing blank lines trimmed
  and the deploy docs' paths turned into links; a CI job verifies the charts and builds the
  image on the hosted Linux runner. The review's security read found nothing open: no secret in
  any file, Secrets by name, images pinned by digest and unpinned ones refused, the fake-model
  sidecar without the PVC or the worker token, non-root read-only containers, default-deny
  policies. Follow-ups: a .dockerignore (#170), two minor template and image points (#171), a
  rerun of the disposable cluster check with an image from this branch (#172), stronger
  verifier assertions (#173).
- 2026-10-13: hrn remote and the MCP remote tools ([#138](https://github.com/cristiangirlea/harness/issues/138), PR #175; slice 5 of #142).
  The snapshot's client, CLI and tools, with the commands in docs/commands.md and the tools in
  docs/mcp.md. The review's security read found one hole: plain HTTP was also accepted for the
  name `localhost`, which a resolver can point anywhere; now only a loopback IP literal. A watch
  error names the status without the server's body. Tests added that the token text reaches no
  error, that a `token` or `authorization` field in a tool input is refused, and that a job id
  not shaped by the server is refused before any request. Follow-ups: an oversized event line
  spins the watch (#164), `--watch` never exits (#165), the CLI skips options.Validate (#166),
  URL and CA file edge cases (#167), no client-side cost cap (#168), tests the review named
  (#169).
- 2026-10-13: the server module ([#137](https://github.com/cristiangirlea/harness/issues/137), PR
  #159; slice 4 of #142). The snapshot's PostgreSQL control service and project worker, cut to the
  control plane: the knowledge pool, retrieval slots and schema 003 wait for #140, the deploy charts
  and image for #139, the remote-client test for #138. CI: server unit jobs on both hosted runners
  and a PostgreSQL race job on Linux. The review's security read found nothing blocking; tests for
  token scope across tenants and projects, wrong tokens of the real length, submission validation,
  migration idempotence and checkpoint sequence were added and passed in CI's postgres job, where
  the suite runs: Docker's engine on the development PC answers with an internal error. Follow-ups:
  refuse mcp and allow_dangerous at submission (#154), the token check before the body (#155),
  bounded event streams (#156), two status codes (#157).
- 2026-10-13: agent usage metadata ([#136](https://github.com/cristiangirlea/harness/issues/136), PR #158; slice 3 of #142).
  Ported from the snapshot: codex input includes its cached input, api and claude counters are
  disjoint, and a missing counter, an incomplete turn or a failed turn marks the run's usage
  missing, which it stays. A missing `cached_input_tokens` field counts as missing too. The
  review found the report had dropped gemini and opencode records from the cache share, the
  snapshot's bug as well: a record without metadata keeps the old sum. Follow-ups: KnownCost
  has no caller (#151), summed counters can wrap (#152), records.md's stale README reference
  (#153).
- 2026-10-13: api durable checkpoints ([#135](https://github.com/cristiangirlea/harness/issues/135), PR #150; slice 2 of #142).
  Ported from the snapshot onto today's loop with two deviations: a `pause_turn` saves its
  `turn_complete` checkpoint and then continues, so the local path keeps its plain `continue`;
  and the context check before a model request runs only in durable mode. `durable.go` is
  otherwise the snapshot's file. The review (read-only, with a security read: nothing open)
  left five follow-ups as issues: certain failures reported as `recovery_required` (#145),
  tool results without their call events for refused calls (#146), `AttemptID` in the task
  digest (#147), a checkpoint's budget and clock trusted as stored (#148), a part-way failed
  stream's usage lost (#149). Inside an hrn run `go test ./...` fails in `internal/gc` (#120);
  the full suite passed outside it.
- 2026-10-13: Durable project workers and project retrieval ([#142](https://github.com/cristiangirlea/harness/issues/142))
  land in slices onto the feature branch `durable`: each slice is one pull request into it,
  and the feature branch reaches main through one pull request reviewed for how the pieces fit.
  The root module stays standard-library only, as AGENTS.md requires; the separate `server/`
  module may depend on maintained PostgreSQL and Kubernetes clients (pgx, client-go), as the spec accepted on 2026-09-27
  allows. The snapshot stays local as commit `8fc689e` and is never pushed.
- 2026-10-13: issue #233 (PR #241): hrn watch shows a run's workspace by name (`repoOf`,
  as the index does) and a batch's repository too, the whole path as a hover title for this
  computer only. Text from a run's events, its prompt and result, and a batch's tasks and
  gates, and the index's rows show the workspace (a task's worktree) as `.`, home as `~`, and programs in system
  folders by file name, matched only at a path boundary, after secrets are blanked. Records
  and `hrn show` keep the full paths, and a run's purpose comes from its prompt as recorded, as
  `hrn report` has it, not from the shortened text.
- 2026-10-13: AGENTS.md back under 100 lines (#196, PR #253). Decided: shorten layout
  rows and the hx note, and point repeated CI and release details to CONTRIBUTING.md.
- 2026-10-13: three wording points from #58's review (#65, PR #252). Decided: a blocking finding is
  fixed, as AGENTS.md and the merge rule say, so the definition of done no longer offers rejecting
  one; a finding shown wrong is no defect, so it was never blocking. AGENTS.md says "the base
  branch" (a piece of a feature branch merges that one in) and "a broken house rule" (PLAN.md's
  rules and `.claude/rules/` count too).
- 2026-10-13: the definition of done and rule 2 follow AGENTS.md (#64, PR #251). Since the
  repository went public, CI again tests both platforms on every pull request, so rule 2's
  "green on both" stays. Decided: the definition of done points at AGENTS.md's checks instead
  of its own older list, asks for the `docs/` guide as well as the README, and names the
  entry an issue-only fix adds; rule 2's branch-name example is current. Whether a blocking
  finding may be rejected rather than fixed is #65.
- 2026-10-13: the records rule names the right guide (#153, PR #250). `.claude/rules/records.md`
  sent a change to what a record holds to the README's "Run records" section, which moved to
  `docs/run-records.md` (PR #107). Decided: the rule points at the guide.
- 2026-10-13: the tree-kill test's timeout (#68, PR #249). `TestRunShellTimeoutKillsTheTree`
  gave the shell 500 ms, and on a busy Windows PC Git's bash had not printed `started` by
  then, so the test failed now and then, in CI too. Decided: three seconds. Its time bound
  was 10 s, which a shell-only kill also met (RunShell waits 5 s for the pipe), so it is now
  the timeout plus 4 s: only a tree kill returns that soon.
- 2026-10-13: a reused process id does not hold a slot (#231, PR #248). Windows can
  reuse a pid soon after its holder exits. Decided: record the holder's process start
  identity in the lease and compare it exactly on Windows and Linux; a different start
  means the live pid does not hold the lease. A wall-clock comparison with the lease's
  taken time could give a live holder's slot away after the clock steps backward.
  Leases without a start identity, or whose current identity cannot be read, use pid
  liveness as before.
- 2026-10-13: CONTRIBUTING.md and immutable tags (#197, PR #247). Since ruleset 24992042 a
  pushed `v*` tag cannot be moved or deleted. Decided: the release section says so, and that a
  wrong tag or a release needing a code fix takes a new version, while a workflow that
  failed for another reason is re-run on the same tag.
- 2026-10-13: gc moves a cache aside before deleting it (#129, PR #246). A build can
  start after the lock probe and before deletion. Decided: rename to a sibling
  `.gc-<pid>-<unix nanos>-<name>` first, probe the moved locks again (including `flock` on
  Unix), moving it back if held, then delete the tag last. A rename failure keeps the
  cache; a failed deletion leaves a tagged leftover, which is scanned next run and can go
  regardless of idle time.
  The worktree pass is left as it is: `git worktree remove` does its own deletion.
- 2026-10-13: gc and links out of a worktree (#94, PR #240). A tracked folder
  replaced by an identical junction can look clean to git, while worktree removal
  deletes the outside copy's files. Decided: check the whole worktree for links
  leading out before a Remove verdict and again just before Apply removes it;
  skip its own `.git` and inner worktrees git lists, and keep on walk errors.
- 2026-10-13: gc --free-below reports the drives after --apply (#130, PR #234). File sizes can
  overestimate freed space for sparse, linked or compressed files, and removal can fail.
  Decided: nothing more is removed; drives that were below are measured again and reported;
  the next-run hint appears only when caches were left because enough was already going.
- 2026-10-13: gc and squash-landed batch work (#85, PR #227). Ancestry alone keeps task
  worktrees after `batch merge --squash` or a reworded landing. Decided: compare the patch
  id (`--verbatim`, whitespace included) of the task's whole diff with each commit on
  origin's default branch since their merge base; remove only on an exact match. Everything
  else keeps, including a 2000-commit bound.
- 2026-10-13: issue #226 (PR #230): public watch accepts GET and HEAD page navigations
  after Cloudflare Access login even when the browser sends a cross-site fetch site or
  `Origin: null`. The public Host and Access token checks still apply; other requests
  retain the existing origin checks, and local mode is unchanged.
- 2026-10-13: the guard's `gh api` method in quotes (PR #223). After PR #213 a quoted method
  still got through `gh-publish`: the shell strips `"DELETE"`, `'delete'`, `\"DELETE\"` and
  `$'delete'` before gh sees them. Decided: quotes, a backslash and `$` may stand between the
  flag and the method. A method in a variable stays out of reach of a textual guard; a `|`
  inside a quoted argument is #222.
- 2026-10-13: issue #212 (PR #220): `hrn watch --public` keeps the listener on
  loopback for a same-PC Cloudflare Tunnel, requires the Access team and audience
  together, and verifies Access JWTs on every request. Public requests are always
  remote to the page, and a private JSON access log records their outcomes.
- 2026-10-13: the guard's `gh api` method in any case (PR #213). `gh-publish` refused
  `gh api` with PUT, PATCH or DELETE in upper case only, and `gh` upper-cases the method it is
  given, so `-X delete` still deleted. Decided: the method alone matches case-insensitively
  (`(?i:PUT|PATCH|DELETE)`); `gh`'s subcommands stay case-sensitive. POST is #210, Windows
  program names in other rules #211.
- 2026-10-13: `gh api` writes in this repository's Claude settings (#198, PR #209). Only the `PUT`
  spellings were denied, so a session with a maintainer's token could still change or delete the
  rulesets. Decided: deny any `gh api` call with an explicit `-X` or `--method`, whatever the
  method's case or spelling (`gh` upper-cases it and takes `-X=DELETE`); nothing here reads with an
  explicit method. A field without a method is an implicit `POST`, left to #206.
- 2026-10-13: gc tests under the guard (#120, PR #205). The fixture's git now drops
  passed-down git config like gc's own git, so `go test ./...` passes inside an hrn run.
  The guard's refusal of pushes to real remotes remains tested in internal/tools.
- 2026-10-13: issue #204 (PR #207): lean dropped the user's own rules, plugins and skills, which
  workers must keep. Decided: lean is `--strict-mcp-config` only (no `--setting-sources`), so it
  drops only MCP servers hrn did not pass (the user's, plugin-provided ones and claude.ai
  connectors). `lean` became a `*bool` in `agent.Options`: unset means the entry point's default,
  `false` is honoured over a spec's `true` (the one switch a task can turn back off). The JSON is
  unchanged (`true` or absent, now also `false`), so old records load. Batch, and with it eval and
  the MCP `batch` tool, makes lean the default per attempt only on a backend that applies it
  (`agent.AppliesLean`, claude), so a codex task's record does not claim lean and prints no `not
  enforced` line; `hrn run` and MCP `run` stay opt-in. New `compact_window` (an integer
  100000-1000000 or `"auto"`, as `claude --autocompact` takes it; it accepts the bare integer) is
  passed to claude only and reported as not enforced by the other backends, the api loop included.
  Measured: lean 55.1K context against 61.7K by default on a two-turn task (docs/backends.md).
- 2026-10-13: issue #200 (PR #201): a claude worker inherited every plugin, skill, hook and MCP
  server of the operator's, about 64K tokens of context on every turn of a tiny task. Decided:
  an additive `lean` option (`--lean`, spec, task and MCP `run` field, off by default) that
  adds `--strict-mcp-config --setting-sources project` to the claude args, before the guard's
  `--disallowedTools` rules, which stay as they are. Not `--bare`: it logs in only with an API
  key (OAuth and the keychain are never read) and skips `CLAUDE.md` discovery. User memory
  (`~/.claude/CLAUDE.md`, `~/.claude/rules`) stops loading too: Claude Code 2.1.284 reads it
  only with the `user` source; the repository's `CLAUDE.md` and `AGENTS.md` import still load.
  codex, gemini and opencode report `lean` as not enforced; the api loop, which reads no Claude
  Code settings, does not warn. Not listed in the backends' capabilities: the exec protocol's
  `start` message has no `lean` field. An eval variant cannot set lean on its own yet.
- 2026-10-13: PR #199 (closes #193): a task waited until the batch was canceled on a slot
  whose lease stayed unreadable (empty, not JSON, or a stale lease a left `<n>.lease.takeover`
  file blocks), since `takeSlot` counted every lease it could not read as one that may come
  free. Decided: `ws.NoSlotError` gains an additive `Unreadable` field naming those slots,
  and the empty-lease line names the file like the other two. The task still waits on them,
  since a lease being written reads on the next look, and keeps waiting while any live
  process holds a slot in its range; when the unreadable leases are all that is left to
  wait for and every one has stayed unreadable for `unreadableWait` (30 seconds, a package
  variable the tests shorten), it fails as `error` naming each file and how to clear it
  (`hrn ws list`; a lease that stays unreadable or a left `.takeover` file only while no hrn
  command at all runs on the repository, since a live take creates its lease empty before
  writing it and a live `hrn ws free` or takeover holds the `.takeover` file while the lease
  it guards may name a dead process).
  Timed per slot from the task's first look that found it unreadable; a look that does not
  find it so, a wait at the batch's bound included, forgets it. A slot whose lease changed
  hands under the take is held, not unreadable. Nothing is removed by hrn.
- 2026-10-13: `main` and `v*` tags are protected by GitHub rulesets (PR #195). Branch ruleset
  24992040 (pull requests only, merge commits only, `test (ubuntu-latest)` and
  `test (windows-latest)` required, no deletion, no force push) and tag ruleset 24992042 (no
  deletion, update or force push; creating a tag still runs `release.yml`), both without bypass
  actors, created 2026-10-13. AGENTS.md's working rules say what GitHub itself refuses.
- 2026-10-13: PR #194 (closes #161): a batch waited while sessions (`hrn ws take`) held every
  slot, for as long as a person kept one. Decided: a batch's bound is `max(parallel, 4)` slots
  of its own, running tasks' and kept ones together, counted from the leases that name its
  pid (`ws.CountLeases`) before every take; at the bound a task waits for a running task's
  slot and fails when all are kept. It tries the slot numbers from 1 that hold that many
  slots other than sessions' (`ws.RangeBeyondSessions`: a lease without a pid is skipped, a
  lease that cannot be read counts), worked out again at every try, so a session among the
  first slots is passed over and one further up widens nothing. It waits only while a slot in
  that range is held by a live process (another batch, or its own running task;
  `ws.NoSlotError.Holders`) or has a lease it cannot read (one that stays unreadable keeps
  it waiting for ever: #193), and fails when the rest are sessions', skipped or kept. A
  session can take a slot in the range between working it out and the take scanning that
  slot, so a take that passed over a session works the range out again and tries again at
  once while the range reaches further, three times at most. A first
  cut added a session count to the range, which let a session make a batch wait for ever,
  a distant session widen the range, and the bound break when sessions freed their slots.
  `hrn ws take`'s own default `--max` is unchanged.
- 2026-10-13: PR #192 (closes #160): the recovery of a slot a batch kept, documented in
  docs/workspaces.md and followed step by step in ws's tests, with no code change. Decided:
  the work is saved on a local branch first, then the slot is detached at a commit a
  remote holds (origin's default branch), because Free counts a commit at HEAD as
  unpushed when no remote-tracking branch holds it, whatever local branch holds it too;
  Free asks for a push, unlike Release. In a repository without remotes Free counts only
  the commits no branch or tag holds, so it frees the slot in place once the branch
  exists. The branch is merged by hand unless the task passed, since batch merge skips
  every task that did not pass, and ignored files are checked first, since a checkout
  overwrites them where the target commit tracks the path.

- 2026-10-13: Concurrent takes skipped free slots (#188, PR #191). CI's Windows run of
  `TestConcurrentTakesNeverShareASlot` got 3 slots for 6 takes with `--max 4`. Reproduced
  here under load: `git worktree add` for slot 4 died with `fatal: failed to read
  .git/worktrees/3/commondir: No error`, because git creates a new worktree's admin folder
  in steps and every command that lists worktrees (add, list, lock) dies on a half-written
  one. Not git's lock files, so retrying would only narrow the window. Decided: takes, in
  any process, add, list and lock slot worktrees under an exclusive OS file lock on
  `.hrn/slots/.create.lock` (`flock` on Unix, `LockFileEx` on Windows), waiting with a
  backoff up to 2 minutes (and the context). The OS drops the lock when its holder exits
  or dies, so there is no staleness rule and the file is never removed. A first version
  used an O_EXCL file taken over after 2 minutes; review found that two waiters could
  both take over one lock, and a holder's unlock remove a successor's, while a slow
  `git worktree add` (up to 5 minutes) still ran. Switches and other git commands
  (batch's, in other processes) that list worktrees are not under the lock; they can
  still die during an add.
- 2026-10-13: PR #187 (closes #162): hrn batch rm and prune leave what another batch made
  when a fresh task found its worktree or branch taken. Decided: a fresh task claims its
  worktree and branch through `branch_made`, as a slot task claims its branch; batch records
  now carry `ownership`, and in records that have it rm, prune and WorktreesRemain trust only
  that claim. Records from before it skip a fresh task only for hrn's own collision error,
  raised before any attempt. The claim is saved after the worktree is made and withdrawn
  before a retry removes it, so a failure leaves things behind rather than removing another
  batch's: a claim that cannot be saved removes the new worktree again, and a removal that
  fails claims again only the registered worktree or branch that is still there.
- 2026-10-13: PR #163 (closes #126): hrn batch takes its worktrees from the reusable slots of internal/ws.
  Decided: slots by default, a new `.hrn/worktrees/<id>` per task only with
  `"worktrees": "fresh"`; any other value is refused. Each running task leases a slot
  (holder `batch <id> task <task>`, hrn's pid), its branch `hrn/<id>` made from the batch's
  base before the take, so a same-named branch on origin is never tracked. A fresh retry
  resets the same slot's branch to the base, and the earlier attempt's commit leaves the
  branch, as it did when fresh mode re-made the worktree. When the task ends, the slot is detached at the base and given back with the new
  `ws.Release`. Release does not require a push, because the branch is local by design:
  batch merge and batch rm read it. A slot left dirty by a refused release keeps its lease
  until hrn exits, and later takes skip it, never clean it. A task waits, rather than
  fails, while every slot is taken (`ws.NoSlotError`, looking again every 5s until
  canceled). ws's take now skips a slot only for commits a switch would strand (on no
  remote, branch or tag), since a released slot's commit is on its local branch. Diff of a
  slot task without a commit is empty rather than reading a slot another task may hold.
  batch rm deletes only a slot task's branch, refusing while it is checked out. prune keeps
  a slot batch's record while its branches remain. eval keeps fresh worktrees for its judge
  and refuses slots with one.

- 2026-10-13: hrn ws (#133), item 16's fourth box: a fixed set of reusable worktrees per
  repository at `<main checkout>/.hrn/slots/<n>`. Decided: slots are locked worktrees, so
  gc keeps them without a change to internal/gc; leases are O_EXCL files beside the slots
  (`<n>.lease`), and a lease whose pid has exited is stale and taken over under a
  `.takeover` file, so two callers never remove each other's fresh lease; take never cleans
  a dirty slot, it skips it and says why; free refuses unpushed work and detaches HEAD, so
  the branch is free for other checkouts and for gc; CLI only, no `hrn mcp` tool. The
  package is exported with a pid on the lease so batch can use it (#126). Its test fixture
  publishes to origin by fetching from origin's side, since hrn's guard refuses every push.
- 2026-10-13: PR #127 (closes #82, widened on 2026-10-12): hrn watch's rows lead with `<repository>
  · <task>`, the id under it. Decided: the repository is the workspace folder's name (the folder
  above `.hrn` for a batch worktree), read from the record, not from git or the disk; a task's name
  is the bot's `taskName`; a batch task named after its issue (`82-repo-and-task`) shows that number
  with no new field. Task names come only from batch records cleaned of secrets, all of them, since
  `taskName` changes text and blanking is by exact match. Built through hrn: a batch task, then hrn
  runs for the checks, two codex reviews and the fixes. One security defect and one id-line defect
  fixed with tests seen failing; the rest is #124 and #125.
- 2026-10-12: PR #123 (closes #63), item 15's third box, which closes item 15. hrn watch's
  index has a Purpose column (the rule id in its title) and a card, "What the runs were for",
  over the listed runs only. Decided: a finished run with no reported cost is left out of the
  sum and counted, never $0; a row with any such run reads "≥", rounded down so the bound is
  never too high, or "-" when none reported; running runs count only as running; the card is
  built in Go as ready strings and sits in the "lists" template, so the stream carries it. Built
  through hrn: a batch task wrote it, and hrn runs did the checks, three reviews (codex, then
  claude when codex's sandbox would not start a shell) and the fixes. Six findings fixed as
  their own commits, two of them the phone layout found by the browser check; the rest is #122.
- 2026-10-12: PR #121 (closes #62), what a run was for, item 15's second box. Decided: only
  the prompt's lead is classified (its first non-blank paragraph, at most 600 bytes), since a
  brief's later paragraphs name every keyword; keyword rules adapted from classify-sessions
  rules-v1 with hrn's reviewer brief first, not a model call, so it is free and testable, and
  a prompt no rule matches is `unknown` rather than a guess; computed when read, so old records
  get a purpose and no format changes. Built through hrn: a batch task wrote it, and hrn runs
  did the checks, two codex reviews and the fixes. Three blocking findings, each fixed as its
  own commit; the rest is #118 and #119. The checks found that gc's tests cannot pass inside
  an hrn run, because the guard's push refusal reaches their fixture (#120, already on main).
- 2026-10-12: The README's quick start names how to install and sign in to the Codex CLI
  and introduces `hrn bot` (Ada) next to `hrn watch`, so a newcomer sees both from the
  entry page (issue #110, PR #111).
- 2026-10-12: Simplify the public README (PR #107) into an introduction, quick start and guide
  index. Move the detailed reference into `docs/` and contributor/release guidance
  into `CONTRIBUTING.md`; keep permission boundaries visible in the README.
  Remove unsupported language-performance comparisons and the install suggestion
  to download releases when no releases have been published. Runtime behavior is unchanged.

- 2026-10-12: Public-repository CI migration (PR #106). Use standard GitHub-hosted Linux and Windows
  runners, including both PR legs, with read-only CI token permissions. Gate CI and releases on
  public visibility to avoid private-minute usage. Retire the repository's desktop runner and ARC
  scale set before changing visibility; infra owns the retirement. The migration and publication
  were authorized. This supersedes the private-only runner rules below. Historical entries remain a
  record of earlier configurations.

- 2026-10-12: A dangling symbolic link out of the workspace on Linux and macOS (#47). The
  non-Windows `throughLink` now reads a link `EvalSymlinks` left unresolved and judges the
  path by its target, as Windows' did since #45. It is its own short walk in `links_other.go`,
  not Windows' `throughLinkWith` shared: that one's mount-point and volume rules have no
  meaning here. Found while writing the test: a target joined as text loses a `..` the
  system takes after following a link (`s -> evil/../x`, `evil` a link out, read as `x` in
  the workspace and written outside), so a target with a `..` after a name is refused. A
  leading `..` is taken from the link's own directory, which is real, and still works. Found
  in review: the walk first ended on any `Lstat` error, but a chain of links whose whole path
  passes `PATH_MAX` makes every look fail with `ENAMETOOLONG` while the system, following
  each target on its own, still reaches the end; only a missing component or one under a
  file ends the walk now, and any other error refuses. Checked on Linux in a container (the
  PC's WSL would not start); macOS only by `GOOS=darwin go vet`.
- 2026-10-11: `hrn gc` (#78). Proof of a merge is GitHub's word on the exact commit: a merged
  pull request whose head is the worktree's `HEAD` or contains it, and no open one for the
  branch. Ancestry of the default branch alone was not enough for a branch: a worktree made
  from `main` a minute ago has nothing of its own yet and would pass. It is enough for a
  batch's worktree, whose record says when the batch has finished. A detached `HEAD` has no
  pull request and stays. Failing closed: when `gh` or the fetch fails, the worktrees that
  needed them stay. Two rules came from how `git worktree remove` behaves: it deletes ignored
  files, so a worktree whose ignored files are not build output (a `.env`, notes) stays, and
  a worktree holding another that stays (`.hrn/worktrees` is ignored) stays too. `--force`
  would not check the files at all, and without it git checks only for changes, so `--apply`
  checks each worktree again just before removing it (`HEAD`, branch, lock, files). A one-hour grace
  after the merge spares the session that merged it. CLI only: tools that delete branches
  stay out of `hrn mcp`, as recorded for `batch rm`. An independent review (codex, read-only)
  found seven ways it could still delete unproven work, each fixed with a test seen failing:
  a parent removed after its child's removal failed, no second look before removing, a
  check-then-delete race on the branch (now one `update-ref -d`), `.hrn/` taken whole as build
  output, a stale or guessed default branch (now asked of origin), an open pull request lost
  past the list's limit (now a query of its own), and unreadable batch records skipped (now
  everything stays). Four more rounds and a security read found the ways around build output
  (a file named like a build folder, a signed-looking tag in a tracked folder, a repository,
  bare repository or link inside it, a junction above it), a worktree made inside a parent
  after the scan, a local ref shadowing origin's, git pointed elsewhere by `GIT_DIR` or
  passed-down config, assume-unchanged files, and replace refs; each was fixed the same way.
  Batch work that landed squashed is #85; a tracked folder swapped for a junction is #94.
- 2026-10-13: `hrn gc`, the cache pass (#79). A build cache is a folder tagged `CACHEDIR.TAG` with
  the convention's signature, found at each PATH and inside each worktree that stays, two levels
  down; one inside a worktree that goes is the worktree's. "In use" is Cargo's own signal, its
  open `.cargo-lock`: an open that shares nothing on Windows, a non-blocking `flock` elsewhere,
  and a lock that cannot be checked counts as held. The last write is read from folder times (the
  profiles, their `deps`, `.fingerprint`, `build` and `incremental`), which catch what a build
  removes, and from the walk that sizes the cache, whose newest time catches a file rewritten in
  place or deep inside a cache with no profile (pytest's); an independent review found the folder
  times alone missed the latter. `--free-below` works per drive (a volume's mount point on
  Windows, not its letter), oldest first, counting what the scan already
  marked, and a drive whose free space cannot be read loses nothing more. Kept from the worktree
  pass: a tagged folder git tracks a file in, a repository or a link out inside it, the current
  directory inside it, and a second look before each removal. Free space comes from
  `GetDiskFreeSpaceExW` through `syscall.NewLazyDLL` and from `syscall.Statfs`, the lock probe from
  `syscall.CreateFile` and `syscall.Flock`: standard library only, as the rule says. A security
  read added two guards: a worktree entry is swept for caches only when its path's `.git` is a
  file naming a folder under the repository's `.git/worktrees` (`git worktree list` prints
  whatever a stale or edited entry says), and only git's discovery failure counts as "outside a
  repository", since a worktree whose repository is gone fails with another message and its
  files are tracked. A build starting in the moment between the last lock probe and the removal
  is #129; saying how far a drive still is below `--free-below` after `--apply` is #130.
- 2026-10-11: PR #93 (closes #91), recording Ada's voice, the first slice of #83. The voice will
  be recorded by a volunteer speaker with their consent; the recordings stay on this computer,
  never in a repository. Decided: the page records in the browser and saves into a folder the
  speaker picks (File System Access, downloads otherwise), so hrn watch stays read-only and
  keeps no one's voice; it is refused from the network. The microphone is taken as it is (no
  echo cancellation, noise suppression or auto gain) and saved as 16-bit WAV with `id|text`
  rows, the layout Piper and most trainers read. Ada's lines come from her own wording
  functions, so the voice is trained on what she says; their numbers are a contract
  (`testdata/voice-lines.golden`), since a take finds its text by its number. Training will use
  only models under MIT or Apache licences, and from ChatGPT conversations only her side is
  recorded, through a shared microphone with headphones. Rounds of independent review through
  hrn (claude; codex at its limit) found four blocking defects and smaller races this branch
  brought in, each fixed and checked in the browser; the rest is #92. Every handler on the page
  awaits, so each says what may happen while it waits: one microphone setup at a time, nothing
  moves while a take is saved, Keep and Retake never overlap, and a talk's parts save in order.
- 2026-10-11: PR #89 (closes #59), Ada, part 1 of item 11. Decided: the sentences are made in Go
  (`internal/watch/bot.go`), not in the page, so they are tested and pass the same redaction as
  every page; one stream per page, the visible card as escaped HTML and the sentences as JSON.
  Read-only: her commands steer her alone, decisions wait for #38. Speech is the browser's, with
  voices on this computer first, since an online voice sends prompts to its maker; a nicer voice
  of her own is #83. She is named Ada, an animal rather than a human, and a parrot rather than
  an owl; she is an inline SVG with CSS animation, as the page loads nothing from elsewhere and
  allows no inline style. Kept out of this branch under the one-purpose rule: naming a batch's
  runs by their task on the index (#82, a question about four look-alike runs). Three
  independent reviews through hrn (claude; codex at its limit) found four blocking defects, each
  fixed with a test seen failing; the rest is #88.
- 2026-10-11: Fuzzing by count (#70). FuzzCodec failed PR #60's job with `context deadline
  exceeded` at exactly 10 s and no input saved, as it had once before (the PR X entry
  below). The cause is in Go's coordinator (`internal/fuzz/fuzz.go`):
  - with `-fuzztime=10s` it closes the deadline context before it cancels the workers'
    child context;
  - `stop` drops the deadline error only if it equals the child's, which is not set yet;
  - the window is the deadline's goroutine being held up between closing the parent and
    storing the child's error: the OS pausing it on a loaded node, or the child's lock,
    which each minimization's own timeout context takes as it starts and ends.

  It never happened in 288 runs on the development PC, so it is a loaded-runner race; reading
  the source, not a repro, is the evidence. With a count (`-fuzztime=Nx`) Go makes no deadline
  context and stops through `stop(nil)`. The 2026-10-10 entry's FuzzParseSSE failure on #34
  (`context deadline exceeded` at 0 execs/s, put down there to garbage from a 1 MiB buffer) fits
  this race after the minimization stall below better; that entry stays as recorded.

  FuzzParseSSE also did no executions after a few seconds in every run looked at.
  `GODEBUG=fuzzdebug=1` showed all four workers minimizing "interesting" inputs, which Go
  allows 60 s each, so minimization is now bounded too (`-fuzzminimizetime=200x`; a count
  ends it gracefully, where a duration stops and restarts the worker and throws the
  minimization away). The counts (500000x for the codec, 150000x for SSE) keep each step
  at about 10 s on the lab runner. Part of why minimization was slow is the parser's own
  cost, quadratic in its deltas: #71.
- 2026-10-11: Streamed text in linear time (#71). `parseSSE` added each text, thinking and
  signature delta to its block's string, which copied the whole text every time:
  - a stream of 64,000 small deltas (about one maximum-length response) parsed in 2.75 s;
  - 32,000 deltas in a 3 MB stream allocated 1.27 GB, about 410 times the stream.

  It also slowed FuzzParseSSE's minimization of large inputs (#70). Each block's deltas
  now go into a `strings.Builder`, written into the block at its `content_block_stop`, or
  by a `defer` when the parse ends any other way. The partial response of a cut-off
  stream keeps what arrived (the api loop reads only its usage today), and a
  `message_start` first writes what was gathered into the blocks it belongs to, since its
  decode can replace or shrink the content even when it then fails (the review found
  that path panicking; it is a FuzzParseSSE seed now). The blocks and the `onDelta` calls
  are the same byte for byte, so the history stays append-only. 64,000 deltas now parse
  in 59 ms. The test checks bytes allocated (under 32 times the stream; about 10 now),
  not time, so a busy runner cannot make it flaky.
- 2026-10-11: Short-name temp dirs in execproto's tests (#57). Two continuation tests have
  failed on the Windows runner since #53. They seeded the earlier run with a raw
  `t.TempDir()`, which the runner spells in 8.3 short names (`C:\WINDOWS\SERVIC~1\...`), and
  continued from the long spelling, so `followup`'s `samePath` saw two folders. Real runs
  record the long one: `hrn run`, MCP `run` and `execproto` go through `NewWorkspace`, and
  batch and the eval judge record the worktree under `git rev-parse --show-toplevel`, which
  Git for Windows prints in long names.
  So the tests now seed it the same way (`filepath.EvalSymlinks`, as `followup`'s tests
  do), and `samePath` stays strict, since claude finds a session by the exact folder it was
  started in. The whole suite was run with `TEMP` set to the short spelling of a
  runner-length folder: everything passes but the junction test #60 fixes. A longer
  folder makes the batch tests fail with git's "Filename too long" instead, which is a
  path-length limit, not a short-name bug.
- 2026-10-11: #45 reviews (PR #48): an independent read-only reviewer, and an adversarial security
  read in a scratch copy, which found nothing it could get out through (junction chains and
  loops, dangling junctions, 8.3 aliases, `\\?\` targets, a junction to `C:\Windows`; no
  hang). The reviewer's blocking point, fixed: a symbolic link whose target goes on past a
  junction (`s -> out\sub`, or to `C:\Documents and Settings\<user>`, a junction Windows
  ships) still got out, because `EvalSymlinks` follows the link inside its own walk, fails
  at the junction, and the fall-back to a shorter prefix kept `s` unresolved. `throughLink`
  now reads symbolic links too, with relative and rooted targets; this PC cannot make them,
  so its logic is tested on a fake file system and the real-link test skips here. Also
  fixed: what cannot be judged (an Lstat error other than "not there", a link Readlink
  cannot read, a reparse point the path goes on past that is not a link, a `C:x` target)
  is refused rather than judged by its name; and realPath for every file cost about ten
  times grep's search, so the walk judges only links and irregular entries, after the glob
  (a warm grep over `GOROOT/src`: 7 to 8 s, now 1.2 s). Moved to #47: on Linux and macOS a
  symbolic link to a file not there yet still lets `write_file` create it where it leads
  (on main too; the same walk on every platform fixes it, but changes Linux, so it is its
  own item). Refused now, allowed before: a volume mounted on a folder in the workspace
  (its target is a `\\?\Volume{...}\` path, never under a drive-letter root), a junction
  target spelt through another mapping (a `subst` drive, a mapped drive against its UNC
  path), and a junction on an SMB share (its target is the server's path, judged here).
  For the guard branch's merge: keep `inside` against `w.real`, and judge secret names
  relative to `w.real` too (relative to `Root`, a workspace reached through a junction
  sees its parents' names); its README sentence on junctions no longer holds. The
  reviewer's second pass found nothing blocking and three low points, fixed: a path that
  cannot be judged said "outside the workspace" (now "cannot tell where it leads"); a loop
  of links ran all 255 rounds, each with `EvalSymlinks` following up to 255 links of its
  own (now refused at the first path met again: a two-junction loop went from 157 ms to
  3 ms); and a mount point made by hand with an NT target (`\Device\...`) read as a folder
  on its own volume (refused now). Left, fail-closed on purpose: a workspace under a folder
  that can be passed through but not read (another account's profile with traverse-only
  rights, a share with access-based enumeration) refuses every path for the file tools,
  since a component that cannot be read could be a junction; `NewWorkspace` does not fail
  there, because it also checks the directory for the CLI backends, which do not use it.
- 2026-10-11: Junctions (#45, PR #48), found while reviewing the guard for #36. Since Go 1.23 a
  Windows mount point (a directory junction, which needs no privilege to make) is an irregular
  file to `os.Lstat`, not a symbolic link, so `filepath.EvalSymlinks` stopped resolving it:
  worse, it fails on a path that goes on past one, and the workspace check's fall-back to the
  longest prefix that resolves then judged `out/secret.txt` by its name. Tests with real
  junctions saw `read_file`, `write_file`, `edit_file` and `grep` reach outside. Now `realPath`
  reads each irregular component with `os.Readlink` on Windows (it answers only for links, so
  OneDrive's reparse points are passed over) and follows the target, up to 255 times; paths are
  compared with the root resolved the same way, so a workspace reached through a junction still
  works while `Root` keeps the spelling it was given for commands. Refusing every junction was
  not taken: pnpm fills `node_modules` with junctions that stay in the project, and `os.Root`,
  which would also close the race between the check and the open, refuses those too. `grep`'s
  walk judged no file it found on main, so a file symlink out of the workspace was searched on
  Linux too; it now judges each link it finds (the guard branch judges every file, and also
  passes over secret files). A walk still goes into no link or junction, as before: `grep` over
  the root, or with `path` set to a junction, does not search the files behind one; a path
  through it does. Windows CI runs only after a merge: the junction tests ran on a Windows
  development PC, the symlink tests on Linux.
- 2026-10-11: PR X done (#44, closes #36): one guard for every backend. Issue #36 asked for
  each CLI's own policy file; the PR puts the guard in what every backend inherits instead,
  so one mechanism covers all four CLIs and the api loop's run_command: tokens and keys
  withheld by name and pattern (git's askpass helpers too; claude's plan login reaches the
  claude CLI only), the logins tools keep on disk (gh, npm, kubectl, docker, aws, gcloud, az)
  looked for in an empty directory of the run's own, the ssh agent out of reach, and git
  pointing every push at `push-refused-by-hrn://`. Claude also gets `--disallowedTools`:
  the publishing commands in Bash and PowerShell, and a Read and an Edit rule per secret
  file. The api loop's guard gains `gh-publish`, `deploy`, `guard-env` and `secret-file`,
  and its file tools refuse secret files. One list of secret files (`tools.SecretNames`,
  `SecretDirs`) feeds both, and a test keeps the two command lists refusing the same
  commands. Codex, gemini and opencode get the environment only: each run's guard line
  says what that leaves open (secret files, wrangler's and cargo's logins), which is the
  issue's "say so on every run". Not done on purpose: clearing git's credential helper
  (private fetches need it; an agent set on undoing the push guard can, and the README says
  how), and withholding `DATABASE_URL` (local tests need it). Checked live with the claude
  CLI in `yolo` (two runs on a scratch repo, scratch `HRN_HOME`): every refused command and
  file was refused by the rules, shell writes to `.env` files included, and `git -C . push`
  failed at the dead end; a PowerShell `Set-Content` was refused by Claude Code's
  working-directory check instead, so that one is not shown to hold by the rules alone.
  Codex could not review or be checked live: its plan reached its usage limit until
  2026-10-16 (#43 is about carrying a run to the other CLI when that happens).
- 2026-10-11: PR X reviews, both by an independent read-only claude run through hrn (codex
  was at its limit). The first found four blocking gaps: secret files could be written
  (claude had Read rules only, the api file tools none), `.env.*`, `*.key` and `.dev.vars`
  were missing, codex, gemini and opencode runs did not say what was unguarded, and "git
  cannot push" claimed more than the mechanism holds. The second found run_command could
  still reach secret files (`echo x > .env`), and that guard-env refused any mention of a
  variable, `kubectl --kubeconfig` and a search of hrn's own source included; also
  `GCM_CREDENTIAL_STORE` withheld as a credential, a link to `.env` read as its own name,
  Windows' other names for a file (`.env.`, `::$DATA`, 8.3), `.netrc`, `.pypirc` and key
  stores missing, and `PushInsteadOf` in another case. All fixed, each as its own commit;
  grep's walk no longer follows a link out of the workspace, which it did before this PR.
  A third pass found nothing blocking; its points were taken too: guard-env missed `+=`,
  `${env:NAME} =`, the env: drive's other cmdlets and aliases (`rm env:X`) and
  `GIT_CONFIG_KEY_$i`; secret-file missed git's `REV:path`, braces and quoted paths with
  spaces; run_command could read the agent CLIs' plan logins, which only claude's rules
  listed (now `tools.SecretPaths`, one list for both); and `.wrangler` refused a worker's
  local state, when wrangler's login is `.wrangler/config`. Left as said in the README:
  `jq '.key'` is refused (the rule reads words), and a Windows directory junction is not
  seen through by the workspace boundary since Go 1.23 stopped resolving mount points; that
  one predates this PR and is a task of its own. A fourth, short pass over those commits
  found one regression of them: anchoring the env: cmdlets to a space dropped `{`, so
  `& {Remove-Item env:KUBECONFIG}` got through again. guard-env now refuses any use of the
  env: drive as a path on a guarded name, or with a wildcard, whatever the cmdlet, alias or
  scriptblock, since a list of cmdlets is never whole; `$env:X` and `${env:X}` stay
  readable. It also takes `export -n`, `declare +x`, `read` and `printf -v`. Decided:
  `.npmrc` is a secret name anywhere, a project's own included: one often carries an
  `_authToken` line, and refusing a pnpm setting costs the agent a turn.
  One FuzzCodec run failed once in a container while the Windows suite loaded the same
  machine; no input was saved, and three 10 s runs and a 2 min run (7.4M executions) after
  it passed.
- 2026-10-11: PR X merged with main (#46, #48, #50 to #53), then a security review of the whole
  PR as it would merge (Claude, read-only): 2 blocking. (1) The merge kept #48's grep walk,
  which judges only links by where they lead; a regular file was judged by its spelling under
  the search path, so `grep` with `path` set to `.codex`'s short 8.3 name (`CODEX~1`) printed
  `auth.json`: the secret paths of two parts never matched. Now each file is also judged at the
  search path's real place plus its relative name, which costs no call per file. A Windows test
  with a real short name (the test skips on a volume without them, E: here; it ran on C:). (2)
  gh keeps its login in the system's credential store and asks it when no token is set, so the
  empty `GH_CONFIG_DIR` did not keep `gh auth token` from printing it; checked on a development
  PC without printing it. Now `GH_TOKEN` and `GH_ENTERPRISE_TOKEN` are a token that is not one,
  and a `gh-token` rule (and claude's rules) refuse `gh auth token`, `gh auth git-credential`
  and `gh auth status --show-token`; `gh auth token --user NAME` still reads the store, so under
  codex, gemini and opencode it is open, and the README and each run say so. Also taken from its
  notes: an hrn started inside a guarded run (from the shell, or as the hrn MCP server the
  claude CLI loads from the user scope) could lift the guard with `allow_dangerous`, and the
  refusal message even named that lever. The guarded environment now sets `HRN_GUARDED`, the api
  loop sets it for its own MCP servers, `Validate` and `agent.Run` refuse `allow_dangerous`
  under it, and the message names no lever. The test packages that lift the guard clear the
  marker in `TestMain` (helper processes keep it), so the suite still passes when an agent under
  the guard runs it. Its other notes are #56. Live, codex in `yolo` under the guard on a scratch
  repo: fake `GH_TOKEN` and `NPM_TOKEN` empty, `git push` at the dead end, the bare remote
  empty.
- 2026-10-11: PR X, the review's second pass over those fixes: B1 and B2 closed, 1
  blocking. The marker reaches an hrn MCP server only if the agent's CLI passes its
  environment on to the servers it starts, which the reviewer believed codex does not
  (it starts them with a cleared environment); there the nested run would be wholly
  unguarded. Decided: the server itself refuses `allow_dangerous` on `run` and `batch`,
  and leaves it out of `run`'s schema, unless the operator started it with `hrn mcp
  --allow-dangerous-runs`; the host calling it is an agent, so a tool call cannot be the
  operator's decision. The marker stays as the second layer. Also: grep refuses a search
  path whose real place it can no longer tell, and the guarded-run refusal names no
  variable. Live, codex in `yolo` on 26bef1e's build: its commands see gh's stand-in token,
  `HRN_GUARDED=1` and the empty gh config dir, so under codex plain `gh auth token` gets the
  stand-in and only `--user NAME` reaches the store.

- 2026-10-11: PR W security review (a condition for merging: tested with all means including
  security), by an independent agent with loopback-only proof tests in a scratch copy. Nothing
  critical or high; four findings, three fixed here, each as its own commit. (1) Record text
  reached the terminal raw in hrn run, show and show -f, so a file an agent read could set the
  window title, write the clipboard (OSC 52) or forge a check mark: control characters now
  become U+FFFD. (2) On Windows a reader of a batch record blocked save's rename, so 64 readers
  on the network failed 14% of saves (a test with 32 saw 10 to 15 of 50): records are read with
  FILE_SHARE_DELETE and replaced with os.Root's Rename, which uses POSIX rename semantics; none
  fail now. (4) A key that reached a run (an agent running env) would show on the LAN page:
  watch blanks the values of its environment's secret-looking variables on every page and
  fragment; the run_command side belongs to #36. Also the footer's home path and read errors'
  paths are shown to this computer only. Left, low: (3) one client can make watch read a whole
  large events.jsonl per request and open many streams, each re-reading the lists every 2 s;
  real runs are under 1 MB today. A shared poller, a cap on streams per address and a bounded
  run page are the fix, with #38.
- 2026-10-11: PR W review (an independent read-only reviewer), nothing blocking, 5 findings
  fixed, each as its own commit. The reviewer wrote its own decoder and found the encoder
  exact at every length 0 to 213 under every mask; since jsQR forgives a few wrong
  modules, the same kind of decode now runs in CI. Fixed: an empty `--lan=` served every
  adapter (now refused); `--lan NAME` with a space got a misleading hint; the terminal
  code was inverted on a light theme (now forced white on black on a terminal); net/http
  answered `OPTIONS *` itself, before the door (now `DisableGeneralOptionsHandler`); and
  tests for the door by method, origin and source. Left for the bot (#38): every acting
  route must be registered as POST, since the door goes by method; the key must never
  appear in a GET response; and on plain HTTP it is a replayable bearer token, so an HMAC
  over method, path and time, or HTTPS, comes before it carries power.
- 2026-10-11: PR W done (#39, closes #33): hrn watch for a phone. Issue #33 asked for a key that
  other devices need even to view. Decided on the issue: a key for control only, viewing stays
  keyless. So the PR has (1) `--lan=NAME`, one adapter by name (any case) or address, since
  plain `--lan` on a PC with several virtual network adapters served every one of them; (2) a QR
  code of each phone link on the PC's own view of the index, and in the terminal when there is
  one network, from a new `internal/qr` (byte mode, level M, versions 1 to 10; tests pin the
  standard's HELLO WORLD codewords, format and version strings; jsQR 1.4.0 in a browser read
  back codes of versions 1, 2, 5, 8 and 10 and the served code itself); (3) on a narrow screen,
  runs and batches as cards, since the first page read poorly on a phone (the tables ran off the
  side and broke words); (4) the door: anything but GET and HEAD must send `Options.Key` in
  `X-Hrn-Watch-Key`, from loopback too, a custom header doubling as the CSRF check. No action
  exists and hrn watch sets no key, so all are refused until the bot brings one; no client side
  was written for an action that does not exist. Not done from #33: HTTPS (a key on plain HTTP
  can be sniffed on the Wi-Fi; worth it when the key matters) and a key per start for viewing
  (declined). A phone that came in over the network sees neither the codes nor the PC's hints,
  decided by the connection's address, not Host.
- 2026-10-10: PR V done (#34): a live view. After an `hrn batch` on the claude backend there was
  no way to see what was going on. `hrn watch` serves a page of the newest runs and batches, a
  timeline per run streamed over Server-Sent Events that tail `events.jsonl`, and a page per
  batch with each task's runs and gates; `hrn show -f` prints the same timeline in the terminal
  until the run saves its result. New package `internal/watch`, standard library only. Choices:
  fragments are rendered and escaped on the server by `html/template` and the script only places
  them, so the escaping is tested in Go; the SSE writer splits data on `\r`, `\n` and `\r\n`,
  because a lone `\r` in a tool result would otherwise end a data line and forge a field; each
  message's id is the byte offset after its line plus that event's time, so a reconnecting
  browser resumes exactly and the wait before the next step stays right; the file is opened for
  each read, since an open handle on Windows would stop `runs prune` and `batch rm`. A run
  killed before it saved its result never ends in either view (it cannot be told from a long
  step). The guard (loopback Host, Origin, Sec-Fetch-Site, CSP, no-store) is a copy of `hrn
  sessions'` in `internal/sessionweb`, which is not on main yet; once both are, they can share
  it. Deviations: (1) the batch record was saved at the start and the end only, so mid-batch
  nothing tied a task to its run or showed a finished gate. It is now saved when an attempt's
  run starts and ends, after each gate and when a task ends (best effort; the end's save still
  reports errors), and `save` retries its rename for up to 200 ms, since a reader holding the
  file open makes it fail on Windows (Go opens files without `FILE_SHARE_DELETE`). The format is
  unchanged; an attempt still running has an empty result status. (2) The brief said loopback
  only; a phone on the same network was then asked for, and the choice (made twice, since it
  widens who can read the runs) was `--lan` now without a login and the key later (#33). `--lan`
  binds each private IPv4 address of the adapters that are up, never 0.0.0.0, and the Host check
  admits exactly those `ip:port` strings. (3) A cost column on the index, since the question was
  whether the harness has the cost: it is the backend's own figure, so codex and gemini show
  none.
- 2026-10-10: PR V review (an independent read-only reviewer), 11 findings, 8 fixed, each
  as its own commit. The ones that mattered: a page loaded while `Close` wrote
  `result.json` showed an empty "Finished" and a stream URL with no id, since
  `runlog.Load` marks a run done before the file parses (the follower now reads
  `task.json` then); `--lan` took VPN tunnels' 10.x addresses (point-to-point adapters
  are left out, and every network served is listed, since a VPN that looks like an
  ordinary adapter cannot be told apart); ids from a request were only checked for
  separators, so `CON` reached the disk, a device on Windows 10 (they must now look like
  `YYYYMMDD-HHMMSS-xxxxxx`); and the traversal tests passed on a missing file instead of
  the check. Also fixed: a portless Host under `--lan` on port 80, a blank batch page for
  a read caught mid-rename, a test calling `t` from a goroutine, stale text. Left: the
  rename retry has no test (it needs an injectable rename); on Windows `runs prune` can
  meet a batch record mid-save and refuse once, which running it again settles; and
  AGENTS.md (#31) needs an `internal/watch/` row once both are merged.
- 2026-09-27: PR T: continuing claude and codex runs, and moving one conversation between
  backends (no PR yet; branch `t-cli-sessions`). The CLI adapters record the session id
  their stream reports (`Result.Session`: claude's `session_id`, codex's `thread.started`
  `thread_id`), and `--continue` on those backends forks it: `--resume <id>
  --fork-session`, `codex exec … fork <id> -`. Forked rather than resumed in place so a
  run's session never changes after the run, and continuing one run twice branches the
  way api runs already did; a resume appends to the same codex thread, which was checked
  by hand. `--continue` onto another backend walks the chain (`runlog.Chain`), carries on
  from the latest run on the target backend that saved something, and hands the turns
  after it over as a transcript of requests and final answers (`internal/followup`, one
  plan the CLI and the MCP run tool share instead of two copies of the old api-only
  check). The record keeps the typed prompt, so transcripts never nest; the transcript
  goes to `handoff.md`. A model is inherited only from runs on the same backend. Same
  backend with nothing saved is refused, not started fresh, as the durable-workers spec
  asks of native continuation. Deviation: codex 0.155 refuses `--full-auto`, so `auto`
  maps to `--sandbox workspace-write`, which is what that flag stood for under `exec`;
  and codex is found in the Codex desktop app's build directories, picked by mtime since
  they are named by hash. Flags were checked against installed `--help` (claude 2.1.278,
  codex-cli 0.155.0-alpha.9.2) and that exec's `-C` and `--sandbox` apply when they come
  before `fork`/`resume` (a workspace-write resume was recorded as such in the rollout).
  Verified live with the built binary: claude reads a file, codex continued from it
  answers from the transcript alone, claude continued from codex forks its first session
  and quotes codex's reply, codex continued from itself forks its thread; every run
  recorded a distinct session id. Not done: gemini and opencode sessions, and the
  durable worker integration the spec's native-session milestone asks for.
- 2026-09-27: PR T self-review, 1 finding, fixed. The MCP `run` tool never inherited the
  earlier run's mode on `continue_run_id`, although its description said it did: the
  readonly default was filled in before the continuation was looked at, so a follow-up to
  an `auto` run could not edit. It inherits now unless the call names a mode, as `hrn run`
  always has, and the test covers both.
- 2026-09-28: PR T second review (#26), 5 findings, 4 fixed and 1 skipped. The one that
  mattered: the handoff pasted earlier answers into the prompt verbatim, framed by "=== now"
  and "--- request" markers the pasted text could itself contain, so an answer quoting a
  README could open a fake "now" section and pass for the request, prompt injection between
  agents. Every quoted line now starts with "> ", and the transcript says it is a record,
  not instructions; a test with an answer shaped like the markers proves they cannot be
  forged. Also: an ancestor run that exists but cannot be read now stops the chain with an
  error instead of being treated as gone and silently shortening the handoff; a missing
  old workspace says that -C (or dir over MCP) continues elsewhere; and task.json records
  the session a run was forked from. Skipped: codex found on PATH still wins over the
  desktop app's build, as every backend prefers the user's PATH; hrn backends names it.
- 2026-10-10: the SSE parser's scanner buffer starts small. `FuzzParseSSE` failed CI on #34
  with "context deadline exceeded" while its rate fell to 0 execs/s, with no crasher and a
  package that PR did not touch; a rerun passed. The likely cause, not reproduced: the lab
  runner's container sees the node's 24 CPUs within 3 GiB, the fuzzer starts a worker per
  CPU, and `parseSSE` gave every response a 1 MiB buffer up front, on the order of 25 GB/s
  of garbage. #32 caps the fuzz steps at `-parallel=4`; this change removes the per-parse
  megabyte. The buffer starts at the scanner's 4 KiB and doubles as a line needs, up to
  the same 64 MiB: a 598-byte stream went from about 1 MiB (1,055,404 bytes) to 10,704
  bytes per parse, and locally the fuzzer ran 1,094,491 inputs in 15 s where it ran
  308,242. A test holds the cost of a small parse under 128 KiB and another parses a 3 MiB
  line whole. This entry is not at the top of the log on purpose: #34 adds its own there,
  and two insertions at one place would conflict.
- 2026-09-27: PR S done: the api backend's price table. Prices come from Anthropic's
  published first-party list (dated 2026-06-24 in the reference used). Three changes. The
  newest models are in it: Opus 5.5 ($4/$20), Mythos 5.1, and the Opus 4.6/4.7 and
  Sonnet 4.6 generation. Cache reads are per model where they are not 0.1x input: Fable
  5.1 at $0.25 and Opus 5.5 at $0.20, where the old flat 0.1x put Fable 5.1's at $1.00,
  four times too high. A dated snapshot (`-20251001`, or Vertex's `@20251001`) is priced
  as its model, so `claude-haiku-4-5-20251001` no longer reads as unpriced. And a model
  with no price now says so at the start of every run, not only when `--max-cost` is set,
  because a cost of zero otherwise looks like free.
- 2026-09-27: PR S self-review, 1 finding, fixed: the cost was priced at the requested
  model for the whole run, but with server-side fallbacks on (Opus 5 and Fable 5.1) a
  refused turn can be served by another model at another price, and that estimate is
  also what `--max-cost` checks. Each turn is now priced at the model its response names,
  falling back to the requested model when the table has no price for it. A three-turn
  fake run served by Opus, Sonnet and an unlisted model fails in the old way when the fix
  is reverted. How the API bills the refused part of such a turn is not modelled.
- 2026-09-23: PR R done: getting the api backend ready for its first real run, since no key
  exists here. (1) The server's context-management report is recorded verbatim as an info
  event per turn. Its location in the stream is not documented here either, so the parser
  reads it from the message, from the top level of message_delta and from inside its delta,
  and keeps any value that is not null or empty. The history stays append-only, and a test
  asserts the report never goes back up. (2) `scripts/api-check.sh` is the first real run
  as one command: a read-only haiku run, a `--continue` of it, then one long sonnet task
  with context editing on and off under a cost cap, each with its summary line and the
  count of server reports. It refuses to start without credentials. It was dry-run
  end to end against a stand-in API (ANTHROPIC_BASE_URL at a 30-line fake server), and
  that dry run found two bugs in the script itself: step 3 printed the run id line
  instead of the summary, and "the newest run" was chosen by directory name, which picks
  the wrong one when two runs start in the same second. The run id now comes from hrn's
  own output. CI checks that both shell scripts parse.
- 2026-09-23: PR R self-review, 1 finding, fixed: step 3 expanded a possibly empty array
  under `set -u`, which bash before 4.4 (the one macOS ships) treats as unbound, so the
  "on" run would have stopped the script on a Mac. It is a plain string with
  `${flag:+"$flag"}` now. Checked and sound: no report leaks across retries or turns, the
  run id extraction matches what hrn run prints, and `--continue` inherits model and mode.
- 2026-09-23: PR P done: `.github/workflows/release.yml` and `.github/release.sh`. A `v*`
  tag runs vet and the tests, builds static binaries for linux, darwin and windows on
  amd64 and arm64 (`CGO_ENABLED=0 -trimpath -s -w`) with SHA256SUMS, and publishes them
  with `gh release create --generate-notes --verify-tag`. Plain binaries rather than
  archives, so the script needs nothing beyond Go and coreutils and runs the same way
  locally. The tag must equal agent.Version or the script stops, so a release and
  `hrn version` cannot disagree. No ldflags for the commit: go build stamps it from the
  checkout and #19's `hrn version` reads it from the build info. Verified by a local dry
  run: a mismatched tag was refused, all six targets built, the windows binary ran and
  the linux arm64 one is a static aarch64 ELF. The workflow itself has not run, because
  that needs a tag push, which publishes a release. Self-review, 1 finding, fixed: the
  script had never run on Linux, where the release job runs, so CI now dry-runs it on the
  Ubuntu runner on every push (about a minute); with no tag it builds the declared
  version. The first attempt at that step lost its sed backreference on the way into the
  file and would have passed an empty version, which is why the script reads the version
  itself now.
- 2026-09-23: PR Q: `hrn eval diff` and `hrn eval list` (open item 8; lettered Q because
  another session used N and P meanwhile). `eval.Compare` matches variants by name and
  tasks by (task, variant), keeps one-sided rows with a nil side instead of dropping them,
  and flags a task id whose prompt differs, since that is usually the change being
  measured but its rows no longer compare like with like. Verified on real records: the
  greeting eval from 2026-09-22 (sonnet 0%, haiku 100%) re-run with the target line fenced
  in backticks went to 100% for both; the diff shows it and names the changed prompt. The
  first prompt's "the line: hx rocks. Do not run git." had put a sentence's full stop right
  after the target text. Found on the way: `eval.List` sorted by id as if ids were
  timestamps, but eval ids are random hex, so "newest first" and `-n` were arbitrary; it
  sorts by start time now, and the list test fails the old way when that is reverted. Also
  `hrn version` named the commit twice on a checkout build, because Go 1.24+ already puts
  it in the pseudo-version (and `+dirty` for edits); it says each once now.
- 2026-09-23: PR Q self-review, 6 findings, 5 fixed and 1 wrong. The real one: variants
  were matched by name alone, so a variant renamed onto another model, a gate made easier
  or a changed rubric moved the numbers with nothing said; the diff now notes each, as it
  already did a changed prompt. Also: `hrn eval list` says how many records it could not
  read (prune refuses over those same records); `eval.List` is gone in favour of ListAll,
  whose comment says why it reads every record; the totals moved to `Record.Totals`; and
  the path-like id test now reaches the id check. The wrong finding was mine: I thought the
  arrows misaligned the columns because `%-16s` pads by bytes, but Go pads by code points;
  the alignment test written to prove it passed, and stays as a guard.
- 2026-09-23: PR N done: `batch`, `batch_show` and `batch_merge` in `hrn mcp`, so a host
  can plan, fan out, read diffs and merge in one conversation. The spec is the batch-file
  JSON with two differences: a relative `dir` resolves against the server's workspace, and
  unknown fields are refused, because a model-written `gates` would otherwise run with no
  gate. `batch rm` is left out on purpose: it deletes branches, and the host can run the
  CLI for that. Verified through a real Claude Code session (`claude -p` with only the
  two tools allowed): two haiku tasks passed their gate and both branches merged in task
  order. Found by that run: Claude Code showed the model the structured content instead
  of the text, so the whole batch record (worktree paths, echoed prompts, token counts)
  went to the model and the "what next" text did not. The structured result is now a
  brief with the same facts and a `next` field, and a test fails if the record leaks back.
- 2026-09-23: PR N self-review, no findings. Checked: unknown-field refusal reaches through
  the embedded options into every task; batch ids with a path separator are refused on
  both show and merge; progress arrives one event at a time; the brief is right for pass,
  fail, error and canceled tasks. A spec's `dir` is not confined, the same as `run`'s: the
  server trusts its host with the machine's paths.
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
- 2026-09-23: housekeeping PR (PR O in the quality-layer plan): `hrn runs prune`,
  per-task `gate_timeout`, and `hrn version` with the commit. Prune decides from the top
  down so it never breaks a reference: a kept eval keeps its batch, a kept batch keeps its
  runs, and an old batch whose worktrees are still on disk is kept with its runs because
  `hrn batch rm` needs the record (on this machine that was five of the old batches). It
  refuses to run without `--older-than`, and a directory under runs/ that is not a run id
  is never touched. Each package owns its own file layout (runlog.Prune,
  batch.RemoveRecord, eval.RemoveRecord); only cmd/hrn knows how records cite each other,
  so runlog still imports neither batch nor eval. gate_timeout travels on attemptPlan
  rather than through runAttempt's signature, because PR M (#18, another session) changes
  that signature at the same time. Found while verifying: Go does not stamp the commit
  into a binary built inside a `git worktree` (`.git` is a file there), so `hrn version`
  shows the commit only for builds from the main checkout; the README says so. After
  merging PR M, a kept eval also keeps the runs its judge made (Record.Judgements[].RunID).
- 2026-09-23: housekeeping PR (#19) self-review, 7 findings, all fixed. The serious three
  were all "prune breaks a reference after all": an unreadable batch or eval record was
  skipped, so the runs it cited lost their protection (prune now refuses while any record
  cannot be read); runs were removed before the records citing them, so a failure partway
  left records pointing at nothing (now records first, runs last); and a run started with
  --continue did not keep the run it continued (now the whole chain stays). Also: a run
  with no result that wrote within the last hour is left alone, since it is probably still
  running and removing it pulls the directory from under its writer; the gate-timeout test
  asserts the killed gate's own duration instead of the whole batch's wall clock, which
  would have flaked on the Windows runner; the byte formatter moved to report.Bytes beside
  the other formatters; and the prune usage line fits the table.
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
  interactively (`/login`); hx finds it without PATH changes. Three smaller smokes passed first
  on the claude backend: `hx run` on this repo; Claude Code -> `hx mcp` run tool -> claude (hx
  registered with `claude mcp add --scope local`, `claude mcp list` shows it Connected); `hx
  batch` in a throwaway repo. Then the prescribed batch in a test checkout, with `model: sonnet`
  instead of the default opus to keep cost down: 3/3 passed in 1m23s at about USD 1.03 list
  price, 26-60 s per task. What the agents did: `api-vet` found nothing to fix (pass, no commit,
  3 turns); `small-test` added a 33-line table-driven test for `isRoleTitleLine` (14 turns,
  green); `readme-typos` changed two British spellings to American (Behaviour, re-emphasises) -
  not typos, and the build gate cannot see that. Lessons: (1) gates verify the build, not the
  intent: PR G should add a per-task `expect` regex on the final answer, and the judge stays
  necessary for diffs; (2) a pass with no commit is a distinct outcome eval should count
  separately; (3) the claude backend spent eight consecutive Grep calls on one file, so
  tool-call count is worth a column next to turns; (4) a 12-turn run read about 1M cached
  tokens, so cost is dominated by Claude Code re-reading its context and per-task `max_cost_usd`
  (PR C) matters more than `max_turns`. The branches hx/readme-typos, hx/api-vet and
  hx/small-test are left in the test checkout to inspect or merge; `hx batch rm
  20260921-100459-f3b353` removes them. PR order unchanged.
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
- 2026-09-21: PR F done (#7). Built in a second git worktree (branch `f-report`) because another
  session was on PR C in the main checkout at the same time; two agents in one working tree
  would overwrite each other. Choices the spec left open: batch rows group by repository (the
  spec named the columns, not the rows); a group with no token counts reports cache share -1 in
  JSON and `-` in text; `--by` defaults to backend; unfinished runs are counted and listed under
  the tables, not in them; a cost is marked `~` when any run in the group carries a non-zero
  estimate. `batch.List` was added for the batch table. The smoke against the real `~/.hx`
  showed long repository paths cut from the end, which hid the part that differs, so keys are
  shortened from the left.
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
- 2026-09-22: PR I done (#11). Built in a second git worktree (branch `i-guard`) because another
  session had the main checkout on PR E. Deviation: the refusal text does not say "rerun with
  --allow-dangerous". That sentence is read by the model, which cannot rerun anything and does
  have a shell, so it invited a retry or a recursive hx call; it now names the rule, says only
  the operator can lift it, and tells the model to explain rather than retry. Design note the
  spec left open: the guard needs per-run state, but `APIBackend.Tools` is built once in
  `NewAPIBackend` and shared by every run, so the switch rides on `tools.Workspace` (already the
  per-run policy object for path confinement) rather than in a tool closure. `pipe-to-shell`
  also matches `powershell` as written in the spec. The dirty check lists up to five paths so
  the message is actionable, and runs before any worktree is created. Two self-inflicted lessons
  for scripted edits, both caught only by merging main: an invalid `open(..., newline=)`
  truncated this file before raising, and `subprocess.run(text=True)` decoded `git show` with
  the console codepage and mangled every arrow. Read bytes, decode UTF-8, and assert the file
  did not shrink.
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
- 2026-10-11: PR #50, reasoning effort for the CLIs. One list of levels
  (`agent.EffortLevels`), exact names (`HIGH` is refused), checked on the way in only: a
  spec with another level now fails to load, while stored records still load and a
  follow-up does not inherit effort. Codex takes it as `-c model_reasoning_effort=<level>`
  (it has no dedicated flag), claude as `--effort`; gemini and opencode report it as not
  enforced through the `mapsEffort` adapter capability, so a new adapter warns by default.
  The review (Claude Opus) found the eval judge skipped the check, which would have failed
  only after every cell ran; fixed. The CLIs accept every level but do not report the
  effective one back.
- 2026-10-11: PR #51, run records surface write failures. Every complete event is still
  attempted, one write each; after a write cut short mid-line the next event starts on a
  new line, so one failure costs one event and the count stays exact. `record_error` is
  additive and also carries a failure to close events.jsonl; `runlog.Events` skips broken
  lines only in a record that says it is incomplete. The MCP run tool reports a record
  failure without flagging an ok run as an error, because a host may retry a tool error and
  spend the plan again; a result.json that could not be saved is still an error, since the
  run then never reads as finished. The eval judge still ignores Close's error. Review (Claude
  Opus) found the short-write corruption; fixed. Left as is: if a broken line and a failed
  result.json happen in one run, no record_error is saved and hrn show refuses the run; that
  means the disk is failing.
- 2026-10-11: PR #52, contained process trees. Windows starts the process suspended, joins it to
  a kill-on-close job, then resumes every thread; the job does not allow breakaway, so a
  descendant using CREATE_BREAKAWAY_FROM_JOB fails (strict containment over compatibility, a
  maintainer's to revisit). A CLI's leftovers end with the run on Windows even after a normal
  exit; on Unix only on cancel or timeout. Tree.Kill makes "canceled" mean killed even when Go
  never called Cancel. On Linux WaitGone counts live members from /proc so an unreaped zombie is
  not a survivor, skips entries it may not read, and falls back to the kill(-pgid, 0) probe when
  the scan fails; hidepid=2 can hide a member owned by another user. hrn stops on SIGTERM and
  SIGHUP as well. Reviews (Claude Opus, twice) asked for the zombie handling, the explicit kill,
  the signals and the /proc fallback; all done. The live test cannot separate this from Codex's
  own sandbox cleanup; the fake-CLI tests show the change.
- 2026-10-11: PR #53, hrn exec --protocol v1. The document came first and the code follows
  it. The capability report is conservative: a mode is listed only where the adapter enforces
  it (gemini auto and yolo, opencode yolo only), and a start that names an option the backend
  does not honour is rejected, presence counting even as zero. Field names must match exactly
  and once, since encoding/json matches case-insensitively and merges duplicates. resolved_model
  needs the additive model_reported, set where a backend reports its model (api, claude,
  gemini). After start, a bad line or a wrong cancel is a non-fatal error and the run goes on;
  end of stdin cancels, because the supervisor is gone. Review (Claude Opus) found the case
  and oversized-line holes; fixed. An api-backend run through exec has not been tried (it costs
  money).
- 2026-10-11: carrying on at a usage limit (#43). Cause: eight codex attempts in local batches
  that night ended at codex's usage limit, and their records said only `exit status 1`, because
  codex's `turn.failed` keeps its text under `error.message` and its empty `message` overwrote
  the limit message; now read from there and never overwriting. The same records show the limit
  was rarely the whole story: in every one of those tasks the first attempt, on claude, had
  already failed a gate (a screen test that needs a live desktop, a base with long dashes, gates
  run in WSL's bash), so the switch saves the retry, not the task. Decisions: only claude and
  codex stand in for each other, never api (billed per token) or gemini/opencode; a limited run
  is carried on once, since a partner that is limited too would only hand it back; the reset
  time is believed only up to 8 days ahead and from a minute in the past; a time without a date
  is the next one; a zone the CLI names is loaded when Go has the zone database and is the
  computer's own otherwise (Windows ships none to Go, and the CLI names the computer's zone
  anyway). In a batch the carry-on is an extra attempt in the same worktree that does not use up
  a `retry` entry, its record `continues` the limited run, and the model named for the other
  backend is dropped. Two processes noting a limit at once can lose one note, which costs one
  more limited start, so the file has no lock. An eval sets `no_switch`, for the reason retry is
  refused there. `hrn report` counts `limited` as not ok, which it is.
- 2026-10-11: #43 review (codex, read-only), 6 findings, 5 fixed. A carry-on that ended
  limited too was not remembered. Notes from a batch's parallel workers failed on Windows
  ("Access is denied": a rename cannot replace a file another reader holds) and could
  overwrite each other: a lock in the process and a short retry across processes, with a
  test that runs two processes. "You've hit your rate limit" read as a plan limit. The fake
  claude's fixed reset time failed its test ten minutes a night. A null limits.json
  panicked the next note. Not changed: switching back to a backend earlier in the chain
  runs the model the chain chose for it, as `--continue -b` does; the reviewer wanted it
  cleared. The README says so.
- 2026-10-11: #46 security review (Claude, read-only), 1 blocking. The switch could start
  a partner that bills per token: claude prefers `ANTHROPIC_API_KEY` (and the other API and
  cloud variables) to the plan, codex `CODEX_API_KEY`, and nobody chose that backend. Now
  such a partner does not stand in and the run names the variable, never its value; a CLI
  logged in with a key in its own files cannot be seen, so the README says to use
  `--no-switch` there. Also from it: a remembered time more than eight days ahead is
  ignored, as one from a CLI is; a batch says why it does not carry an attempt on; the MCP
  `run` tool says a run can move to the other vendor. Left as notes in the README: the same
  mode is not the same permissions on both, and a carried-on run's timeout starts again. A
  limit read from stderr alone is still believed (bounded by the eight days). Merging main
  (#50 to #53) needed: the carry-on's record error is the one the MCP answer reports, the
  effort test accepts `limited` from codex, and exec's protocol lists the statuses and that
  exec never switches.
- 2026-10-11: WSL's launcher spelt in lower case (#72, PR #87). `isWSLLauncher` compared the
  folder name to `System32` exactly, but `exec.LookPath` returns it as PATH spells it
  (`C:\WINDOWS\system32\bash.exe` on a development PC), so gates and `run_command` ran in WSL,
  and a batch's gates all timed out starting it. The folder name is now compared in any case, by
  a helper that reads both separators so its test runs on the Linux runner. Left for their own
  items: other WSL launchers, such as the WindowsApps alias and SysWOW64 (#86), and a gate whose
  shell cannot start being retried as a code failure, with WSL's UTF-16 output shown undecoded
  (#84).
- 2026-10-12: A null content block (#77, PR #99). `json.Unmarshal` decodes
  `"content_block":null` into a nil map without an error, and the block's first text,
  thinking, signature or citations delta (an input delta: its stop) then wrote into it and
  panicked the api loop. `content_block_start` now refuses it with an
  error, like the other malformed events; a unit test covers each delta kind and
  FuzzParseSSE has the stream as a seed.
- 2026-10-12: Issue-only fixes name their PR (#98, PR #101). AGENTS.md asked a PR to update
  PLAN.md for the item it delivers, and a fix for an issue with no item got a Decisions-log
  entry that sometimes named only the issue. Such an entry now names the issue and the PR,
  as the #72 and #77 entries do.
- 2026-10-12: Last-turn context and continuation warnings (#97, PR #105).
  `context_tokens` is additive and omitted when unknown. The api loop uses its last
  message's input plus cache reads and writes, claude its last assistant message,
  and opencode its last step; a claude sub-agent's lines are ignored. Codex's agent-turn totals and gemini's run totals
  cannot identify the last model call, so neither sets it. The run page shows the
  reported size. Above 150,000 tokens, one shared threshold, the CLI warns on stderr
  before starting and MCP includes the warning in its answer; neither refuses the
  continuation. Old records stay silent. For #37: context size can inform future
  budget warnings; this change adds no budget enforcement or flags.
- 2026-10-12: continue: warn on the session carried, not the run named (#103, PR #109)
  The warning uses the run whose conversation is resumed or session is forked, including
  an earlier run on the same backend; transcript-only hand-overs give no warning.
- 2026-10-13: Usage per turn (#235). A `usage` event in `events.jsonl` (additive kind, its
  figures under `usage`, zero fields omitted) records each model call: the api loop per
  request, claude per main-loop assistant message, opencode per step. Claude's stream-json
  repeats a message's usage on the line of each content block, so the event goes out once per
  message id with the first line's figures (its output count can be short of the final one;
  the context is the same on every line). Sub-agent messages are left out, as for
  `context_tokens`. Codex and gemini report agent-turn or run totals only and write none.
  Claude Code's `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger",
  "pre_tokens"}}` becomes a `compaction` event; that shape is the Agent SDK's documented
  message and the fixtures are written from it, not captured from a real run, so the first
  long claude run with `compact_window` should confirm it. A drop of more than half from the
  turn before is shown as a probable compaction by `hrn watch` and `hrn show --usage` only,
  never recorded. The watch chart is server-rendered inline SVG (two panels on one turn axis,
  no dual scale), redrawn over every turn on each new one through a `usage` SSE message.
  `hrn report` token totals come from results (sub-agents included); median and peak context
  per turn come from usage events, or, without them, from one per-run average
  (totals over turns), and the row says `turns`, `totals` or `mixed`. `--by day` lists days
  in date order.
- 2026-10-14: PR #313 (closes #151): `hrn report` sums costs through `KnownCost`, so a run
  or a batch task whose cost is unknown (codex usage marked missing, or an old record with
  no usage and a zero cost) adds nothing, is left out of the median and is counted in
  `runs_without_cost` or `tasks_without_cost` (additive, omitted at zero). The table shows
  `-` when nothing in the row is known and `$1.23 (2 unknown)` when some are; a reported
  zero is known and shows `$0.00`. A task that never ran (no attempts) is not counted as
  unknown: nothing about its cost is missing. `hrn eval` still sums raw (#309).
- 2026-10-14: A declared purpose on run and batch (#312, PR #331). Callers may
  declare a category with `--purpose` or `purpose`, otherwise a valid leading
  `Purpose: <category>` header supplies it, and the record stores it without changing
  the prompt. Watch and reports prefer the declaration over keyword rules; a header
  with free text or `unknown` is ignored as a declaration, while an invalid explicit
  value is refused before a run starts.
- 2026-10-14: PR #314 (closes #304): secrets masked on display. `internal/mask.Text` applies the
  issue's ordered rules (URL credentials, `-u user:password`, JSON secret fields,
  authorization-style headers, bare Bearer tokens, known token shapes, `NAME=value` where a
  whole segment of the name is KEY, TOKEN, SECRET, PASSWORD, AUTH, CREDENTIALS, COOKIE or
  SESSION, and `--password`-style flags), replacing each value with `••••`. It runs on hrn
  watch's rows, pages and streams (local and `--lan`, beside the env-value replacer), `hrn
  show`, the renderer's progress lines and Ada's sentences; records and `--json` output are
  unchanged. Deviation from the issue: the mask does not cut text at 1 KiB. The first cut
  broke tool inputs over 1 KiB (the summary and pretty body need the whole JSON) and lost
  everything past 1 KiB of prompts, answers and results, so the mask scans the whole text
  (Go's regexp is linear; a page shows at most 32 KiB) and the existing excerpt cuts run
  after it, which keeps the rule that a cut never leaves half a token visible. The names
  pattern matches whole segments so `--max-tokens=400` and `authors=x` stay. Left out, as
  #311: `hrn runs`, streamed deltas, URL user names.
- 2026-10-14: PR #317 (closes #298): kept scripts. `docs/scripts.md` has the header (name,
  summary, effects, parameters, an example that is the script's test, a hash line) and the eight
  rules; `scripts/verify.sh` runs every example, stamps the hash and regenerates the index
  between markers in AGENTS.md, and `--check` (CI, ubuntu only, and `/check`) fails on a failing
  example, a stale hash or a differing index. First kept scripts: `check.sh` (`--list`),
  `test-summary.sh` and `wait-for.sh`, both over `answers.go` through `go run`; `api-check.sh`
  is listed as not verified (it spends money). Sized before #295's numbers exist: this order was
  asked for on 2026-10-14. Hardening and extra tests from the review are #318; AGENTS.md is 119
  lines for now, with the rules also in the doc.
- 2026-10-14: PR #321 (closes #264): a codex cache read is counted once. The report's
  context average for a run without usage events and hrn watch's purposes card both go
  through `ModelTokens` (context is its total less output; the card sums input, output,
  cache reads and writes once). A record the helper cannot place that carries no usage
  metadata keeps the plain sum of its counters on the card, as in the report, so opencode
  and gemini rows do not shrink silently; gemini's and opencode's semantics in the helper
  itself are #320.
- 2026-10-14: PR #329 (closes #297): routing by evidence. `$HRN_HOME/route.json` maps a purpose
  to backend, model and effort with the eval each row came from; `hrn route show|set|from-eval`;
  `Options.Route` fills the three only when a run names none of them (`hrn run`, a batch task,
  the MCP run tool) and records `routed_by`; `hrn report --by routed`. An eval cell's backend is
  pinned so an eval never measures the table it feeds. Deviation: the first table from a
  hrn-evals run is not in the PR (the run spends plan quota; a maintainer's yes first), so the
  table starts empty and nothing is routed until it is filled. Left open as #327: a routed model
  following a retry onto another backend, `routed_by` after a limit switch, eval naming shared
  with `FromEval`, uneven row validation.
- 2026-10-14: PR #330 (closes #295): the measurement before any saving is claimed.
  `scripts/tool-shapes.sh` (a kept script over `scripts/tool-shapes/`) reads the run records
  of a window and prints bytes per result shape, two estimated shares of input tokens
  (once-counted, and replayed by every later call of the run), and inline-script groups by a
  normalised form, never the text; `docs/tool-shapes.md` holds the numbers for this machine.
  Over 30 days: 87 MB of tool results against 1.35 billion input tokens, so output shaping
  (#296) can touch about 1.6% once-counted and about 10% replayed, three quarters of it
  terminal output; inline scripts 0.4% replayed and the groups that repeat 0.001%, so kept
  scripts (#298) are justified by reuse, not tokens. Shares use only runs with a token figure
  (usage events, else result totals; codex input already holds its cached tokens). Left open
  as #328: interpreter groups in the table, the verifier and subdirectories, cache writes in
  the denominator, the shape of `go test -json`.
- 2026-10-14: PR #335 (closes #296): large tool results shaped. `internal/shape` summarises
  terminal, log and test output (the shapes #295's page ranks first; JSON and tabular are 0.3%
  of the bytes and wait for their own issue), and a summary replaces the cut only when its
  invariants hold against the original (numbers, negations, error strings, paths, every failing
  test with its own diagnostics in order), else the head-and-tail cut as before. The original is
  kept whole under the run's `originals/` behind a hash handle that `read_original` reads back,
  read-only. All of it is behind `--shape-tools` (`shape_tools` on a run, batch task, MCP run or
  eval variant), off by default: the issue says on by default only once the hrn-evals eval shows
  it pays, and that run spends plan quota, so it waits for a maintainer's yes, with the suite as
  a slice of its own in hrn-evals. With the flag off the tool schema and every byte of behaviour
  are as before.
- 2026-10-15: hrn update (#377, PR #389). `hrn update` replaces every hrn copy on this
  computer (the running one, PATH, Go's bin, `%LOCALAPPDATA%\hrn`) with the newest release
  (checksum verified), a `go install` of `--ref` or a `--file`, renaming each aside as
  `hrn-<build>-<stamp>.prev` first because Windows allows that and not an overwrite, and
  restarts a running `hrn watch` with its arguments while `hrn mcp` servers are only listed.
  The process list is read with the standard library alone: Toolhelp and the PEB on Windows,
  /proc on Linux, ps on macOS; `--rollback` swaps the newest backup back.
