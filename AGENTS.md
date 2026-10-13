# hrn, a small agent harness

One Go binary, `hrn`, that runs coding tasks on its own Anthropic API loop (`-b api`) or on agent
CLIs that are installed (claude, codex, gemini, opencode), and records every run under
`~/.hrn` (or `$HRN_HOME`). It speaks MCP both ways: the api loop uses stdio MCP servers' tools, and
`hrn mcp` serves hrn to hosts such as Claude Code. `hrn batch` fans tasks out into git worktrees with
gates; `hrn eval` scores a matrix of backends and models.

Read the README for the quick start and guide index. `docs/commands.md`,
`docs/permissions.md` and `docs/run-records.md` hold the command, guard and record references. `PLAN.md` is the
work queue and the decisions log: read it before taking an item, and don't reopen a recorded
decision. The binary was called `hx` until a rename; older PLAN.md entries keep that name (`cmd/hx`,
`$HX_HOME`), the code is `hrn` throughout.

## Layout

| Path | Holds |
|---|---|
| `cmd/hrn/` | The CLI and terminal renderer; batch, eval, report, prune and gc subcommands. |
| `internal/agent/` | `Backend` contract (`Task` -> events -> `Result`), the api loop (`api.go`) on a hand-written Messages client (`messages.go`), one file per CLI adapter, project instructions, the price table. |
| `internal/tools/` | Workspace-confined tools for the api loop, the guard every backend runs under (`guard.go` commands, `guardenv.go` environment, `secretfiles.go` the one list of secret files; claude's deny rules are built from it in `internal/agent/guardenv.go`), the MCP tool bridge. |
| `internal/mcp/` | JSON-RPC 2.0 over stdio: codec, client, server, config loader. |
| `internal/hrnmcp/` | `hrn mcp`: run, runs, show, backends, batch, batch_show, batch_merge. |
| `internal/batch/`, `eval/`, `report/`, `purpose/` | Parallel worktrees with gates and retries; a batch across variants with a judge; tables over records; what a run was for, from keyword rules on its prompt. |
| `internal/runlog/`, `followup/` | Run records and pruning; continuing a run on the same or another backend. |
| `internal/gc/` | `hrn gc`: which worktrees are proven merged (by GitHub or the default branch), and removing them. |
| `internal/ws/` | `hrn ws`: reusable worktree slots per repository (`.hrn/slots/<n>`), handed out under O_EXCL lease files. |
| `scripts/api-check.sh` | The first real api runs. Costs real money; the owner's to run. |
| `.github/release.sh` | Builds the six release binaries and `SHA256SUMS` into `dist/`. |

## Build and test

Go as pinned in `go.mod` (1.26.x), nothing else. What CI (`.github/workflows/ci.yml`) runs, in
order; run the same before a PR (`/check` does):

```bash
go vet ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...                                  # Linux only in CI
go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=150000x -fuzzminimizetime=200x -parallel=4 ./internal/agent
go test -run='^$' -fuzz=FuzzCodec -fuzztime=500000x -fuzzminimizetime=200x -parallel=4 ./internal/mcp
bash -n .github/release.sh && bash -n scripts/api-check.sh
bash .github/release.sh                                       # release dry run, writes dist/
test -z "$(gofmt -l .)"                                       # /check: tracked files only
```

`-race` needs cgo, which the owner's Windows PC lacks: run `go test -count=3 ./...` there and leave
`-race` to CI. CI runs on standard GitHub-hosted Linux and Windows runners. Pull requests and pushes to
`main` test both platforms. The workflows run only while the repository is public, so they
cannot consume private-repository Actions minutes. A `v*` tag runs `release.yml` on
`ubuntu-latest`: vet, tests, `release.sh`, then `gh release create`. Do not attach self-hosted
runners or route public contributions to the owner's machines.

## Rules of this repository

- **Standard library only.** `go.mod` has no `require` lines and there is no `go.sum`. Windows
  syscalls go through `syscall.NewLazyDLL` (`internal/tools/exec_windows.go`), never
  `golang.org/x/sys`. The agent CLIs are external programs hrn finds at run time, not dependencies.
- **No paid model calls.** Tests use fakes: an `httptest` SSE server for the api loop
  (`internal/agent/api_test.go`), the test binary re-run as a helper process for CLIs and MCP
  servers (`TestHelper*`). Never read `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` and never run
  `scripts/api-check.sh`. `hrn run`, `batch` and `eval` spend the owner's API credit or CLI plans
  (with no `-b`, the backend is `api` whenever a key is set): only when asked.
- **Records stay loadable.** Fields added to `agent.Task`, `agent.Result`, `runlog` or `batch.*`
  are additive with `omitempty`; old records on disk must still load.
- **The api history is append-only** (`api.go`): assistant turns go back exactly as received.
  Never trim, rewrite or reorder earlier messages; context is reduced server-side.
- The api loop puts this file and `CLAUDE.md` into its system prompt when it runs on this repo
  (`instructions.go`, 32 KiB cap): keep them short.
- A release tag must match `Version` in `internal/agent/agent.go`; tags and releases are the
  owner's.
- Every PR updates the relevant guide in `docs/` (and README when the overview changes)
  for a new flag, field, command or status, and `PLAN.md` for the item it
  delivers (tick the box, a `Done: PR #n, <date>` line, deviations in the Decisions log);
  fixes without a `PLAN.md` item add a Decisions-log entry naming the issue and PR.

## Working rules

- **A branch delivers what it is for, nothing else.** One purpose per branch (one issue or PLAN.md
  item; short name, as in `fix-batch-rm-deregistered`) and one PR, whose description lists the
  files it touches and why. Larger work is a feature branch: each piece is a branch whose PR targets
  the feature branch under every condition below, and the feature branch reaches `main` through one
  PR reviewed for how the pieces fit. A problem found on the way is fixed here only if this branch
  caused it or it stops the change meeting its acceptance criteria; otherwise it gets an issue and
  a branch of its own. No refactors, renames or extras the item did not ask for.
- **Everything is tested before a merge**: the full local checks (`/check`), tests for every
  behaviour the change adds or alters, and CI green on GitHub's standard hosted runners. Only then may an agent
  merge its PR, and only through the PR (`gh pr merge <n> --merge --delete-branch
  --match-head-commit <sha>`), when an independent review ran and its blocking findings are fixed,
  a security read of the diff has nothing open, and the head being merged is the commit all of
  that ran on. Merge the base branch in only for a conflict, or once before those final checks and
  review.
  What could not be tested is said in the PR and waits for the owner. Never auto-merge (`--auto`)
  or `--admin`, never push to `main`, never tag or release: hand over the command. PLAN.md's merge
  step (merge commit, delete the branch) is done through the PR under these conditions.
  Rulesets with no bypass protect `main` and `v*` tags: GitHub itself refuses a direct push to
  `main`, a squash or rebase merge, a merge with a failing or missing check, and a moved or deleted
  `v*` tag; in an emergency the owner turns the ruleset off in Settings > Rules and back on after.
- Commit messages: the area first, lower case, then a plain sentence (`batch rm: name a single
  leftover file instead of counting it`), one logical change per commit. No emoji, no AI
  attribution anywhere (no `Co-Authored-By`, `Assisted-by` or "Generated with", in commits, PR
  text, comments or docs). Commits are `Cristian Girlea <cristiangirlea@gmail.com>`: check
  `git config user.email` first.
- Self-review with `/code-review`. A finding is blocking when it is a defect this branch brings in,
  a missing test for what it changes, a broken house rule, or a security hole: fix each as
  its own commit, with the smallest change that closes it, and don't squash. Everything else
  (problems already on `main`, improvements, style) becomes an issue linked from the PR.
- Tests first, seen failing. On Windows the temp dir has 8.3 short names: compare paths after
  `filepath.EvalSymlinks`. Files are LF (`.gitattributes`).
- Secrets are never read, printed or committed. Small diffs: don't restyle what you didn't change.
  Not sure? Ask, with one proposal.
