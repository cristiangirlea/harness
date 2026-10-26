# Every backend's commands through the gate

[← README](../README.md)

Status: proposed, 2026-10-20, for issue #435. Nothing here is built; each slice below is its own
pull request once this note is approved. The gate is
[adanext-exec](https://github.com/cristiangirlea/adanext-exec); its design and build happen there.

## The goal

One place controls three things for every backend's agent: what it may run, how hrn watch shows
what it ran, and how many tokens the output costs. Today only the api loop has all three:
`run_command` (`internal/tools/shell.go`) applies hrn's guard, picks the shell and, with
`--shape-tools`, shapes large output and keeps the original behind a handle. claude, codex and
agy run commands with their own shell tools; hrn steers them from outside (the guarded
environment, claude's `--disallowedTools` rules, codex's sandbox modes) and their models read raw
output. Terminal output is 77% of all tool-result bytes in the 30-day snapshot of
[tool-shapes](tool-shapes.md), and shaping could touch up to about 10% of all input tokens
(about 16% for claude) once replayed. Those are upper bounds, not measured savings, and none of
it reaches the CLI backends today.

## How claims are marked

**Verified** means documented in the installed CLI's `--help` on the development PC on 2026-10-20:
Claude Code 2.1.284, codex-cli 0.163.0-alpha.10.2, agy 1.4.2. Anything else is **unverified**. A
documented flag is not a tested behaviour: each slice runs its live check before it merges.

## How each backend's commands reach the gate

**The api loop.** hrn is already the terminal. `run_command` calls the gate instead of
`runShell`: shell text with a named shell (Git's own bash on Windows, never a WSL launcher, which
settles #86 by design), the run's policy profile, its timeout and the run id as the caller. The
gate returns the exit code, the output (capped, or shaped) and its decision. Two ways to call
it: the Go SDK (`broker`), or the `adanext-exec run` binary and its JSON protocol. The SDK is a
`require` in `go.mod`, which this repository's rules forbid today, so the first is an open
decision (below); the binary works either way.

**claude.** Two routes; the note proposes the first and keeps the second as the fallback.

1. *A PreToolUse hook.* hrn adds the hook for one run with `--settings <file-or-json>`
   (verified: `claude --help`), matching `Bash|PowerShell|Monitor`; claude runs recorded by hrn
   use all three to run commands (last 400 runs: 1,195 Bash, 47 PowerShell and 25 Monitor calls,
   Monitor taking a `command` like Bash). The hook, `adanext-exec hook claude-code`
   (adanext-exec #8), decides, and rewrites an allowed command to `adanext-exec run --
   <command>` through `updatedInput`, so it runs under the gate's limits, audit and shaping.
   Refusing a Bash or PowerShell call from a PreToolUse hook (exit 2) is in use in
   adanext-exec's own push guard; the `updatedInput` rewrite on 2.1.284 is
   **unverified**, and so is whether the hook fires for a subagent's calls and in
   `bypassPermissions`. The same run passes `--tools` (below) naming Bash, PowerShell, Monitor
   and the tools that run no code, so REPL and the other code-running tools the hook does not
   cover are off. Print mode "silently" ignores settings files that fail validation (verified:
   `claude --help`, `-p`), so a broken hook would leave the run ungated without a word. hrn
   passes `--include-hook-events` (verified, stream-json only) and stops the run when a command
   call arrives without the hook's event. That detects; it does not prevent: whether the event
   comes before the command runs is **unverified**, so one ungated command may have run.
2. *The shell tools off and a gate-backed tool.* `--tools` takes the list of built-in tools
   (verified: `claude --help`, `""` disables all), so hrn names the tools it keeps and leaves out
   Bash, PowerShell, Monitor and the other code-running tools (`--restricted`'s help names
   "Bash, PowerShell, REPL and the other code-running tools"; an allow list, since that set
   grows). Commands go to the gate-backed MCP tool (below), loaded with `--mcp-config` and
   `--strict-mcp-config` (verified, already passed by hrn). This route prevents rather than
   detects, but the model loses its native shell tool, so it needs the eval before it is a
   default. Whether print mode calls an MCP tool without a prompt in each permission mode is
   **unverified**; `--allowedTools` (verified) can name it, only where the mode allows commands.

Either way, hrn keeps `--disallowedTools`. After a rewrite, settings rules see `adanext-exec run
-- ...` instead of the original (adanext-exec #6), so the gate's policy is what refuses it.

**codex.** codex documents no flag to wrap or rewrite one command, so the proposal turns its
command tools off and gives it the gate-backed tool: `--disable <FEATURE>` (verified: `codex exec
--help`) for `shell_tool` and `unified_exec`, both stable and on (verified: `codex features
list`). That these two are all of codex's command tools, and that `code_mode_host` (stable, on)
runs nothing, is **unverified**; the live check is a run with no `command_execution` item. The
tool reaches codex through the `-c mcp_servers.<name>.command` overrides hrn already writes
(verified: `-c`); whether `codex exec` runs an MCP call without approval in each sandbox mode is
**unverified**. codex has hooks too: the `hooks` feature is stable and on, and
`--dangerously-bypass-hook-trust` exists (verified: `codex features list`, `codex exec --help`).
Their events, and whether one can refuse or rewrite a shell call, are **unverified**; a hook
route would replace the MCP tool only if it can rewrite.

codex's sandbox is the catch. In `readonly`, `ask` and `auto`, codex runs commands inside its
sandbox (`--sandbox read-only|workspace-write`, verified). That an MCP server runs outside it is
**unverified** but must be assumed, and the gate is not a sandbox, so routing those modes would
trade codex's sandbox for a textual policy. This design therefore routes codex in `yolo` only
(which bypasses the sandbox today) and leaves the sandboxed modes on codex's own shell. Routing
them later needs its own design: the bridge would run `codex sandbox -C <dir> -- <command>`
under the gate (verified: `codex sandbox --help`; that it applies `exec --sandbox`'s policy is
**unverified**), and since codex runs sandboxed commands without asking today, the bridge's
mode rule below would change for codex in those modes.

**agy.** agy 1.4.2 documents no flag to turn off its terminal tool, add a hook, or add an MCP
server for one run (verified by their absence from `agy --help`; `agy mcp add` changes its
persistent configuration, which hrn never does for a run). Its `--sandbox` "with terminal
restrictions" (verified) is agy's own. So agy cannot be routed; whether it may still run,
labelled ungated, is an open decision. gemini and opencode were not examined; until they are,
they are ungated like agy (**unverified** that they lack a route).

**The gate-backed tool.** Route 2 and codex need an MCP tool whose calls run through the gate.
hrn serves it (a stdio server in the `hrn` binary, started per run like its other MCP servers):
`run_command` and `read_original` with the api loop's definitions, so one definition and one
result shape serve api, claude and codex. It passes the run id as caller and the run's profile;
its results land in the CLI's stream, which hrn records as today. The guard is not the
permission mode: the api loop offers no command tool in `readonly` and asks before every command
in `ask` and `auto` (`internal/agent/api.go`). The bridge keeps that: it is offered in `yolo`
only, since elsewhere it would have to ask a person and a CLI run has no channel for that. So
route 2 leaves a non-`yolo` claude run without commands, where today the user's permission
rules let some Bash commands run; the hook route keeps claude's permission handling, one reason
it is the proposal.

## What harness keeps and what moves into the gate

Moves into the gate:

- **Running the command**: the shell choice, the timeout, the whole-tree kill (today
  `internal/tools/exec_windows.go`, which adanext-exec takes over), the output cap.
- **The command guard's rules** (`internal/tools/guard.go` and the secret-file names it reads),
  as adanext-exec's M2 rule set, kept in step with harness by running harness's guard tests
  against it. hrn hands the gate a profile per run: the guard rules, or none under
  `--allow-dangerous`; in `ask` mode an ask callback where a person can answer
  (adanext-exec #20), and refuse where none can.
- **Shaping a command's output** and storing its original (adanext-exec #1).
- **The audit line**: what ran, for which run, the decision and its rule, wait, duration, exit,
  whether output was cut or shaped.

Stays in harness:

- **Run records.** `events.jsonl` stays the run's record; a tool result gains additive
  `omitempty` fields from the gate's answer (decision, rule, exit, duration, cut, shaped, handle).
- **The guarded environment** (`GuardEnv`: withheld secrets, the push dead end, empty login
  directories). The gate passes the environment through unchanged (adanext-exec #13), so this
  stays hrn's and keeps covering what textual rules cannot. claude's Read and Edit deny rules for
  secret files stay too: they are file tools, not commands.
- **The api file tools' guards** (paths, links, secret files), which are not commands.
- **The watch display.** Watch shows a command as the agent wrote it, without the `adanext-exec
  run --` wrapper or the shell's interpreter (paths are already shortened, PR #518), with the
  gate's decision, rule, exit code, wait and, when shaped, the original's size. A run on a
  backend that cannot be routed is marked `ungated` in its header (agy's stream carries no
  command events today, so there is nothing per command to mark). The audit line carries the
  run id, so watch and `hrn show` can link it.
- **Shaping's summarizer for MCP results** in the api loop (`internal/shape`), and the
  originals store: the per-run directory `<run>/originals`, its caps (64 MiB a run, 1 GiB in
  all, refused rather than evicted) and continuation copying. hrn names that directory to the
  gate, so every original of a run sits in its record whichever backend ran it.

One summarizer, not two: the shaping code and its adversarial tests move to adanext-exec with
#1, and harness either imports it (the same `require` decision as the SDK) or keeps a copy held
in step by the same tests. The note recommends the import if the one
dependency is allowed, since two copies of an invariant-checked summarizer is where a bad summary slips in.

## The adanext-exec milestones it needs

- **M1, one command contained** (`broker.Run`: timeout, tree kill, output cap, audit line).
  Blocking issues: #4 (argv or shell text with a named shell: hrn passes shell text), #18 (batch
  files on Windows), #19 (console signals and nested jobs: hrn already runs in a job), #10 and
  #11 (Unix kills), #13 and #14 (environment, working directory and audit redaction said
  plainly). #22 (splitting M1) changes order, not need.
- **M2, policy**: the guard rule set with harness's tests. #20 (ask callback) blocks `ask` mode
  only; without it `ask` refuses what it would ask about.
- **M3, the binary and JSON protocol v1** (`run`, `check`). Blocking: #12 (distinct exit codes:
  hrn must tell a refusal, a timeout, a kill and a missing gate apart).
- **M6, the Claude Code hook**: #5 (PowerShell text not reparsed), #6 (the hook decides), #7 (how
  it finds the binary, failing closed when missing or older), #8 (a subcommand, not a script).
- **#1, shaping at the gate**, which no milestone holds yet; shaping on claude and codex waits
  for it. #2 (kept-script hints) does not block.
- **Not needed**: M4 (daemon, heavy slots: #9, #15, #16, #17) and M5 (memory ceilings). They
  help batch gates later. #21 (trust statement) should land but blocks nothing; #23 to #25 are
  adanext-exec's own push-guard hook, which #8 replaces.

Gaps with no adanext-exec issue yet, to file there once this note is approved: the caller
names the originals directory and its caps (#1 keeps originals "under the gate's own
directory"); the audit carries a caller-given run id that hrn can read back per run; `run`
streams output live for long commands (Monitor, background Bash), where shaping does not apply;
and `original <handle> --range` output is neither shaped again nor cut by the gate's output cap,
so an original can be read whole in ranges below the CLI's own output limit.

## Slices, in order

Each is one branch and one pull request in harness, behind an opt-in run option (`--gate`,
default off) until a maintainer turns it on.

1. **api loop through the binary** (needs M1, M3). `run_command` runs through `adanext-exec run`
   with the shell named, the run id as caller and a version handshake; when `--gate` is on and
   the binary is missing or older, the run fails before it starts. hrn's guard still runs first.
   Records gain the gate fields. Tests use a `TestHelper*` stand-in for the binary.
2. **The guard handed to the gate** (needs M2). hrn passes the profile; `internal/tools/guard.go`
   becomes a thin adapter once harness's guard tests pass against the gate; the guard stays in
   hrn for runs without `--gate`.
3. **claude through the hook** (needs M6). `--settings` with the hook, `--tools`,
   `--include-hook-events` and stopping a run whose command call has no hook event; a live check
   list in [permissions](permissions.md) like the deny rules' (`git push` refused, a long `go
   test` killed at its timeout with its children, a PowerShell pipeline unchanged).
4. **The gate-backed MCP tool and codex** (needs M3). hrn serves `run_command` and
   `read_original`; codex gets `--disable shell_tool --disable unified_exec` and the server, in
   `yolo` only. hrn's codex parser reads only `command_execution` items today, so it learns the
   item codex streams for an MCP call (its name **unverified**) and records it as a shell call.
   Live check: no `command_execution` item in a routed run.
5. **Watch shows the gate's record**: decision, rule, exit, wait, shaped size, the audit link,
   and `ungated` on the header of a run that is not routed. Display only.
6. **Shaping through the gate** (needs #1, the summarizer decision and the ranged `original`
   gap). `--shape-tools` reaches the routed combinations only: api, claude through the hook, and
   codex in `yolo`. hrn names `<run>/originals` to the gate; the summary's last line says how to
   read the original (`read_original` through the hrn tool, or `adanext-exec original <handle>
   --range`, itself run through the gate). The sentence in
   [commands](commands.md) that the flag does not change CLI backend tools becomes the list of
   combinations it does reach.
7. **Batch gates through the gate** (needs M1, M3). An operator profile with no guard rules, as
   today, gaining the timeout, tree kill and audit; heavy-command slots when M4 lands.

## How shaping reaches every backend, and the eval

After slices 3, 4 and 6, every command of a routed run (api; claude; codex in `yolo`) runs
through the gate, so `shape_tools` means the same there: the gate shapes a large result, the
original sits in the run's record, and the model can read it back. agy and codex's sandboxed
modes stay unshaped. The eval then runs where the tokens are: an hrn-evals suite in the shape of
hrn-evals #69 (one exact fact buried in a large test, log, JSON, table or terminal output, plus
a no-op task) on claude and on codex in `yolo`, three repeats each, in three arms: no gate (the native shell, today's baseline), gate with raw
output, and gate with shaping. The first two measure what routing costs, codex's tool
replacement included; the last two measure shaping. It records pass rate and input, cache-read
and cache-write tokens per arm from the run records, runs on subscription plans and so waits for
a maintainer's yes. harness #542 (eval checks that ignore `shape_tools` on variants) is fixed
first, or arms could be scored as one.

## Risks

- **Textual policy, not a sandbox.** The gate stops plausible accidents; the guarded environment
  stays, and nothing here is presented as isolation. Routing codex's sandboxed modes would weaken
  a real boundary, which is why this design leaves them on codex's shell.
- **Silent ungating.** A hook that does not load, a missing binary, or a CLI update that renames
  or adds a tool would leave commands ungated. The start-time check under `--gate`, the
  `--tools` allow list, the hook-event check (which detects after the fact) and `ungated` in
  watch answer it; the handshake (adanext-exec #7) and each slice's live check, repeated on a
  CLI update, answer version skew.
- **Answer quality.** A model may do worse with a non-native shell tool or a shaped result. The
  eval measures both; neither becomes a default without it.
- **Cost per command.** The hook adds a decision and a second process per command; slice 3
  measures it. **Two records**: the audit and hrn's events can disagree; hrn's record is the
  run's truth, and the audit is linked, never merged in.

## Refused

- Changing a CLI's persistent configuration to route a run (`agy mcp add`, `codex features
  enable`, `~/.claude/settings.json`): per-run flags only.
- A shim named `bash` or `powershell` on `PATH`: it would catch the user's own shells, and codex
  starts PowerShell by its full path (seen in hrn's run records, `pwsh.exe` or `powershell.exe`;
  no flag documents it, so **unverified** as a rule).
- codex's `exec-server`: experimental (verified: `codex --help`) and made to register remote
  environments (verified: `--remote`); serving it would mean implementing codex's own executor
  protocol, which no documentation covers (**unverified** that it could route one run's commands).
- Rewriting tool output in a PostToolUse hook: **unverified** that Claude Code allows it for
  Bash, and the gate already sees the output.
- Running ungated without saying so.

## Open decisions

1. May harness `require` adanext-exec (public, standard library only) for the SDK and
   the summarizer, or does it call the binary and keep a tested copy of the summarizer?
2. claude: the hook (recommended) or the shell tools off with the gate-backed tool?
3. codex: route `yolo` only and leave the sandboxed modes on codex's shell (this design), with
   a later design for them through `codex sandbox`, or give up routing codex's sandboxed modes?
4. Backends that cannot be routed (agy now, gemini and opencode until checked): allowed and
   labelled `ungated`, or refused when `--gate` is on?
5. When `--gate` is on and the gate is missing or older: fail closed (recommended) or run
   ungated with a warning?
6. Whether the eval's result decides the default for `--gate` and `shape_tools`, and its bar.
