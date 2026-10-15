# Backends and continuing runs

[← README](../README.md)

## Backends

The status column preserves the original implementation notes. It is not a current
compatibility matrix for every CLI version; check your installed backend with `hrn backends`.

| Backend | Billing | How it runs | Status |
|---|---|---|---|
| `api` | per token | The harness's own loop: Messages API (streaming, adaptive thinking, prompt caching, server-side refusal fallbacks on `claude-opus-5`) + [built-in tools](permissions.md#api-tools) + MCP tools. Needs `ANTHROPIC_API_KEY` (or an OAuth `ANTHROPIC_AUTH_TOKEN`); `ANTHROPIC_BASE_URL` overrides the endpoint. | Every path is covered by tests against a fake API server; it has not yet run against the real API. |
| `claude` | your Claude subscription | `claude -p --output-format stream-json`. Found on PATH, `~/.local/bin`, npm, or the Claude desktop app's bundled build (that build needs its own `claude /login` first). | Verified end to end: `run`, `batch`, `eval`, and Claude Code delegating through `hrn mcp`. |
| `codex` | your ChatGPT subscription | `codex exec --json`. Found on PATH, npm, or the Codex desktop app's bundled build. | `run` and `--continue` (fork, and handing a claude run over) verified on codex-cli 0.155; not exercised by `batch` or `eval`. |
| `gemini` | your Google login | `gemini -o json` (single JSON result, no live stream). | Not yet run for real. |
| `opencode` | whatever opencode is configured with | `opencode run --format json`, prompt attached as a file. | `run` verified; not exercised by the later layers. |
| `agy` | your Antigravity plan (Google) | `agy --input-format stream-json --output-format stream-json`, the prompt as one stream-json event on stdin. Found on PATH or `%LOCALAPPDATA%\agy\bin` (Windows), or `~/.local/bin` elsewhere. | Parser tested against an anonymised agy 1.3.3 sample; `run` and `--continue` verified once each on agy 1.3.3 (2026-10-14); not exercised by `batch` or `eval`. |

"Verified" means a real agent did real work through that path and the result was checked
by hand, not that a test passed. Treat the unverified rows as drafts that need one run and
probably one fix.

Without preferences, the default backend is `api` when credentials exist, otherwise
`claude`, or the route table's row for the prompt's purpose, when one exists.

### Choosing which backends hrn may use

The [Backends card in hrn watch](watch.md#backends) can also change the enabled backends and default.

Set the enabled backends once for runs, MCP calls, batches, eval cells and judges,
and the supervisor protocol:

```bash
hrn backends set --only claude,codex --default claude
hrn backends
hrn backends reset
```

`set` validates names against the backends registered in this hrn binary and writes
`$HRN_HOME/backends.json` (normally `~/.hrn/backends.json`) atomically:

```json
{"enabled":["claude","codex"],"default":"claude"}
```

For a build that registers `agy`, use `--only claude,agy` instead. A name need not
be installed to be enabled, but it must be registered. `--default` is optional;
when supplied it must appear in `--only`. `set` replaces the whole preference.
`reset` removes the file, including a malformed one, and restores today's default
selection. A missing file or an empty enabled list with no default leaves all
backends enabled. A nonempty default without an enabled list is refused.

The configured default wins when installed; otherwise hrn chooses enabled `api`
when credentials exist, then the first installed enabled backend in preference
order, falling back to `claude`. If that fallback is disabled, the run is refused.
`hrn backends` shows enabled/disabled, the configured default (or the selected default
when none is configured), availability and remembered limits beside each backend.

An explicit disabled backend is refused before execution (`hrn run` exits 2).
Purpose routing ignores disabled rows with a warning and records no routing evidence.
Batch retry lists lose disabled entries while keeping their original order and
repetitions; a nonempty list with no enabled entries is refused.

At a usage limit the carry-on partner is the first other installed enabled backend
in preference order, subject to the existing readiness and billing checks. With no
preference, the claude/codex pairing is unchanged. With no ready enabled partner,
the run stays limited. `--no-switch` still disables carry-on.

The `agy` adapter sends one `{"event":"user","message":{"content":"..."}}` line on
stdin with `--input-format stream-json --output-format stream-json`, which selects
print mode without putting a long prompt in Windows' command line. The `antigravity`
IDE launcher is not the CLI. `-m` passes `--model` (use `agy models` to list IDs), and
all hrn effort levels pass through as `--effort`: low, medium, high, xhigh and max.
Usage comes from the result event; output tokens already include thinking tokens,
and input tokens exclude cache reads. A requested model is retained, but marked as
reported only if an event names one.

| hrn mode | agy flags |
|---|---|
| `readonly` | `--mode plan` |
| `ask` | `--mode plan` (no interactive approval) |
| `auto` | `--mode accept-edits` |
| `yolo` | `--dangerously-skip-permissions` |

These are agy's permission modes; `plan` is not a filesystem sandbox.

The `claude` backend uses the plan login only. If Claude Code's init line reports an
`apiKeySource` other than `none`, hrn stops the worker as soon as that init line arrives
and ends the run with an error. Claude Code may already have sent its first request.
Older CLI versions that omit the field keep their existing behaviour.

### Context management

A long run carries every tool result, and the cap is 30 KB per result, so the
conversation reaches hundreds of thousands of tokens. The api backend therefore asks
the server to clear old tool results itself (`clear_tool_uses_20250919`). hrn still
uploads its whole history, which stays append-only; what the clearing changes is the
context the model is given, so it is the context window and the billed input that
shrink, not the request body. A clear invalidates the cached prefix from that point,
which is the trade: one cold turn for a smaller context afterwards. That balance has
not yet been measured against a real long run, so compare `cache_read_tokens` across
two runs before trusting it. If the API ever refuses the beta, the run says so and
retries once without it. `hrn run --no-context-edits` turns it off for good, as do the
`no_context_edits` field on a batch spec or task and the MCP `run` argument.

Whatever the server reports about the edits it applied is recorded as an info event,
`server context management, turn N: {...}`, with the JSON exactly as it arrived: its
shape was not documented where this was written, so hrn keeps it rather than guessing at
it. Nothing about it goes back into the history.

### Lean claude workers

By default a claude worker starts the way Claude Code starts for you, with every MCP
server you have: your user-scope servers, the project's `.mcp.json`, the servers your
plugins bring and your claude.ai connectors. Their tool definitions are context on every
turn (#200). A lean worker starts with only the MCP servers in the run's `--mcp` files
(and `~/.hrn/mcp.json`): hrn adds `--strict-mcp-config`, and nothing else changes.

What a lean worker keeps: all of your settings (user, project and local), so your
`~/.claude/CLAUDE.md` and `~/.claude/rules`, the repository's `CLAUDE.md` and `.claude/rules`,
your hooks, your plugins and their skills and commands, your own skills, and your
subscription login. What it drops: every MCP server hrn did not pass, including the ones a
plugin or a claude.ai connector provides (the plugin's skills stay). hrn's guard is the same
either way: its `--disallowedTools` rules are command-line flags, not settings.

Loading `CLAUDE.local.md` under lean was not checked against Claude Code 2.1.284.
To check it, put a unique harmless instruction in `CLAUDE.local.md` in a temporary
repository, then compare a normal run with `hrn run -b claude --lean` using the same
prompt that exercises that instruction. These runs use your Claude plan; record the
Claude Code version and whether each run follows the instruction.

Lean is the default for `hrn batch` and `hrn eval` tasks that run on claude; a spec or task
sets `"lean": false` to give its workers every MCP server (a task's `false` wins over a
spec's `true`). The record of an attempt says `"lean": true` when the default applied. `hrn
run` and the MCP `run` tool stay as they were: lean only with `--lean` or `"lean": true`.
Claude Code's `--bare` is not used: it logs in only with `ANTHROPIC_API_KEY` or an
`apiKeyHelper`, never the subscription. The api loop never reads Claude Code's settings, so
lean changes nothing there; codex, gemini, opencode and agy say `lean: not enforced` when it is
asked for and run as usual (a batch's default does not ask them).

Measured on 2026-10-13 (Claude Code 2.1.284, claude-sonnet-5-5, the owner's setup, one
read-only question on this repository, two runs each; `context_tokens` is the context of the
last turn):

| Run | `context_tokens` | `cache_read_tokens` | `cache_write_tokens` | Turns |
|---|---:|---:|---:|---:|
| default | 61,617 | 88,349 | 34,724 | 2 |
| default | 61,874 | 98,866 | 24,721 | 2 |
| lean | 55,150 | 92,042 | 17,997 | 2 |
| lean, `--compact-window 200000` | 55,051 | 92,043 | 17,898 | 2 |

Lean took about 6.6K tokens (11%) off the context; the rest is the system prompt, built-in
tools, and the settings, rules and skills lean now keeps. Claude Code already defers most
MCP tool definitions until a tool is searched for, so the saving depends on how many
servers you have and grows with every turn of a long run. The compaction window does
nothing on a run this small; it accepted the value (`--autocompact 200000` in the
recorded command line) and the run succeeded.

Before #204, lean also passed `--setting-sources project`, which kept the user's
`CLAUDE.md`, rules, plugins and skills out as well; workers then lost the owner's rules,
which is why it was dropped.

### Compaction window

Claude Code compacts a conversation (summarises its history into one message) when the
context nears the model's window. On a 1M-token model that is only at about 967K tokens, so a
long run carries 300-400K tokens of context into every turn well before it compacts, and
each of those turns reads them back from the cache. `compact_window` sets the threshold
instead: `hrn run --compact-window 200000`, or `"compact_window": 200000` on a batch or eval
spec or task and on the MCP `run` tool. It takes an integer from 100000 to 1000000, or
`"auto"` (Claude Code's own choice), and is checked before the run starts; hrn passes it as
`claude --autocompact <value>`. Unset, nothing is passed.

The trade: every compaction is one expensive turn (the whole history is read to write the
summary), and the summary loses detail the model may later want (exact error text, a file
it read early). A lower window means smaller turns and more compactions. Set it for long
runs whose early history matters little; leave it unset for short ones, which never reach
it anyway. Only the claude backend applies it; the api loop (whose context editing does a
similar job, see above), codex, gemini, opencode and agy say `compact_window: not enforced`.

### Codex cost estimates

Codex costs are list-price estimates from the OpenAI table in
`internal/agent/prices_openai.go`, never a bill or subscription charge. The table
records its source URL and date and uses standard short-context rates; service
tiers, long-context rates and regional uplifts are not represented. Reported input
includes cached input: cache reads use the cached-input rate, the remaining input
uses the input rate, and output uses the output rate.

The model the run asked for (`-m` or the routing table) selects the price, with
the same dated-snapshot suffix rule as the Claude table. The adapter records no
CLI default model: a run with neither keeps cost marked `missing`, as does an
unknown model or missing usage. `gpt-6-sol`
is omitted because the pricing page fetched on 2026-10-15 did not list it.
Known models with reported usage carry `cost_is_estimate` and an `estimated`
usage cost status, including an explicitly reported zero.

### Continuing a run

A finished run can be picked up where it stopped, on the backend that ran it or on
another one:

```bash
hrn run --continue 20260921-095628-f18a6d "now fix the failing test"
hrn run --continue 20260921-095628-f18a6d -b codex "review what claude just did"
```

The follow-up is a new run whose `task.json` says which run it continues, so a
conversation is a chain of runs. It inherits the earlier run's backend, workspace and
mode unless you pass those flags, and its model only from runs on the same backend: a
claude model name means nothing to codex. The MCP `run` tool takes the same thing as
`continue_run_id`.

If the run whose conversation or session is carried on recorded more than 150,000
tokens of context at its last turn, hrn warns with its run id and context size and
suggests a fresh run with a short hand-over. This may be the named run or an earlier
run in the chain on the same backend; a transcript-only hand-over to another backend
gets no warning.
The CLI prints the warning to stderr before starting, including with `--json`;
the MCP tool includes it in its answer. Continuing is still allowed. Unknown
context sizes and sizes at or below the threshold produce no warning.

What the backend carries on from:

| Backend | Saves | A follow-up on it |
|---|---|---|
| `api` | its message history, `messages.json` | resumes that conversation |
| `claude` | the session id the CLI reports | `--resume <id> --fork-session` |
| `codex` | the thread id the CLI reports | `codex exec … fork <id>` |
| `agy` | the conversation id the CLI reports | `--conversation <id>` (resumes in place) |
| `gemini`, `opencode` | nothing yet | starts a new session |

A follow-up looks back along the chain for the latest run on its own backend that saved
something, carries on from that, and is handed the turns after it as a transcript at the
top of its prompt: each turn's request and final answer, not the tool calls or the
reasoning, oldest turns left out first past 32 KiB. With nothing saved in the chain it
starts fresh and gets the whole chain that way. So claude → codex → claude works: codex
starts fresh with claude's turn, and claude then forks its own session and is told only
what codex did. The run records the prompt as you typed it; what went before it is saved
beside the run as `handoff.md`, and the run's first info lines say what was picked up.

A claude or codex session is forked, never resumed in place, so every run keeps a session of its own
that nothing appends to later, and continuing the same run twice gives two separate
branches, the way api runs do. `hrn show` prints the session id, so a run can also be
opened by hand: `claude --resume <id>` from the run's workspace, or `codex resume <id>`.
A session is only forked in the workspace it ran in: claude finds a session from the
directory it started in, and the session refers to that workspace's files.

agy uses the same continuation hook but resumes the saved conversation in place:
continuing the same run twice appends to that conversation instead of creating two
branches. Its session capability is `resume`, and the continuation note says
`resuming`.

Continuing on the same backend refuses rather than quietly starting over when that
backend saved nothing: a run from before session ids were recorded, a gemini run, or an
api history that ended on a tool call the model never got an answer for, which the API
refuses to resume from. Batch attempts save their conversations too, so a failed attempt
can be picked up by hand.

Thinking blocks belong to the model that produced them. Continuing an api conversation
on a different model is allowed and warned about: the API serves it without the earlier
thinking.

### When a plan's usage limit is reached

Without preferences, the claude and codex CLIs stand in for each other at subscription
usage limits. With preferences, the partner follows the enabled list described above.
A run that ends because its CLI said the limit was reached ("You've hit your
session limit · resets 10:10pm (Europe/Bucharest)", "You've hit your usage limit ... try
again at Oct 16th, 2026 3:29 PM") is `limited`, not `error`, and its result keeps the
time the limit lifts as `limit_resets` when the message gives one. Then:

- The other CLI carries it on, once, as a follow-up run (see above): a new record that
  continues the limited one, with the transcript handed over, the same workspace and
  mode. A model is never carried from one backend to the other: the new backend runs the
  model chosen for it earlier in the same chain, as any follow-up does, or its default. `hrn run` and the MCP `run` tool answer
  with that run; the MCP result names the limited one as `limited_run_id`.
- The reset time is kept in `~/.hrn/limits.json`. Until it passes, a run, a batch
  attempt or a retry meant for that backend starts on the other one, and its transcript
  says why. A `--continue` follows the same rule. `hrn backends` (and the MCP `backends`
  tool) shows each limit it remembers. A limit can lift early (codex sells credits);
  deleting the file forgets it.
- Nothing moves automatically to `api`, which is billed per token. Without preferences,
  gemini, opencode and agy are not partners either; with one, the enabled installed plan
  backends stand in, in preference order. If no enabled partner is installed, or the
  selected partner is limited too, the run stays `limited` and says so.
- Nor to a CLI that would bill per token: with `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`
  or a `CLAUDE_CODE_USE_BEDROCK`/`_VERTEX`/`_FOUNDRY` switch set, claude prefers them to
  the plan, and so does codex with `CODEX_API_KEY` or `OPENAI_API_KEY`; that CLI does not
  stand in, and the run says which variable stopped it. hrn cannot see a CLI logged in with
  an API key in its own files (`codex login --with-api-key`, claude's `apiKeyHelper`): use
  `--no-switch` there.

The same mode is not the same permissions on both: `auto` lets codex run commands in its
sandbox but claude only edit files, and codex's `readonly` reads the whole disk (see the
[permission table](permissions.md#permission-modes)). A carried-on run's `--timeout` starts again, so the two runs together can
take twice as long. `--no-switch` (field `no_switch`) keeps a run on its backend, and on the
vendor it was meant for. `hrn eval` always sets it: a cell measures its own backend. `hrn
exec` never switches; its supervisor decides. Limits are recognised by the CLIs' wording,
which is matched in `internal/agent/limit.go` against messages seen in real runs; a new
wording is an `error` until it is added there. A remembered time more than eight days ahead
is ignored, as a CLI's would be.

### Project instructions

The api loop reads `HRN.md`, `AGENTS.md` and `CLAUDE.md` from the workspace root and
puts them in its system prompt, so it sees the same house rules the agent CLIs read
from their own files. Only the root is searched, the files are sent in that order, and
the total is capped at 32 KiB; the run says what it loaded and whether the cap cut
anything. A file that is a symlink out of the workspace is refused, like any other
path. `hrn run --no-instructions` skips them, as do the `no_instructions` field on a
batch spec or task and the MCP `run` argument of the same name.
