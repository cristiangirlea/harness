---
name: check
description: Run what CI runs on this module (vet, build, tests, fuzz, scripts, release dry run, gofmt) before a PR
---
Run the steps in order from the repository root and stop at the first failure. Report each
step's result in one line; on a failure, show the error text and fix it before re-running.

```bash
go vet ./...
go build ./...
go test -count=1 ./...
go test -run='^$' -fuzz=FuzzParseSSE -fuzztime=10s ./internal/agent
go test -run='^$' -fuzz=FuzzCodec -fuzztime=10s ./internal/mcp
bash -n .github/release.sh && bash -n scripts/api-check.sh
bash .github/release.sh
test -z "$(gofmt -l .)"
```

CI also runs `go test -race -count=1 ./...` on Linux. It needs cgo: run it only where
`go env CGO_ENABLED` prints 1 and a C compiler is present; on the owner's Windows PC report it as
"left to CI" and run `go test -count=3 ./...` instead. The release dry run writes `dist/`
(ignored). Nothing here may call a model API: never run `scripts/api-check.sh` or `hrn run`.
