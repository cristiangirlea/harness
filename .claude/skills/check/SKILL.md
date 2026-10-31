---
name: check
description: Run what CI runs on this module (vet, build, tests, fuzz, scripts, release dry run, gofmt) before a PR
---
Run the steps in order from the repository root and stop at the first failure. Report each
step's result in one line; on a failure, show the error text and fix it before re-running.

```bash
go vet ./...
go build ./...
go test -count=1 -timeout 40m ./...
go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=150000x -fuzzminimizetime=200x -parallel=4 ./internal/agent
go test -run='^$' -fuzz=FuzzCodec -fuzztime=500000x -fuzzminimizetime=200x -parallel=4 ./internal/mcp
go test -run='^$' -fuzz=FuzzSummarize -fuzztime=14000x -fuzzminimizetime=200x -parallel=4 ./internal/shape
bash -n .github/release.sh && bash -n scripts/api-check.sh
bash scripts/archtest-baseline.sh origin/main
bash scripts/verify.sh --check
bash scripts/comment-check.sh
bash scripts/privacy-check.sh
bash .github/release.sh
test -z "$(git ls-files -co --exclude-standard -- '*.go' | xargs -r gofmt -l)"
```

The gofmt step lists the repository's own Go files, so it does not walk batch worktrees under
`.hrn/worktrees`; CI's `gofmt -l .` sees the same files on a clean checkout.

CI also runs `go test -race -count=1 -timeout 40m ./...` on Linux. It needs cgo: run it only where
`go env CGO_ENABLED` prints 1 and a C compiler is present; on a Windows PC without one report it as
"left to CI" and run `go test -count=3 -timeout 40m ./...` instead. The release dry run writes `dist/`
(ignored). Nothing here may call a model API: never run `scripts/api-check.sh` or `hrn run`.

This skill does not merge: merging is through the PR, under AGENTS.md's conditions.
