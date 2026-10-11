---
paths:
  - "**/*_test.go"
---
# Tests

- Write the test first and see it fail. Name it as a sentence about behaviour
  (`TestEvalListShowsTheNewestFirst`), not `TestFoo`.
- Code that starts a process is tested by re-running the test binary as the child:
  `os.Args[0]` with `-test.run=^TestHelper...$` and an environment flag that turns the helper on
  (`TestHelperCLIError` in `internal/agent/cli_run_test.go`, `TestHelperMCPServer` in
  `internal/mcp/mcp_test.go`). Never depend on an installed claude, codex, gemini or opencode.
- The api loop is tested against the `httptest` fake SSE server in `internal/agent/api_test.go`.
  No test may reach the real API or need a key.
- A test that writes records sets `t.Setenv("HRN_HOME", t.TempDir())`, never the owner's
  `~/.hrn`. Git-backed tests (`internal/batch`, `internal/eval`, `internal/hrnmcp`) make their own
  repo in `t.TempDir()`. On Windows compare paths after `filepath.EvalSymlinks` (8.3 short names).
- A fuzzer crash: add the input as an `f.Add` seed or a `testdata/fuzz` file with the fix.
- Locally `go test -count=3 ./...`; `-race` runs only in CI (no cgo on the owner's PC).
