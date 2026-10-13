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

"Verified" means a real agent did real work through that path and the result was checked
by hand, not that a test passed. Treat the unverified rows as drafts that need one run and
probably one fix.

Default backend: `api` when credentials exist, otherwise `claude`.

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

By default a claude worker starts the way Claude Code starts for you: with your user and
local settings, so every plugin, skill, hook and MCP server you have installed, and your
own `~/.claude/CLAUDE.md` and rules. On a small task that start-up is most of the context
on every turn (#200). `hrn run --lean`, the `lean` field on a batch or eval spec or task,
and the MCP `run` argument start it with only:

- the MCP servers in the run's `--mcp` files (and `~/.hrn/mcp.json`):
  `--strict-mcp-config`, so no user-scope, project `.mcp.json`, plugin or claude.ai
  connector servers;
- the project's settings: `--setting-sources project`, so no `~/.claude/settings.json`
  or `.claude/settings.local.json`, and with them no user plugins, hooks or permission
  rules.

What a lean worker still gets: the repository's own `CLAUDE.md` (and the `AGENTS.md` it
imports with `@AGENTS.md`; Claude Code reads `AGENTS.md` only that way), the project's
`.claude/settings.json`, `.claude/rules`, skills and hooks, and managed (organisation)
settings and memory. What it no longer gets: your `~/.claude/CLAUDE.md` and
`~/.claude/rules` (Claude Code 2.1.284 loads user memory only when `user` is a setting
source), `CLAUDE.local.md`, your plugins and their skills, user skills, user hooks, and
every MCP server hrn did not pass. It still logs in with your subscription. hrn's guard
is the same either way: its `--disallowedTools` rules are command-line flags, not settings.

Claude Code's `--bare` is not used: it logs in only with `ANTHROPIC_API_KEY` or an
`apiKeyHelper`, never the subscription, and skips `CLAUDE.md` discovery. Lean is off by
default. The api loop never reads Claude Code's settings, so lean changes nothing there; the
codex, gemini and opencode backends say `lean: not enforced` and run as usual.

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
| `gemini`, `opencode` | nothing yet | starts a new session |

A follow-up looks back along the chain for the latest run on its own backend that saved
something, carries on from that, and is handed the turns after it as a transcript at the
top of its prompt: each turn's request and final answer, not the tool calls or the
reasoning, oldest turns left out first past 32 KiB. With nothing saved in the chain it
starts fresh and gets the whole chain that way. So claude → codex → claude works: codex
starts fresh with claude's turn, and claude then forks its own session and is told only
what codex did. The run records the prompt as you typed it; what went before it is saved
beside the run as `handoff.md`, and the run's first info lines say what was picked up.

A session is forked, never resumed in place, so every run keeps a session of its own
that nothing appends to later, and continuing the same run twice gives two separate
branches, the way api runs do. `hrn show` prints the session id, so a run can also be
opened by hand: `claude --resume <id>` from the run's workspace, or `codex resume <id>`.
A session is only forked in the workspace it ran in: claude finds a session from the
directory it started in, and the session refers to that workspace's files.

Continuing on the same backend refuses rather than quietly starting over when that
backend saved nothing: a run from before session ids were recorded, a gemini run, or an
api history that ended on a tool call the model never got an answer for, which the API
refuses to resume from. Batch attempts save their conversations too, so a failed attempt
can be picked up by hand.

Thinking blocks belong to the model that produced them. Continuing an api conversation
on a different model is allowed and warned about: the API serves it without the earlier
thinking.

### When a plan's usage limit is reached

The claude and codex CLIs bill to subscriptions with usage limits, and each stands in for
the other. A run that ends because its CLI said the limit was reached ("You've hit your
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
- Nothing moves to `api`, which is billed per token, or to gemini or opencode. If the
  other CLI is not installed, or is limited too, the run stays `limited` and says so.
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
