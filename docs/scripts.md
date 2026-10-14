# Kept scripts

Use Bash (Git Bash on Windows, Bash on Linux), or standard-library Go via
`go run`. Run examples from the repository root. No Python is needed.

Every file under `scripts/` starts with this header (after a shell shebang).
Go uses `//` instead of `#`:

```bash
# name: example
# summary: One line describing the answer.
# effects: read, exec
# parameters: file:path=stdin; limit:int=20
# example: bash scripts/example.sh
# hash: <sha256 of the file with this line removed>
```

Effects are a comma-separated subset of `read`, `write-workspace`, `exec`,
and `network`. Parameters give each input's type and default; use `required`
when there is no default, and `none` when there are no inputs. Environment
inputs are parameters too. An example is executable Bash and is the script's
test: it must assert its answer, finish in seconds, use no network or model,
and leave no files in the tracked tree. Temporary fixtures must be cleaned up.
The local TCP probe's example checks a closed loopback port.

- Consult the index first: run a fitting script; add a parameter if it almost fits.
- Keep a script you would run twice or that exceeds ten lines.
- Make inputs parameters, never hardcoded paths, IDs or dates.
- Return an answer, not raw data: a small JSON object, with capped output.
- Fail loud: non-zero exit and an error object.
- Be safe to run again.
- Write the header first.
- Compose by calling scripts, never copying their code.

`bash scripts/verify.sh` reads headers of `.sh` and `.go` files, runs each
example, stamps SHA-256 excluding the hash line, and replaces the marked index
in AGENTS.md with sorted paths and summaries. Failed examples are reported,
omitted from the index and cause a non-zero exit. Header errors also fail.
Writes are in place so Windows can handle files held open by other processes.

`bash scripts/verify.sh --check` performs the same examples but writes nothing.
It reports failing examples, missing or stale hashes, and a differing index,
and exits non-zero for any of them. A second normal run is idempotent.
`--root path` selects a fixture repository for the verifier's tests.

The runner has `example: none (the runner)` and `hash: none`: it neither runs
nor stamps itself. Its behavior is tested by `scripts/verify_test.sh`.
Explicit exclusions use `example: none (reason)` and `hash: none`; they remain
indexed with `(not verified: reason)`. The paid `api-check.sh` is excluded and
keeps its existing behavior. No example may invoke it.

`check.sh --list` prints the check skill's commands without executing them;
normal execution stops on the first failure. Each check reports its step number;
failures include at most 2,000 bytes of diagnostics. `test-summary.sh [file]`
reads JSON test events from a file or stdin, counts terminal test events (including
subtests, excluding package events), and returns at most 20 failure names,
each at most 200 characters, plus `truncated`. Invalid input is an error.
`wait-for.sh host port [timeout]` waits for TCP connection success; timeout
defaults to 10 seconds. Zero timeout makes one immediate probe. Invalid inputs
and timeouts return error objects. Successful probes return `{"ready":true}`.
