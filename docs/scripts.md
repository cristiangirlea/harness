# Kept scripts

Use Bash (Git Bash on Windows, Bash on Linux), or standard-library Go via
`go run`. Run examples from the repository root. No Python is needed.

Every `.sh` and `.go` file in `scripts/`, and every `.go` file one directory
down (a Go package such as `scripts/tool-shapes/`, its tests included), starts
with this header (after a shell shebang). Go uses `//` instead of `#`. The
header ends at the first comment line that is not a lowercase `key: value`, so
prose comments after it are not read as keys:

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

An optional `# matches: <Go regexp>` line (RE2 syntax, one line) says which
inline scripts the kept script covers. The pattern is matched against an
inline script's normalised text: trimmed, with each run of whitespace
collapsed to one space. A script with no `matches:` line is never hinted.

- Consult the index first: run a fitting script; add a parameter if it almost fits.
- Keep a script you would run twice or that exceeds ten lines.
- Make inputs parameters, never hardcoded paths, IDs or dates.
- Return an answer, not raw data: a small JSON object, with capped output.
- Fail loud: non-zero exit and an error object.
- Be safe to run again.
- Write the header first.
- Compose by calling scripts, never copying their code.

`bash scripts/verify.sh` reads headers of `scripts/*.sh`, `scripts/*.go` and
`scripts/*/*.go`, runs each example, stamps SHA-256 excluding the hash line, and
replaces the marked index in AGENTS.md with sorted paths and summaries. Failed
examples are reported, omitted from the index and cause a non-zero exit. Header
errors also fail, and so does any other file: in `scripts/` only `.sh`, `.go`
and `.txt` data (such as the comment baseline) are allowed, one directory down
only `.go`; `scripts/testdata/` is not checked. Shell scripts are mode 100755
but are always run through `bash`.
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

`check.sh --list` prints the check skill's commands without executing them:
each non-blank line of the first `bash` block in its `SKILL.md`. Normal
execution stops on the first failure; steps run with stdin from `/dev/null`. Each check reports its step number;
failures include at most 2,000 bytes of diagnostics. `test-summary.sh [file]`
reads JSON test events from a file or stdin, counts terminal test events (including
subtests, excluding package events), and returns at most 20 failure names,
each at most 200 characters, plus `truncated`. Invalid input is an error.
`wait-for.sh host port [timeout]` waits for TCP connection success; timeout
defaults to 10 seconds. Zero timeout makes one immediate probe (200 ms dial).
Invalid inputs and timeouts write only the error object to stderr (`go run`'s
`exit status` line is dropped). Successful probes return `{"ready":true}`.

`comment-check.sh [--root DIR] [--baseline PATH] [--prune] [--init]` scans
non-test `.go` files with `go/scanner`, flagging comment blocks over five
counted lines (`long`) and comments citing `#<digits>`, `YYYY-MM-DD` dates, or
review narration (`cite`). Directives and leading kept-script headers are
exempt. `comment_bytes` and `comment_share` count whole-line comments only
(trailing comments are checked for citations but not counted), so the share is
comparable across runs. It compares violations by `(path, fingerprint)` against
`scripts/comment-baseline.txt`, prints a JSON summary, and exits 1 when any new
violation or stale baseline entry is found. Only `--prune` removes stale lines
from the baseline (without adding new ones), while a renamed file may have its
baseline path edited by hand.

## The hint hook

`hrn scripts hint` is a Claude Code PreToolUse hook. It reads the hook's
payload on stdin and, for a Bash or PowerShell command that runs an inline
script, looks for a kept script that covers it: a heredoc body (`<<EOF`,
`<<'EOF'`, `<<"EOF"`, `<<-EOF`), the argument of `python`/`python3`/`py -c`,
`node -e`, `bash -c`/`sh -c`, or a PowerShell `-Command`/`-c` script block or
multi-statement string. It finds the repository root by walking up from the
payload's `cwd` to the first `.git`, then tests each `matches:` line of
`scripts/*.sh` and `scripts/*.go` in file-name order. On the first match it
adds one sentence of context for the agent, such as
`scripts/wait-for.sh covers this inline script; parameters: host:string=required; port:int=required; timeout:seconds=10.`

It only advises: it never blocks, never rewrites the command and never makes
a permission decision. It stays silent and exits 0 on anything it does not
understand (malformed or oversized input, another tool, no inline script, no
repository or `scripts/` directory), skips a header whose pattern does not
compile, and gives no hint when the command already runs a kept script.
Since any repository you open can commit scripts, hint text is limited to a
file name of `[A-Za-z0-9._-]` and a `parameters:` value of at most 200 bytes
of `[A-Za-z0-9_:=;.,[]|/ -]`; a script outside either is never hinted.

To opt in, add it to your `~/.claude/settings.json` or a repository's
`.claude/settings.json` (this repository does not turn it on for itself):

```json
{"hooks":{"PreToolUse":[{"matcher":"Bash|PowerShell","hooks":[{"type":"command","command":"hrn scripts hint || true"}]}]}}
```

The `|| true` is there because an `hrn` that predates the command exits 2 on
it, which Claude Code treats as a block of the tool call.

Each hint shown appends one line, `{"time":"<RFC 3339>","script":"scripts/<file>","repo":"<root>"}`,
to `$HRN_HOME/script-hints.jsonl` (`~/.hrn` by default); a failed write is
ignored so the hook never fails.
