# hrn, a small agent harness (private)

One Go binary, `hrn`, that runs coding tasks on its own Anthropic API loop (`-b api`) or on agent
CLIs that are installed (claude, codex, gemini, opencode), and records every run under
`~/.hrn` (or `$HRN_HOME`). It speaks MCP both ways: the api loop uses stdio MCP servers' tools, and
`hrn mcp` serves hrn to hosts such as Claude Code. `hrn batch` fans tasks out into git worktrees with
gates; `hrn eval` scores a matrix of backends and models.

Read the README for commands, flags, the command guard and the run record format. `PLAN.md` is the
work queue and the decisions log: read it before taking an item, and don't reopen a recorded
decision. The binary was called `hx` until a rename; older PLAN.md entries keep that name (`cmd/hx`,
`$HX_HOME`), the code is `hrn` throughout.

## Layout

| Path | Holds |
|---|---|
| `cmd/hrn/` | The CLI and terminal renderer; batch, eval, report and prune subcommands. |
| `internal/agent/` | `Backend` contract (`Task` -> events -> `Result`), the api loop (`api.go`) on a hand-written Messages client (`messages.go`), one file per CLI adapter, project instructions, the price table. |
| `internal/tools/` | Workspace-confined tools for the api loop, the command guard (`guard.go`), the MCP tool bridge. |
| `internal/mcp/` | JSON-RPC 2.0 over stdio: codec, client, server, config loader. |
| `internal/hrnmcp/` | `hrn mcp`: run, runs, show, backends, batch, batch_show, batch_merge. |
| `internal/batch/`, `eval/`, `report/` | Parallel worktrees with gates and retries; a batch across variants with a judge; tables over records. |
| `internal/runlog/`, `followup/` | Run records and pruning; continuing a run on the same or another backend. |
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
go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=10s ./internal/agent
go test -run='^$' -fuzz=FuzzCodec -fuzztime=10s ./internal/mcp
bash -n .github/release.sh && bash -n scripts/api-check.sh
bash .github/release.sh                                       # release dry run, writes dist/
test -z "$(gofmt -l .)"
```

`-race` needs cgo, which the owner's Windows PC lacks: run `go test -count=3 ./...` there and leave
`-race` to CI. Pull requests test on Linux only (`vars.CI_RUNNER`, the cluster); pushes to `main`
also run the Windows leg (`vars.CI_RUNNER_WINDOWS`, a runner on the owner's desktop with no container,
so only merged code reaches it). Both variables are set; the hosted fallbacks in `runs-on` only
apply if one is removed. Don't add hosted jobs. A `v*` tag runs `release.yml` (GitHub-hosted),
which tests, builds with `release.sh` and publishes the release.

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
- Every PR updates the README for a new flag, field, command or status, and `PLAN.md` for the item it
  delivers (tick the box, a `Done: PR #n, <date>` line, deviations in the Decisions log).

## Working rules

- One branch per change (short name, as in `fix-batch-rm-deregistered`), one PR; the owner
  merges. Never `gh pr merge`, never push to `main`, never tag or release: hand over the command.
- Commit messages: the area first, lower case, then a plain sentence (`batch rm: name a single
  leftover file instead of counting it`). No emoji, no AI attribution anywhere (no `Co-Authored-By`,
  `Assisted-by` or "Generated with", in commits, PR text, comments or docs). Commits are
  `Cristian Girlea <cristiangirlea@gmail.com>`: check `git config user.email` first.
- Self-review with `/code-review`; fix each finding as its own commit on the PR, don't squash.
- Tests first, seen failing. On Windows the temp dir has 8.3 short names: compare paths after
  `filepath.EvalSymlinks`. Files are LF (`.gitattributes`).
- Secrets are never read, printed or committed. Small diffs: don't restyle what you didn't change.
  Not sure? Ask, with one proposal.
