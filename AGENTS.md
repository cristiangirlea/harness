# hrn, a small agent harness

One Go binary, `hrn`, runs tasks via Anthropic API (`-b api`) or installed CLIs (claude, codex,
gemini, opencode, agy), saving runs under `~/.hrn` or `$HRN_HOME`. Its api loop uses stdio MCP tools;
`hrn mcp` serves hosts; `hrn batch` uses gated git worktrees; `hrn eval` scores backends and models.

README has the quick start and guide index; `docs/commands.md`, `docs/permissions.md`, and
`docs/run-records.md` cover commands, guards, and records. Read `PLAN.md` before taking an item;
don't reopen its decisions. Older entries use `hx` (`cmd/hx`, `$HX_HOME`); code uses `hrn`.

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

Go 1.26.x (`go.mod`), nothing else. Run these CI checks before a PR (`/check` does):

```bash
go vet ./...
go build ./...
go test -count=1 -timeout 40m ./...
go test -race -count=1 -timeout 40m ./...                     # Linux only in CI
go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=150000x -fuzzminimizetime=200x -parallel=4 ./internal/agent
go test -run='^$' -fuzz=FuzzCodec -fuzztime=500000x -fuzzminimizetime=200x -parallel=4 ./internal/mcp
go test -run='^$' -fuzz=FuzzSummarize -fuzztime=14000x -fuzzminimizetime=200x -parallel=4 ./internal/shape
bash -n .github/release.sh && bash -n scripts/api-check.sh
bash scripts/verify.sh --check
bash scripts/comment-check.sh
bash scripts/privacy-check.sh
bash .github/release.sh                                       # release dry run, writes dist/
test -z "$(gofmt -l .)"                                       # /check: tracked files only
```

`-race` needs cgo: where it is missing (a Windows PC without a C compiler), run
`go test -count=3 -timeout 40m ./...` and leave `-race` to CI. Do not attach self-hosted runners
or route public contributions to private machines. See `CONTRIBUTING.md` for CI and release details.

## Rules of this repository

- **Standard library only.** No `require` in `go.mod` and no `go.sum`. Windows syscalls use
  `syscall.NewLazyDLL`, never `golang.org/x/sys`; agent CLIs are external programs.
- **No paid model calls.** Tests use fake SSE (`internal/agent/api_test.go`) and `TestHelper*`
  processes for CLIs and MCP servers. Never read `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` or
  run `scripts/api-check.sh`. Run `hrn run`, `batch`, or `eval` only when asked: they spend API
  credit or CLI plans; with no `-b`, a set key selects `api`.
- **Records stay loadable.** Fields added to `agent.Task`, `agent.Result`, `runlog` or `batch.*` are
  additive with `omitempty`; old records on disk must still load.
- **The api history is append-only** (`api.go`): assistant turns go back exactly as received. Never
  trim, rewrite or reorder earlier messages; context is reduced server-side.
- **Comments say why, not what**: one line, three at most (a doc comment: one sentence, plus one
  for a non-obvious contract); no issue numbers, dates or review history (git and PLAN.md hold
  them); long explanations go to `docs/`. `scripts/comment-check.sh` enforces it; its baseline
  only shrinks.
- The api loop puts this file and `CLAUDE.md` into its system prompt when it runs on this repo
  (`instructions.go`, 32 KiB cap): keep them short.
- A release tag must match `Version` in `internal/agent/agent.go`; tags and releases are a
  maintainer's.
- For every new flag, field, command or status, update the relevant `docs/` guide (and README
  if its overview changes). For a PLAN.md item, tick its box, add `Done: PR #n, <date>`, and
  log deviations; for an issue-only fix, add a Decisions-log entry naming the issue and PR.
  These PLAN.md changes land in a separate Markdown-only PR once the change has merged (CI skips
  it), never in the code PR: an entry at the top of the log conflicts with every open PR (#374).

## Working rules

- **A branch delivers what it is for, nothing else.** One purpose per branch (one issue or PLAN.md
  item; short name, as in `fix-batch-rm-deregistered`) and one PR, whose description lists the files
  it touches and why. Larger work is a feature branch: each piece is a branch whose PR targets the
  feature branch under every condition below, and the feature branch reaches `main` through one PR
  reviewed for how the pieces fit. The branch finishes everything its issue asks. A defect found
  in code the branch touches is fixed here; one elsewhere, already on `main`, gets an issue and a
  branch of its own. No refactors, renames or extras the item did not ask for.
- **Everything is tested before a merge**: the full local checks (`/check`), tests for every
  behaviour the change adds or alters, and CI green on GitHub's standard hosted runners. Only then
  may an agent merge its PR, and only through the PR (`gh pr merge <n> --merge --delete-branch
  --match-head-commit <sha>`), when an independent review ran and every defect it found is fixed, a
  security read of the diff has nothing open, and the head being merged is the commit all of that
  ran on. Merge the base branch in once before those final checks and review (CI tests the merge
  with `main`, so local checks must too), or for a conflict. What could not be tested is said in the
  PR and waits for a maintainer. Never auto-merge (`--auto`) or `--admin`, never push to `main`,
  never tag or release: hand over the command. PLAN.md's merge step (merge commit, delete the
  branch) is done through the PR under these conditions. Rulesets with no bypass protect `main` and
  `v*` tags: GitHub itself refuses a direct push to `main`, a squash or rebase merge, a merge with a
  failing or missing check, and a moved or deleted `v*` tag; in an emergency a maintainer turns the
  ruleset off in Settings > Rules and back on after.
- Commit messages: the area first, lower case, then a plain sentence (`batch rm: name a single
  leftover file instead of counting it`), one logical change per commit. No emoji, no AI attribution
  anywhere (no `Co-Authored-By`, `Assisted-by` or "Generated with", in commits, PR text, comments or
  docs). Commits use the git identity configured for the repository: check `git config
  user.email` first.
- Self-review with `/code-review`. Every defect, missing test, broken house rule or security hole in
  what the branch touches is fixed before the push, blocking or not, with the smallest change; the
  fixes of one review round go in one commit, never a commit per finding. Nits (wording, naming,
  style) are dropped, at most a line in the PR. Only a defect elsewhere on `main`, or a choice for
  a maintainer, becomes an issue; a follow-up branch files no follow-ups of its own nits.
- Tests first, seen failing. On Windows the temp dir has 8.3 short names: compare paths after
  `filepath.EvalSymlinks`. Files are LF (`.gitattributes`).
- Secrets are never read, printed or committed. Small diffs: don't restyle what you didn't change.
  Not sure? Ask, with one proposal.

## Scripts
- Index first: run a fitting script; add a parameter if it almost fits.
- Keep scripts you would run twice or that exceed ten lines.
- Inputs are parameters, never hardcoded paths, IDs or dates.
- Answer, not data: small JSON objects, capped output.
- Fail loud: non-zero exit with an error object.
- Be safe to re-run.
- Header first; see docs/scripts.md for the format.
- Compose by calling scripts, never copying their code.
<!-- scripts index: start -->
- `scripts/answers.go`: Provide JSON test summaries and bounded TCP probes for Bash entry points.
- `scripts/answers_test.go`: Check that TCP probes stay within the wait deadline.
- `scripts/api-check.sh`: Exercise real API runs, continuation and context editing; spends real money. (not verified: spends money)
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
