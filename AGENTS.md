# hrn, a small agent harness

One Go binary, `hrn`, runs tasks via the Anthropic API (`-b api`) or installed CLIs (claude, codex,
gemini, opencode, agy) and saves runs under `~/.hrn` or `$HRN_HOME`. README indexes the guides;
`docs/commands.md`, `docs/permissions.md` and `docs/run-records.md` cover commands, guards and
records. Read `PLAN.md` before taking an item; don't reopen its decisions. Older entries use `hx`
(`cmd/hx`, `$HX_HOME`); code uses `hrn`.

## Layout

| Path | Holds |
|---|---|
| `cmd/hrn/` | The CLI and terminal renderer; batch, eval, report, prune and gc subcommands. |
| `internal/agent/` | `Backend`, `api.go`, `messages.go`, CLI adapters, instructions, prices. |
| `internal/tools/` | API tools, `guard.go`, `guardenv.go`, `secretfiles.go`, MCP bridge. |
| `internal/mcp/`, `internal/hrnmcp/` | JSON-RPC stdio and `hrn mcp` host tools. |
| `internal/batch/`, `eval/`, `report/`, `purpose/` | Worktrees, variants, tables, prompt labels. |
| `internal/runlog/`, `followup/` | Run records, pruning, continuation. |
| `internal/flow/`, `owner/`, `flowview/` | Execution events and step trees, local ownership, the shared flow view. |
| `internal/gc/`, `internal/ws/` | Merged cleanup; O_EXCL-leased slots (`.hrn/slots/<n>`). |
| `server/` | Separate module with its own `go.mod`; PostgreSQL control service and project worker. PostgreSQL tests need `HRN_TEST_DATABASE_URL`. |
| `scripts/api-check.sh` | The first real api runs. Costs real money; a maintainer runs it. |
| `.github/release.sh` | Builds the six release binaries and `SHA256SUMS` into `dist/`. |

## Build and test

Go 1.26.x (`go.mod`), nothing else. Before a PR, `bash scripts/check.sh` runs every CI check in
order from `.claude/skills/check/SKILL.md` (`/check` uses the same list; `--list` prints it). CI
also runs `go test -race -count=1 -timeout 40m ./...` on Linux. `-race` needs cgo: where it is
missing (a Windows PC without a C compiler), run `go test -count=3 -timeout 40m ./...` instead.
Do not attach self-hosted runners or route public contributions to private machines.
`CONTRIBUTING.md` has the CI and release details.

## Rules of this repository

- **Standard library only.** No `require` in `go.mod` and no `go.sum`. Windows syscalls use
  `syscall.NewLazyDLL`, never `golang.org/x/sys`; agent CLIs are external programs.
- **No paid model calls.** Tests use fake SSE (`internal/agent/api_test.go`) and `TestHelper*`
  processes for CLIs and MCP servers. Never read `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` or
  run `scripts/api-check.sh`. Run `hrn run`, `batch` or `eval` only when asked: they spend API
  credit or CLI plans, and with no `-b` a set key selects `api`.
- **Records stay loadable.** New fields in `agent.Task`, `agent.Result`, `runlog` or `batch.*` are
  additive with `omitempty`; old records on disk must still load.
- **The api history is append-only** (`api.go`): assistant turns go back exactly as received.
  Never trim, rewrite or reorder earlier messages; context is reduced server-side. With
  `mask_tool_results` set, old tool results are masked in the outgoing request only.
- **Comments say why, not what**: one line, three at most (a doc comment: one sentence, plus one
  for a non-obvious contract); no issue numbers, dates or review history. Long explanations go to
  `docs/`. `scripts/comment-check.sh` enforces it; its baseline only shrinks.
- Keep this file and `CLAUDE.md` short: the api loop loads both (`instructions.go`, 32 KiB cap).
- A release tag matches `Version` in `internal/agent/agent.go`; only a maintainer tags or releases.
- A new flag, field, command or status updates its `docs/` guide (and README if the overview
  changes). A PLAN.md item gets its box ticked, `Done: PR #n, <date>` and its deviations; an
  issue-only fix gets a Decisions-log entry naming the issue and PR. These PLAN.md changes land
  after the merge in a separate Markdown-only PR (CI skips it), never in the code PR.

## Working rules

- **A branch delivers what it is for, nothing else**: one issue or PLAN.md item, a short name
  (`fix-batch-rm-deregistered`), one PR whose description lists the files it touches and why. It
  finishes everything its issue asks, with no refactors, renames or extras beyond it. Larger work
  is a feature branch: each piece's PR targets it under the rules below, and the feature branch
  reaches `main` through one PR reviewed for how the pieces fit.
- **Reviews and fixes.** Self-review with `/code-review`, then get an independent review. Every
  defect, missing test, broken house rule or security hole in what the branch touches is fixed
  before the push, blocking or not, with the smallest change; one review round's fixes go in one
  commit. Nits (wording, naming, style) are dropped, at most a line in the PR. Only a defect
  elsewhere on `main` (its own issue and branch) or a maintainer's choice becomes an issue; a
  follow-up branch files no follow-ups of its own nits.
- **Merge only what is tested.** Merge the base branch in once before the final checks and review
  (CI tests the merge with `main`), or for a conflict. Then `/check` passes, every added or changed
  behaviour has tests, CI is green on GitHub's hosted runners, the review's defects are fixed and a
  security read of the diff has nothing open, all on the head being merged. What could not be
  tested is said in the PR and waits for a maintainer. Merge through the PR only, also for
  PLAN.md's merge step: `gh pr merge <n> --merge --delete-branch --match-head-commit <sha>`. Never
  `--auto` or `--admin`, never push to `main`, never tag or release: hand over the command.
  Rulesets with no bypass protect `main` and `v*` tags; `CONTRIBUTING.md` has the details.
- **Commits**: the area first, lower case, then a plain sentence (`batch rm: name a single leftover
  file instead of counting it`), one logical change per commit. No emoji and no AI attribution
  anywhere (no `Co-Authored-By`, `Assisted-by` or "Generated with" in commits, PR text, comments
  or docs). Use the repository's git identity: check `git config user.email` first.
- Tests first, seen failing. Windows temp dirs have 8.3 short names: compare paths after
  `filepath.EvalSymlinks`. Files are LF (`.gitattributes`). Secrets are never read, printed or
  committed. Small diffs: don't restyle what you didn't change. Not sure? Ask, with one proposal.

## Scripts

Run a fitting script from the index below, or add a parameter if one almost fits. Keep a script
you would run twice or that exceeds ten lines, header first (`docs/scripts.md`). Inputs are
parameters, never hardcoded paths, IDs or dates. Answer with small, capped JSON; fail loud with a
non-zero exit and an error object; be safe to re-run; call other scripts, never copy them.
<!-- scripts index: start -->
- `scripts/answers.go`: Provide JSON test summaries and bounded TCP probes for Bash entry points.
- `scripts/answers_test.go`: Check that TCP probes stay within the wait deadline.
- `scripts/api-check.sh`: Exercise real API runs, continuation and context editing; spends real money. (not verified: spends money)
- `scripts/archtest-baseline.sh`: Compare the architecture baseline and layer map with a base ref.
- `scripts/archtest-baseline_test.sh`: Test baseline comparison and layer map reporting in fixture repositories.
- `scripts/check.sh`: Run the check skill commands in order, stopping on failure.
- `scripts/comment-check.sh`: Check Go comments against the repository rule and baseline.
- `scripts/comment-check/main.go`: Check Go comments against the repository rule and baseline.
- `scripts/comment-check/main_test.go`: Check comment rules, baseline comparison and pruning in fixtures.
- `scripts/privacy-check.sh`: Check tracked text files for personal folders, local work paths and narration of a person.
- `scripts/privacy-check/main.go`: Check tracked text files for personal folders, local work paths and narration of a person.
- `scripts/privacy-check/main_test.go`: Check the personal folder, local path and narration rules on fixture files.
- `scripts/test-summary.sh`: Count test results and return capped failure names as JSON.
- `scripts/tool-shapes.sh`: Measure recorded tool output, repeated inline scripts and tool definitions without exposing text.
- `scripts/tool-shapes/defs.go`: Measure the api loop's tool definitions against the recorded context per model call.
- `scripts/tool-shapes/main.go`: Measure recorded tool output, repeated inline scripts and tool definitions without exposing text.
- `scripts/tool-shapes/main_test.go`: Check tool shape classification, normalisation and record accounting.
- `scripts/verify.sh`: Run kept examples and synchronize script hashes and the AGENTS index. (not verified: the runner)
- `scripts/verify_test.sh`: Test verifier stamping, exclusions, errors and read-only checks in fixtures.
- `scripts/wait-for.sh`: Wait for a TCP port with a bounded timeout.
<!-- scripts index: end -->
