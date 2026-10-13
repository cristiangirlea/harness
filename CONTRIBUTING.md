# Contributing to hrn

[← README](README.md)

## Build from source

Use the Go version pinned in `go.mod` (currently 1.26.4):

```bash
git clone https://github.com/cristiangirlea/harness.git
cd harness
go build ./cmd/hrn
go test ./...
```

This produces `hrn` on Linux/macOS or `hrn.exe` on Windows. The Go code uses only the
standard library, with no third-party modules or separate runtime to install.

## Checks and pull requests

Read [AGENTS.md](AGENTS.md) for the complete checks, repository rules, and review/merge
requirements before changing code. Tests use fake API servers and helper processes;
do not make paid model calls as part of validation.

CI tests pull requests and pushes to `main` on standard GitHub-hosted Linux and Windows
runners. Releases build on `ubuntu-latest`. These runners are free for public repositories;
both workflows skip their jobs if the repository is private. No self-hosted runner is needed.
See [SECURITY.md](SECURITY.md) for the security boundaries and private vulnerability reports.

- **Standard library only.** No third-party Go modules, and Windows syscalls go through
  `syscall.NewLazyDLL`, not `x/sys`. The CLI backends are external programs, not
  dependencies.
- **Tests first, seen failing.** Process-spawning code is tested by re-running the test
  binary as a helper; the API loop against a fake SSE server; MCP over in-memory pipes.
- **One pull request per increment**, small enough to review, with the plan updated in
  the same PR.

## Release development

Releases are cut by pushing a `v*` tag that matches `agent.Version` in
`internal/agent/agent.go`: the release workflow runs the tests, builds with
`.github/release.sh`, and publishes. Tags and releases are the owner's to create.
A pushed `v*` tag cannot be moved or deleted (a GitHub ruleset refuses it), so a tag on the
wrong commit, or a release whose tests failed, is followed by a new version: bump
`agent.Version` and tag again.

For a local release dry run, use `bash .github/release.sh`. It writes static binaries
for Linux, macOS, and Windows (amd64 and arm64), plus `SHA256SUMS`, into `dist/`.
Running that script does not publish a release. Use Git Bash on Windows.

## Layout

```
cmd/hrn/           CLI + terminal renderer (batch subcommands in batch.go)
internal/agent/    Backend interface, api loop + stdlib Messages client, one file per CLI adapter
internal/tools/    workspace-confined tool runtime + MCP tool adapter
internal/mcp/      MCP over stdio: JSON-RPC codec, Client, Server, config loader
internal/hrnmcp/   hrn's own MCP server (run, runs, show, backends)
internal/batch/    orchestrator: parallel worktrees, gates, retries, batch records
internal/eval/     a batch across a matrix of variants, scored per variant and per task
internal/report/   aggregation over run and batch records (hrn report; reused by eval)
internal/runlog/   run recording
internal/watch/    following records as they are written: hrn show -f, and hrn watch's page
internal/qr/       QR codes for hrn watch's phone links (byte mode, level M, versions 1-10)
internal/followup/ continuing a run: what the backend picks up, what it is handed
internal/limits/   plan usage limits: which backend takes over, the reset times in limits.json
internal/gc/       hrn gc: which worktrees are proven merged, and removing them
```

## Validation notes and work queue

The original implementation notes below describe the validation still outstanding
at that stage; they are not a current compatibility matrix.

The three layers from the original brief are all here: the agent core (`hrn run`), the
orchestrator (`hrn batch`, `hrn mcp`) and the scoring layer (`hrn eval`). What remains,
in the order it is worth taking:

1. **A real run of the `api` backend**, and with it **the context-editing measurement**.
   It is the only backend that has never spoken to its API. `scripts/api-check.sh` does
   both in one command once `ANTHROPIC_API_KEY` is set: a read-only run, a `--continue`
   of it, and one long task with context editing on and off, printed side by side with
   the server's reports of what it cleared. It makes paid API calls and records
   into a temporary `HRN_HOME`.

`PLAN.md` is the work queue and the decisions log: every increment so far, what was
deferred and why. It still calls the binary by its old name, `hx`, in entries written
before the rename.
