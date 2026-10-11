# hrn — a small agent harness

One Go binary that runs coding tasks on **your own agent loop** (Anthropic API) or on
**agent CLIs you already pay for** (Claude Code, Codex, Gemini, opencode), and records
every run in the same format so runs can be replayed, compared and scored later.

It speaks **MCP** both ways: the api loop can use any stdio MCP server's tools, and
`hrn mcp` turns hrn into an MCP server so Claude Code (or any host) can delegate tasks to
any backend, several at a time.

`hrn batch` is the orchestrator: N agents in parallel, each in its own git worktree on
its own branch, with build/test gates and retries on another backend.

**Zero dependencies.** Standard library only — `go.mod` has no `require` lines. The API
client (streaming SSE, retries, prompt caching, thinking round-trips) is ~400 lines in
`internal/agent/messages.go`; MCP (JSON-RPC 2.0 over stdio, client and server) is ~600
lines in `internal/mcp`. The CLI backends are optional programs hrn calls if they're
installed, not build dependencies.

```
hrn run -b opencode --mode readonly "find the bug in math.go"
→ glob **/math.go
  ✓ …/hrndemo/math.go
→ read …/hrndemo/math.go
  ✓ <path>...</path> (+8 lines)
Yes, `add()` has a bug: it subtracts `b` from `a` instead of adding them.
── ok · opencode · 3 turns · 6.5k in / 87 out (cache 11.9k read) · 8.5s ──
```

## Why Go

Answer quality comes from the model, not the harness language. What the harness
language decides is overhead and concurrency: a static ~7 MB binary with low idle memory,
cheap goroutines for parallel tool calls and for many agents at once, and no runtime
to install. Rust would shave a little memory at a large cost in iteration speed; Python
costs 3–5× the memory and needs an interpreter.

## Install

Go 1.26 or later, nothing else:

```bash
go install github.com/cristiangirlea/harness/cmd/hrn@latest
```

Or download a binary from the repository's releases: one static file per platform
(Linux, macOS and Windows, each on amd64 and arm64), named
`hrn_<version>_<os>_<arch>`, with a `SHA256SUMS` file beside them. Or from a checkout:
`go build -o hrn ./cmd/hrn` and `go test ./...`. CI tests on Ubuntu and Windows.

Releases are cut by pushing a `v*` tag that matches `agent.Version` in
`internal/agent/agent.go`: the release workflow runs the tests, builds with
`.github/release.sh`, and publishes. Run the script by hand first to check a release,
`bash .github/release.sh v0.2.0`, which writes the same files to `dist/`.

## Commands

| Command | What it does |
|---|---|
| `hrn run [flags] <prompt>` | Run a task. Prompt from args, or stdin when omitted / `-`. |
| `hrn exec --protocol v1` | Run one recorded task over the supervisor JSONL protocol (see [hrn exec](#hrn-exec)). |
| `hrn backends` | Show which backends are usable and where they were found. |
| `hrn version` | Print the hrn version, the Go version, and the commit it was built from (a `go install` build shows its module version instead; Go does not stamp builds made inside a `git worktree`). |
| `hrn runs [-n N]` | List recent runs. |
| `hrn runs prune --older-than 30d [--keep N] [--dry-run]` | Remove old runs, batch records and eval records (see [Run records](#run-records)). |
| `hrn show [--json] <id>` | Replay a recorded run. |
| `hrn show -f <id>` | Follow a run as it happens, until it ends (see [Watching runs live](#watching-runs-live)). |
| `hrn watch [--addr HOST:PORT] [--lan[=NAME]]` | A local page that follows runs and batches live (see [Watching runs live](#watching-runs-live)). |
| `hrn mcp` | Serve hrn as an MCP server on stdio (see [MCP](#mcp)). |
| `hrn batch [flags] <tasks.json>` | Run tasks in parallel git worktrees with gates and retries (see [Batches](#batches)). |
| `hrn batch show [--json] <id>` | Reprint a batch's table, while it runs or long afterwards. |
| `hrn batch diff <id> <task-id>` | What one task changed, as a diff on stdout. |
| `hrn batch merge [--squash] <id> [task-id…]` | Merge the branches of the tasks that passed. |
| `hrn batch rm <id>` | Remove the worktrees and branches a batch created. |
| `hrn eval [flags] <eval.json>` | Run a task set across a matrix of backends, models and efforts (see [Evals](#evals)). |
| `hrn eval show <id>` | Reprint a saved eval. |
| `hrn eval list [-n N]` | List saved evals, newest first: cells, pass rate, cost with the judge. |
| `hrn eval diff [--json] <a> <b>` | Two evals side by side, variant by variant and task by task. |
| `hrn report [--since 7d] [--by backend\|model\|dir] [--json]` | Tables over the finished runs and the batches (see [Reports](#reports)). |
| `hrn gc [--apply] [--grace 1h] [PATH…]` | Remove worktrees whose work is merged, with their build output (see [Clearing merged worktrees](#clearing-merged-worktrees)). |

Run flags: `-b/--backend`, `-m/--model`, `-C/--dir`, `--mode`, `--max-turns`,
`--effort` (api, codex and claude: low, medium, high, xhigh, max; unset uses the
backend default; gemini and opencode report it as not enforced),
`--json` (JSONL event stream for scripts), `--thinking`,
`--mcp FILE` (repeatable), `--no-instructions`, `--allow-dangerous`, `--no-switch` (see
[When a plan's usage limit is reached](#when-a-plans-usage-limit-is-reached)), and the
budgets below.

### hrn exec

`hrn exec --protocol v1` lets a supervising program start, follow, cancel and record
one run per process without parsing terminal output. stdin and stdout carry JSON
lines: hrn sends `hello` first, then accepts one `start`, streams complete events,
and ends with `result`. Closing stdin or interrupting hrn cancels the run. See
[the v1 protocol](docs/exec-protocol.md) for messages, capabilities, limits and exit codes.

### Budgets

Turns are a poor proxy for what a run costs: most of the spend is the context
re-sent every turn. Three ceilings, all optional and all off by default.

| Flag | Spec / MCP field | What it does |
|---|---|---|
| `--timeout 20m` | `timeout` | Ends the run when the deadline passes, on every backend. The result is `timeout`, not `canceled`, so a slow run is distinguishable from an interrupted one. |
| `--max-cost 2.50` | `max_cost_usd` | The api loop stops before a turn that would take its estimated cost past the limit. The result is `budget`, and the cost so far is still recorded. |
| `--task-budget 64000` | `task_budget` | Tells the api backend's model how many tokens it has for the whole task, so it paces itself and finishes rather than being cut off. Minimum 20000, checked before the run starts. |

The CLI backends cannot enforce a cost or token ceiling, so they say so once and
carry on; the timeout applies to them all the same. The cost ceiling is checked against
hrn's own estimate, so a model with no price in the table cannot be capped and the run
says so instead of pretending.

Agent CLIs and shell commands start in a Windows Job Object or a Unix process group.
Cancel or timeout kills that tree; CLI cleanup can take up to about 15 seconds while
stdout/stderr reads are bounded and remaining processes are checked (survivors or
unavailable Windows containment are reported). On Windows, a CLI's leftover background
processes end when the run ends, including after a normal exit, because the job is
closed. On Unix, background processes survive a normal exit and are killed only on
cancel or timeout; children can escape the group with setsid.

On Unix, Ctrl+C, SIGTERM and SIGHUP cancel runs. A process in the tree that reads the
terminal (such as a credential or host-key prompt) stops because the tree is not the
terminal's foreground process group.

On Windows, closing the console, logging off or shutting down reaches hrn as
SIGTERM. It has a few seconds to save the cancelled record while the job ends the
process tree. On Linux with procfs mounted with hidepid=2, a tree member owned by
another user (sudo or a setuid helper) is invisible to the check, so "gone" can be
optimistic.

Where a cost comes from depends on the backend. The claude and opencode CLIs report one
themselves; on a subscription that figure is what the run would cost at API list
prices, not a charge. codex and gemini report none. The API returns token counts only,
so the api backend prices them from a table in `internal/agent/api.go`: Anthropic's
first-party list prices per model, including the newer models' own cache-read prices,
with cache writes at 1.25x input. A dated snapshot id such as
`claude-haiku-4-5-20251001` is priced as its model. A model missing from the table has
no estimate, and the run says so at the start rather than showing a cost of zero.

## Backends

| Backend | Billing | How it runs | Status |
|---|---|---|---|
| `api` | per token | The harness's own loop: Messages API (streaming, adaptive thinking, prompt caching, server-side refusal fallbacks on `claude-opus-5`) + client-side tools below + MCP tools. Needs `ANTHROPIC_API_KEY` (or an OAuth `ANTHROPIC_AUTH_TOKEN`); `ANTHROPIC_BASE_URL` overrides the endpoint. | Every path is covered by tests against a fake API server; it has not yet run against the real API. |
| `claude` | your Claude subscription | `claude -p --output-format stream-json`. Found on PATH, `~/.local/bin`, npm, or the Claude desktop app's bundled build (that build needs its own `claude /login` first). | Verified end to end: `run`, `batch`, `eval`, and Claude Code delegating through `hrn mcp`. |
| `codex` | your ChatGPT subscription | `codex exec --json`. Found on PATH, npm, or the Codex desktop app's bundled build. | `run` and `--continue` (fork, and handing a claude run over) verified on codex-cli 0.155; not exercised by `batch` or `eval`. |
| `gemini` | your Google login | `gemini -o json` (single JSON result, no live stream). | Not yet run for real. |
| `opencode` | whatever opencode is configured with | `opencode run --format json`, prompt attached as a file. | `run` verified (the example above); not exercised by the later layers. |

"Verified" means a real agent did real work through that path and the result was checked
by hand, not that a test passed. Treat the unverified rows as drafts that need one run and
probably one fix.

Default backend: `api` when credentials exist, otherwise `claude`.

### Permission modes

| Mode | api backend | claude | codex | gemini | opencode |
|---|---|---|---|---|---|
| `readonly` | read tools only (write tools not offered) | `plan` | `--sandbox read-only` | `default` | its config |
| `ask` (default) | reads auto; writes + commands prompt y/N | `default` | `--sandbox read-only` | `default` | its config |
| `auto` | reads + edits auto; commands prompt | `acceptEdits` | `--sandbox workspace-write` | `auto_edit` | its config |
| `yolo` | everything | `bypassPermissions` | bypass sandbox | `yolo` | `--auto` |

When the prompt comes from stdin there is no terminal to ask, so anything needing
approval is denied.

### api tools

`read_file`, `list_files` (glob with `**`), `grep` (RE2), `write_file`, `edit_file`
(exact unique match, CRLF-aware), `run_command` (Git Bash when present, else PowerShell;
120s default timeout). All paths are confined to the workspace and judged where their links
lead: symbolic links, and on Windows directory junctions too (Go has not counted a junction
as a link since 1.23, so hrn reads its target itself). A path through a link out of the
workspace is refused, and so is one through a link hrn cannot read; a path through a link
that stays inside works. `grep` over a directory goes into no link or junction and passes
over a file link that leads out. Not yet on Linux and macOS: a symbolic link to a file that
does not exist yet is judged by its own name, so `write_file` on it creates that file where
the link leads ([#47](https://github.com/cristiangirlea/harness/issues/47)).
Read-only calls in one turn run in parallel; writes and commands run in order.

#### The command guard

`run_command` refuses seven kinds of command line before the shell sees them:

| Rule | Refuses |
|---|---|
| `git-push` | any `git` command mentioning `push`, including `git -C dir push`, or setting a `pushurl` or `pushInsteadOf` — a batch commits on its own branch; what leaves the machine is the operator's call |
| `guard-env` | a command that changes a variable of the guarded environment below (`GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n`/`PARAMETERS`, `GH_CONFIG_DIR`, `NPM_CONFIG_USERCONFIG`, `KUBECONFIG` and the other login places, `SSH_AUTH_SOCK`, gh's stand-in `GH_TOKEN` and `GH_ENTERPRISE_TOKEN`, the `HRN_GUARDED` marker): setting it (`NAME=`, `NAME+=`, `export`, `$env:NAME =`, `${env:NAME} =`, `read`, `printf -v`, `GIT_CONFIG_KEY_$i=` in a loop), unsetting it or hiding it from child processes (`unset`, `export -n`, `declare +x`, `typeset +x`, `env -u`, `SetEnvironmentVariable`), `env -i`, and any use of PowerShell's `env:` drive (or `Environment::`) as a path on one of them or with a wildcard (`Remove-Item env:X`, `rm env:X`, `& {Set-Item env:X …}`), whatever the cmdlet, reads through that path (`Test-Path env:X`) included; any case. Reading one as a variable (`$X`, `$env:X`, `${env:X}`), searching for it, or a flag like `--kubeconfig` or `--read-only` is fine |
| `secret-file` | a command line naming a secret file (the list below) as an argument, a redirect's target or a flag's value: `cat .env`, `echo x > .env`, `cp .env.example .env`, `Get-Content .\.env`, `cat ~/.ssh/id_ed25519` |
| `sudo` | `sudo` — a password prompt hangs a run with no terminal |
| `rm-root` | `rm -r` whose target is a root: `/`, `/*`, `~/`, `"$HOME"`, `C:\`, `C:/`, `/c/` |
| `pipe-to-shell` | `curl`/`wget` piped into a shell, however many stages the pipe has |
| `disk` | `mkfs`, `format X:`, `dd of=/dev/…` |
| `gh-publish` | `gh pr merge`, `gh release create/upload/edit/delete`, `gh repo delete/archive/rename` or `--visibility`, and `gh api` with PUT, PATCH or DELETE; opening a pull request stays allowed |
| `gh-token` | `gh auth token` (with `--user` too), `gh auth git-credential` and `gh auth status --show-token`/`-t`: gh keeps its login in the system's credential store, which no variable hides, and these hand it out |
| `deploy` | `wrangler deploy`, `npm`/`pnpm`/`yarn`/`bun`/`cargo publish`, a project's deploy script (`npm run deploy`, `deploy:prod`, `deploy-docs`, with `pnpm`, `yarn` or `bun` too), `deploy/push.sh`, `kubectl apply/delete/replace/patch/rollout`, `docker push`, `terraform`/`tofu apply/destroy`, `helm install/upgrade/uninstall`, `vercel --prod`, `fly`/`netlify deploy` |

The api backend's file tools refuse the secret files as well (rule `secret-file`):
`read_file`, `write_file`, `edit_file` and `grep` on a `.env*` file, `.dev.vars`,
`*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `.git-credentials`,
`.netrc`/`_netrc`, `.npmrc`, `.pypirc`, anything inside a `.private`, `.ssh`, `.aws`,
`.kube` or `.azure` directory, and the logins that are secret only where they sit: gh's
(`.config/gh/`, `GitHub CLI/` on Windows), gcloud's, `.docker/config.json`,
`.cargo/credentials*`, wrangler's `.wrangler/config/` (a worker project's own `.wrangler`
holds local state and stays open), and the agent CLIs' plan logins
(`.claude/.credentials.json`, `.codex/auth.json`, `.gemini/oauth_creds.json`). A path is
judged inside the workspace, both as written and where its links lead, Windows directory
junctions included (`notes.txt -> .env` is `.env`), with Windows' other spellings of a
name (`.env.`, `.env::$DATA`, a short `8.3` name) read as the name; `grep` over a
directory passes over secret files, and a link among them that leads to one.
`.env.example` is refused too: a template costs the agent one turn, and `.env.staging`
or `.envrc` are what the pattern is for. The command rule reads words (and quoted paths,
and git's `REV:path`), so `echo "*.pem" >> .gitignore` is refused as well (`edit_file` on
`.gitignore` is not), and so is a `jq '.key'` filter.

`rm-root` reads the arguments rather than matching one pattern, because the spelling
that matters is not the famous one: `rm -rf /` is refused by `rm` itself without
`--no-preserve-root`, while `rm -rf /*` and `rm -rf "$HOME"` are not.

The refusal comes back to the model as a failed tool call naming the rule, so the run
continues and the transcript says what was stopped. It is decided before any approval
prompt, so `ask` mode never asks for a yes it would then ignore. Matching is textual and
leans towards refusing too much: `echo "git push"` and `git commit -m "push later"` are
refused as well, which costs a turn and is the trade this makes on purpose.

This is a guard rail, not a sandbox — an agent that wants around a pattern can get
around it. What decides whether commands run at all is the [permission mode](#permission-modes);
this stops the plausible accident. `--allow-dangerous` (task field `allow_dangerous`,
also a batch spec/task field and an MCP `run` argument) lifts it for a whole run, and the
system prompt tells the model which way it is set; `yolo` on its own does not lift it.
The hrn MCP server refuses `allow_dangerous`, and does not offer it, unless you started it
with `hrn mcp --allow-dangerous-runs`: its caller is an agent, perhaps one under the guard
whose CLI loaded it (the claude CLI loads your user-scope servers, hrn's among them), and
whether a CLI passes its environment on to the servers it starts is that CLI's business.
An hrn that does see the guard refuses it too: the guarded environment sets `HRN_GUARDED`
(it reaches codex's commands, checked live), the api loop sets it for its own MCP servers,
and `hrn run --allow-dangerous` from an agent's shell is refused. A refused command tells
the model only that the operator can lift the guard.

Two things it does not cover, by design: tools from an MCP server are the server's
business, and batch gate commands are not checked, because the operator wrote those.
The agent CLIs run their own shells; the next section is their guard.

#### The guard for every backend

A command line can be spelled many ways, so every backend also runs under a guarded
environment. A CLI backend (claude, codex, gemini, opencode) inherits it, and so does
every command the api backend's `run_command` runs:

- tokens, keys, secrets and passwords are left out, by name (`GH_TOKEN`,
  `CLOUDFLARE_API_TOKEN`, `AWS_SECRET_ACCESS_KEY`, `NPM_TOKEN` …) and by pattern
  (`*_TOKEN`, `*_KEY`, `*_PAT`, `*_AUTH`, `*SECRET*`, `*PASSWORD*`, `*CREDENTIAL*`,
  `*PRIVATE_KEY*`, Terraform's `TF_TOKEN_*`, 1Password's `OP_SESSION_*`,
  `BW_SESSION`), model API keys among them: a command has no use for them, and a CLI
  that finds one bills per token instead of using its own login. Git Credential
  Manager's `GCM_*` settings stay: they say where its store is, not what is in it, and a
  private fetch needs them. git's askpass helpers
  go too (`GIT_ASKPASS`, `SSH_ASKPASS`, VS Code's `VSCODE_GIT_*`), since they hand out
  a login when git asks for one. `CLAUDE_CODE_OAUTH_TOKEN`, claude's plan login,
  reaches the claude CLI and no other;
- the logins tools keep on disk are looked for in an empty directory of the run's own:
  gh (`GH_CONFIG_DIR`), npm (`NPM_CONFIG_USERCONFIG`), kubectl (`KUBECONFIG`), docker
  (`DOCKER_CONFIG`), aws (`AWS_SHARED_CREDENTIALS_FILE`, `AWS_CONFIG_FILE`), gcloud
  (`CLOUDSDK_CONFIG`) and az (`AZURE_CONFIG_DIR`); the directory goes when the run ends.
  gh also keeps its login in the system's credential store (Windows Credential Manager,
  the macOS Keychain) and asks it when no token is set, so `GH_TOKEN` and
  `GH_ENTERPRISE_TOKEN` are set to a token that is not one;
- the ssh agent is out of reach (on Windows `SSH_AUTH_SOCK` names a pipe nobody
  listens on, since Windows' ssh otherwise falls back to its agent's own pipe);
- git points every push at `push-refused-by-hrn://`, which it cannot reach, through
  `GIT_CONFIG_*` (your own `GIT_CONFIG_*` entries stay): the usual prefixes and an empty
  one that catches the rest (scp-style aliases, shorthands, upper case, local paths),
  so `git push`, `git -C dir push` and `bash -c "git push"` fail at once without
  touching the network. Fetching keeps its address.

The claude backend also gets the guard as Claude Code's own deny rules
(`--disallowedTools`), which hold in every permission mode, `bypassPermissions`
included:

- the publishing and deploying commands of the table above, in both its Bash and
  PowerShell tools, and the commands that would steer a push around the environment
  (`git config …push…`, `git -c …push…`, `git remote set-url --push`, `unset
  GIT_CONFIG…`, `env -u`); a test keeps the two lists refusing the same commands;
- the secret files, with a `Read` rule and an `Edit` rule each, since a Read rule does
  not stop a Write that creates the file: the same list as the api backend's, anywhere
  on the disk, the agent CLIs' plan logins among them (the CLIs read those with their
  own code, not through these tools).

The command rules match command text, so the environment is what stops the other
spellings; what they add is an early, plain refusal and the secret files. Checked live
against the claude CLI in `yolo`, all refused by the rules: `git push` with and without
arguments, setting a `pushurl` or a `PushInsteadOf`, `npm run deploy:prod`, `gh api …
-X DELETE`; creating `.env.production` with the Write tool, editing `.env` with the Edit
tool, and writing secret files from Bash (`echo X=3 > .env.staging`, `printf … >> .env`,
`cp .env .env.bak`); reading `.env`, gh's `hosts.yml`, a `.private/` file and a `*.key`
(with the Read tool and with `cat`). `git -C . push`, which no rule matches, failed at
the environment's dead end, and the remote stayed empty. A PowerShell `Set-Content` of
`.env.test` was refused too, but by Claude Code's working-directory check rather than a
deny rule, so that path is not shown to hold by the rules alone.

What the guard does not stop, said plainly. It is a guard rail, not a sandbox:

- an agent that sets out to undo the environment can: a `pushurl` or a longer
  `pushInsteadOf` in the repository's own config, a `git -c`, or an unset
  `GIT_CONFIG_COUNT` gives git its address back, and git's credential helper then signs
  the push in (Git Credential Manager keeps its login in the system's credential store,
  where no variable reaches). Both guards refuse the plain spellings of those commands;
  the api backend's `guard-env` reads text, so a spelling it has no pattern for gets past
  it (a relative name after `Set-Location env:`, `GIT_CONFIG_KEY_"$i"=` with the index
  quoted, `declare -g +x`).
  The credential helper itself is left alone, because fetching private repositories and
  Go modules needs it;
- an ssh key file on disk without a passphrase still works for an ssh remote;
- logins kept where no variable moves them: wrangler's and cargo's, so under codex,
  gemini or opencode a `npx wrangler deploy` or `npm run deploy` would still deploy;
  claude and the api backend refuse those commands, the others get the environment only;
- gh's login in the system's credential store: `gh auth token --user NAME` reads it
  whatever `GH_TOKEN` says. Claude and the api backend refuse `gh auth token`; under
  codex, gemini or opencode an agent that runs `gh auth token --user NAME` gets your
  GitHub token (plain `gh auth token` gets the stand-in, which reaches codex's commands);
- secret files stay readable and writable under codex, gemini and opencode; under the
  api backend and claude, a command that opens one without naming it (a recursive
  `grep -r` from the shell, a tool that loads `.env` itself) is not seen, and nor is a
  login named by a relative path after a `cd` (`cd ~/.codex && cat auth.json`), since
  the logins that are secret only with their folder are matched in the command's text;
- secrets inside ordinary variables stay: a `DATABASE_URL` with a password in it, since
  local tests need it;
- a CLI that logs in with an API key (opencode or gemini with a key, claude on Bedrock or
  Vertex) needs `--allow-dangerous`, which lifts the push guard as well;
- under the guard no backend can push or open a pull request itself: handing work
  over is hrn's (batch keeps a branch per task) or the operator's.

Every run says whether the guard is on, names the variables it withheld, and says what
its backend leaves open: on codex, gemini and opencode, the secret files, the wrangler
and cargo logins, and gh's login in the credential store. `--allow-dangerous` lifts all
of it.

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
table above). A carried-on run's `--timeout` starts again, so the two runs together can
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

## MCP

### Using MCP servers as tools

`hrn run --mcp .mcp.json ...` starts every stdio server in the file, offers its tools to
the model as `mcp__<server>__<tool>` (Claude Code's naming) and stops the servers when
the run ends. `~/.hrn/mcp.json` (or `$HRN_HOME/mcp.json`) is loaded whenever it exists;
`--mcp` files add to it, and a later file overrides an earlier server of the same name.
The format is Claude Code's, so existing `.mcp.json` files work unchanged:

```json
{"mcpServers": {"github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
                           "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}}}}
```

`${VAR}` and `${VAR:-default}` expand from the environment. Entries with a `url` (HTTP
transports) are skipped: hrn speaks stdio only. Servers run with the workspace as their
working directory and are told it is the root.

Permissions follow the mode table. A tool the server marks `readOnlyHint` is a read:
auto-approved and run in parallel. Anything else needs approval in `ask` and `auto` and
is automatic only in `yolo`; in `readonly` mode it is not offered at all. The hint is
trusted because you chose the server.

Per backend: `api` uses the servers directly; `claude` receives each file as
`--mcp-config`; codex, gemini and opencode keep their own MCP configuration.

The config paths are recorded in the run's `task.json`, so a run can be replayed with
the same tool set.

### hrn as an MCP server

`hrn mcp` serves hrn on stdio. Register it once with Claude Code, from the project you
want it to work in by default:

```bash
claude mcp add hrn -- hrn mcp
```

The default workspace is the server's working directory, or `--dir PATH` (an absolute
path; a relative one would resolve against the host's working directory, the very
thing the flag avoids) for hosts that do not start servers in the project folder.
Cursor (`.cursor/mcp.json`) and VS Code (`.vscode/mcp.json`) take the same command
and args.

The server refuses `allow_dangerous` on `run` and `batch`, and leaves it out of `run`'s
arguments, unless it was started with `--allow-dangerous-runs`: the host that calls it is
an agent, and lifting [the guard](#the-guard-for-every-backend) is yours to decide.

Tools:

| Tool | Arguments | Returns |
|---|---|---|
| `run` | `prompt`, `backend`, `model`, `dir`, `mode`, `max_turns`, `effort`, `mcp`, `no_instructions`, `no_context_edits`, `allow_dangerous` (with `--allow-dangerous-runs` only), `continue_run_id`, `timeout`, `max_cost_usd`, `task_budget`, `no_switch` | Blocks until the task finishes: final answer, summary line (status, tokens, cost, duration) and run id. Tool calls stream back as progress. A run carried on at a usage limit answers with the run that carried it on, and `limited_run_id`. |
| `runs` | `limit` | Recent runs, newest first. |
| `show` | `run_id`, `events` | Task, event transcript and result of one run. |
| `backends` | — | Which backends are usable here. |
| `batch` | `spec` | Runs a batch spec, the same JSON as a batch file, and blocks until every task is done: one line per task (status, backend, attempts, the gate that failed, branch and commit) and the batch id. Task outcomes stream back as progress. |
| `batch_show` | `batch_id`, `task`, `limit` | No id: recent batches. An id: its tasks. An id and a task: that task's diff and its untracked files. |
| `batch_merge` | `batch_id`, `tasks`, `squash` | Merges the passing branches into the repository's current branch, stopping at the first conflict with the tree put back. |

Requests are served concurrently, so parallel tool calls from the host become parallel
agents, each recorded under `~/.hrn/runs` like a CLI run. Cancelling the tool call cancels
the run. `run` defaults to `readonly`; `auto` allows file edits and `yolo` also allows
commands.

There is no terminal behind the server, so `ask` mode asks the host instead: each write
or command goes back to the person through MCP elicitation, one question at a time, and
a run waits up to five minutes for each answer. The prompt names the run and carries the
whole command or path being approved, because a truncated one can hide the part that
matters, and the decision goes back to the host as progress. Anything other than a clear
yes is a no, including a declined prompt, a cancelled one, an error, or a host that does
not support elicitation at all. Two runs in ask mode at the same time ask independently,
which is why the prompt says which run it is from. That makes `ask` a real mode in the
place hrn runs most.

The batch tools let a host fan work out without leaving the conversation: write a spec,
call `batch`, read the diffs with `batch_show`, and land what passed with `batch_merge`.
A spec's relative `dir` is taken from the server's workspace. Unknown fields are refused
rather than ignored, because the spec is written by a model and a misspelt `gates` would
otherwise run with no gate. The structured result is a short summary rather than the
whole record, since some hosts, Claude Code among them, show it to the model in place of
the text. Worktrees stay after a merge; `hrn batch rm <id>` removes them.

## Batches

`hrn batch tasks.json` runs every task in the file against one repository, `parallel`
agents at a time, each in its own git worktree on its own branch. After an agent
finishes, the gate commands run in that worktree; a task whose run or gates fail is
retried on the next backend in `retry`. Every attempt is an ordinary recorded run.

```json
{
  "parallel": 3,
  "backend": "claude",
  "mode": "yolo",
  "gate": ["go build ./...", "go test ./..."],
  "retry": ["opencode"],
  "tasks": [
    {"id": "fix-add", "prompt": "add() in math.go subtracts. Fix it and add a test."},
    {"prompt": "Make list_files skip .venv directories.", "backend": "api", "gate": ["go test ./internal/tools"]}
  ]
}
```

`dir` (default: the file's directory) is any path inside the repository. Task fields
override the file's defaults of the same name; `id` defaults to `t1`, `t2`, ….

Every option `hrn run` takes as a flag is a spec field under the same name with
underscores (`--max-turns` is `max_turns`, `--max-cost` is `max_cost_usd`), on the spec
or on a task; the MCP `run` tool takes the same names. They are one struct in the code
(`agent.Options`), so the three cannot drift apart. A task's value replaces the spec's;
a switch a task turns on stays on; a task's own `mcp` list replaces the spec's, and an
empty list (`"mcp": []`) means no servers at all, not even `~/.hrn/mcp.json`. Every list
is checked before the first task starts.

For each task hrn creates branch `hrn/<id>` and worktree `.hrn/worktrees/<id>` from the
current `HEAD` (uncommitted changes in the main worktree are not included), runs the
agent there, then the gates in order, stopping at the first non-zero exit or after 15
minutes. Whatever the agent left is committed on the branch as `<id>: <prompt>`, pass or
fail, so a worktree can be inspected, merged or removed without losing work; if that
commit fails (no git identity, a pre-commit hook) the task ends as `error` and the work
stays uncommitted in the worktree. hrn writes `.hrn/.gitignore` so the worktrees never
show up in the repository's status.

A task can also set `expect`, a regular expression (Go RE2) its final answer must match.
It is checked before the gates and reported as one, `expect /pattern/`, so a task whose
answer is the deliverable can fail on what it said: `"expect": "(?i)^no\\b"` for "is
there a race here? answer yes or no". A retry after a miss is shown the answer it gave.

A failed attempt is retried on each backend named in `retry`, in order. The same name
may repeat, and `"retry": ["claude"]` after `"backend": "claude"` is the common case: a
second try with the same model, told what went wrong. How the retry starts depends on
how the attempt failed.

| Previous attempt | Next attempt |
|---|---|
| ran to the end, a gate failed | continues in the same worktree, on top of the work already committed, with the failing gate command, its exit code and output appended to the prompt |
| stopped at its plan's usage limit | continues in the same worktree, told that the limit stopped it and shown its last message |
| failed, refused or was cut short | starts in a fresh worktree from the base commit, with one line saying how the previous attempt ended |

An attempt that ends `limited` on claude or codex is carried on by the other one first, as
an extra attempt that does not use up a `retry` entry, and an attempt meant for a backend
whose limit is remembered starts on the other one (see [When a plan's usage limit is
reached](#when-a-plans-usage-limit-is-reached)). When both are limited, the attempt is
skipped, and a task with nothing left to run fails without starting an agent, its error
saying why. `no_switch` turns this off.

A continued attempt commits as `<id>: <prompt> (attempt N)`, so the branch reads as a
sequence of fixes. Each attempt records the prompt it was actually sent, whether it
continued, and `switched_from`, the backend a usage limit moved it from, which `hrn batch
--json` prints.

`hrn batch` refuses to start when tracked files in the repository are modified: every
worktree branches from `HEAD`, so that work would not reach the agents and the batch
would quietly be testing something else. Commit or stash first, or pass `--allow-dirty`
(spec field `allow_dirty`). Untracked files are not counted.

Nothing can answer approval prompts in a batch, so `mode` defaults to `auto` (file edits
allowed, no commands). Use `yolo` when the agent must build or run tests itself; the
gates run either way. Backend names, gate commands, durations and task budgets are all
checked before anything runs.

The budgets above are spec fields too (`timeout`, `max_cost_usd`, `task_budget`), and
they apply per attempt rather than per task, so a retry gets its own allowance. A task
may set its own value or change the spec's, but zero means inherit, so it cannot remove
a limit the spec set.

Each gate command gets 15 minutes before it is killed with its whole process tree;
`gate_timeout` (e.g. `"90s"`, `"45m"`, at most `24h`) changes that on the spec or on one
task, so a slow integration suite and a quick lint can share a batch. A killed gate fails
with `[timed out after …]` in its output, which a retry then sees.

The record goes to `~/.hrn/batches/<id>.json`, written before the first task starts,
again each time a task moves on (an attempt's run starts or ends, a gate finishes, a task
ends) and at the end, so `hrn watch` and `hrn batch show` can follow it; `--json` prints
it, `-v` streams every agent's tool calls. Ctrl+C
cancels running agents and skips the rest. Exit status is 1 unless every task passed.

### Reading and landing the work

```
hrn batch show 20260922-164137-1a216b     # the same table, during the run or after it
hrn batch diff 20260922-164137-1a216b alpha
hrn batch merge 20260922-164137-1a216b
```

`show` reprints the end-of-run table from the record; a batch still going shows its
tasks as `running` and says how many. The record is written before the first task and
renamed into place, so reading it mid-run gets a whole file, never half of one.

`diff` writes one task's changes to stdout and nothing else, so it pipes into `git
apply` or a pager: the commit against the base the batch branched from, or whatever is
still uncommitted in the worktree when the task never committed. Files git has never
seen cannot appear in a diff, so they are listed on stderr instead.

`merge` merges the branches into whatever the repository has checked out, in spec
order, and never pushes. It takes every task that passed, or the ones you name; a task
that failed its gates is skipped even when named, so nothing unproven arrives by
accident. The tree must be clean first, since the merge would otherwise mix your work
with the agents'. At the first conflict it puts the tree back as it found it, says
which task, and stops — the branches are still there to merge by hand. `--squash`
records one commit per task, `<id>: <prompt>`, instead of a merge commit. Undo a whole
run with `git reset --hard ORIG_HEAD`.

`hrn batch rm <id>` removes the worktrees and branches of a batch (it refuses a worktree
with uncommitted changes); the record and the run logs stay. A directory git no longer counts
as a worktree (on Windows a file lock can leave one behind) is removed when it is empty and
reported when it is not, and its branch is deleted either way. One task that cannot be cleaned
does not stop the others.

## Clearing merged worktrees

```
hrn gc E:\work            # what would go, and why each worktree stays or goes
hrn gc --apply E:\work    # remove them
```

Worktrees outlive their pull requests: the session that made one rarely comes back, and with
a Rust `target/` or a `node_modules` inside, each can hold gigabytes. `hrn gc` looks at every
linked worktree of the repositories at PATH (a repository, any folder inside one, or a folder
whose repositories are found two levels down; the current directory by default) and removes a
worktree only on proof that its work is merged:

- **On a branch:** GitHub (`gh pr list`) reports no open pull request for the branch, and one
  merged at the worktree's commit, or at a later commit that contains it, more than `--grace`
  ago (default 1h, so a session finishing up after its merge keeps its folder).
- **Made by an hrn batch** (a batch record names the folder): the batch has no task still
  running, and the worktree's commit is on the branch origin names as its default
  (`git ls-remote --symref`), fetched by name just before.

And only when nothing would be lost with it: the tree is clean (untracked files count), its
ignored files are build output (`target`, `node_modules`, `dist`, `.venv` and the like, or any
folder carrying `CACHEDIR.TAG`, with no repository and no link out of the worktree inside, since
git on Windows deletes through a junction; an ignored `.env` or notes keep it, because `git
worktree remove` deletes ignored files; its `.hrn/` may hold only the worktrees git lists there), it is
not locked, it holds no worktree that stays and no repository, and the current directory is
not inside it. Everything else stays and the listing says why: an open or
closed pull request, none at all, commits after the merged one, a detached `HEAD`, uncommitted
files. When `gh` fails (not installed, not logged in) or the fetch does, the worktrees that
needed it stay, and while any batch record cannot be read every worktree stays (it may be the
record of a batch still running). Main checkouts are never touched.

The listing puts the removals first, with the size each one frees and the pull request's URL,
which is the record once the folder is gone. `--apply` checks each one again just before it goes
(its `HEAD`, branch, lock and files, ignored ones included) and leaves one that changed since the
scan, or that holds a worktree that stays, then removes it with `git worktree remove` (never
`--force`) and deletes the branch if it still points at the commit that was checked. A batch's record stays (`hrn runs prune` can drop
it once its worktrees are gone). Open pull requests are read on their own, so a long history
cannot hide one; with 1000 or more open, no worktree on a branch is removed. Up to 1000 pull
requests of every state are read besides; a branch whose pull request is older stays.

## Watching runs live

```
hrn watch                  # http://127.0.0.1:8089, this computer only
hrn watch --lan            # also on this computer's private network addresses, for a phone
hrn watch --lan="Ethernet 3"   # only that adapter's network (a name or an address)
hrn show -f <run-id>       # the same timeline in the terminal
hrn bot                    # Ada, the bot, in a small window: what hrn is doing, out loud
```

`hrn watch` serves a page that lists the newest runs and batches (id, backend, status,
duration, cost, the prompt's first line) and keeps itself current. Every attempt of a
batch's task is also a run; its row links to the batch under the run's id, even when that
batch is older than the ones listed. A run's page is its
timeline as it happens: the assistant's text, each tool call with its input in one line
(the input itself folded under it, its first 6 KB and last 2 KB), each result folded and
capped (its first 12 KB and last 4 KB; all of it stays in `events.jsonl`), how long each
step waited, and, once the
run ends, its status, tokens, cost and final answer (and, for a `limited` run, when the
limit lifts). A batch's page shows each task with a link to each attempt's run, the
backend a usage limit moved it from, and its gates (command, exit code, folded output) as
they finish. Steps arrive over Server-Sent Events that check the run's `events.jsonl` twice a
second; a browser that loses the connection picks up where it left off. Cost is whatever
the backend reports: the api backend's estimate (`~`), the claude and opencode CLIs' own
figures, nothing for codex and gemini, and only once the run has ended.

Everything from a record is escaped as text, since tool results hold whatever the agent
read. The page loads nothing from anywhere else (one stylesheet and a script or two, all
served by hrn, no fonts), follows the system's light or dark theme, and on a phone shows
each run and batch as a small card instead of a table. It cannot start, stop or merge
anything: a request other than GET or HEAD must carry hrn watch's key in
`X-Hrn-Watch-Key`, from this computer or the network alike (a custom header is also the
CSRF check), and as no action exists yet, hrn watch sets no key and refuses them all.

It listens on a loopback address and refuses any other `--addr`, and it answers only
requests whose Host header names it, so a DNS name rebound to 127.0.0.1 cannot read it
through a browser. `--lan` also serves it on each private IPv4 address (192.168.x, 10.x,
172.16-31.x) of this computer's network adapters that are up, never on 0.0.0.0 or a public
address, and the terminal and the index list those links; on Wi-Fi, use the one on the
Wi-Fi adapter. `--lan=NAME` keeps one adapter, by its name in any case or by its address,
and a name that matches nothing is refused with the adapters there are. Point-to-point
links (most VPN tunnels) are left out, but a VPN that shows up as an ordinary adapter
with a private address is served too, and listed. Viewing needs no login: while `--lan`
runs, anyone on each listed network can read the runs. Windows Firewall asks once
whether hrn may listen; allowing Private networks is enough when the network is marked
Private.

For the phone, the index as seen from this computer shows each link with its QR code,
and with one network (`--lan=NAME`, or only one adapter) the terminal draws that code
too. The codes come from hrn's own encoder (`internal/qr`: byte mode, level M, versions
1 to 10), standard library only. A phone that came in over the network sees neither the
codes nor the hints meant for this computer.

`hrn show -f` prints the same steps in the terminal, with a line for each wait of a
second or more, and ends with the summary line once the run's result is saved. Ctrl+C
stops following and leaves the run alone. A run killed before it saved its result never
ends, on the page or in the terminal: hrn cannot tell it from a long step.

### Ada, the bot

`hrn bot` opens Ada, hrn watch's bot (`/bot`), in a small window of her own: Edge's or Chrome's
app window where one is installed, else the default browser. It serves hrn watch on
loopback for her until Ctrl+C, or, when an hrn watch already serves that address, just
opens her there. The index links to her too. Ada is a robot scarlet macaw: she blinks,
bobs her head and moves her beak while she talks, and keeps still for anyone whose system
asks for less motion.

Ada says what hrn is doing in plain UK English, in a line at the top, a log, out loud
and as desktop notifications:

| Kind | For example | Says it | Notifies |
|---|---|---|---|
| started | Claude has started in harness: add the budget guard. | yes | no |
| working | Claude is editing bot.go in harness. | no | no |
| task | Task two of four in tidedesk passed: fix the flaky test. | yes | no |
| done | Claude has finished in harness. It took four minutes. | yes | yes |
| failed | The batch in tidedesk is done: three of four tasks passed. It took twelve minutes. | yes | yes |
| status | Two things are going: Claude in harness, editing bot.go; and a batch of four tasks in tidedesk. | no | no |

Those are the defaults; the settings change each one, pick the voice and the speed, and
are kept in that browser. A done or failed notification stays until it is dismissed;
clicking one opens its run or batch. Skip (or Esc) stops talking; Snooze keeps it quiet for
15 minutes, an hour, or until unmuted; each run or batch going has a Mute this. "What an
agent is doing" comes from its latest tool call (reading, editing, searching, running a
command and the like), across the backends' different tool names. Ada opens quietly:
what ended before it was opened is not news, and the first thing it says is what is going.
A batch's own runs are told as the batch's, not twice.

Talk to me listens for one sentence each time it is pressed: "status" (or "what are you
doing?"), "skip", "quiet" ("for an hour", "until I unmute"), "unmute" and "repeat". These
steer the bot only; nothing it hears or shows acts on hrn, which waits for the
confirmations and audit log of #38.

The sentences are made by hrn (`internal/watch/bot.go`, where they are tested) and pass
through the same blanking of secret values as every page; the page only chooses which to
say. She speaks with the browser's own speech synthesis, which needs one click on the page
first ("Let me talk"). Voices on this computer come before online ones, since an online
voice sends what it says (prompts included) to its maker; among them British ones, then
a woman's, come first. Windows has British voices only with the English (United Kingdom)
speech pack (Settings, Time and language, Speech); without it the page says so and speaks
British English in the voice there is. Edge's online Natural voices (Sonia, Libby and
Maisie are British women's) sound far better and can be chosen in the settings, knowing
that each sentence then goes to Microsoft. Talk to me uses the browser's speech
recogniser, which in Edge and Chrome sends what the microphone hears to Microsoft or Google
while it listens. The microphone is allowed on the bot's page only. Opened over `--lan`,
Ada speaks, but the browser allows neither notifications nor the microphone on a plain
network address.

## Reports

`hrn report` reads every run and batch record and prints two tables.

```
runs by backend, since 2026-09-14 17:40 (7d)
backend  runs  ok  ok%  med dur  med turns  total cost  med cost  cache
claude      6   5  83%    1m2s          9      ~$1.03     $0.17    96%
api         3   3 100%     48s          6       $0.42     $0.14    71%
all         9   8  89%     55s          8      ~$1.45     $0.15    90%

batches by repository, since 2026-09-14 17:40 (7d)
repository              batches  tasks  pass%  med attempts  total cost
~/src/jobpilot                1      3   100%             1      ~$1.03
all                           1      3   100%             1      ~$1.03
```

Runs group by backend, or by `--by model` (the model that answered) or `--by dir` (the
workspace). Columns: runs, how many ended `ok`, median duration, median turns, total and
median cost, and the cache share (cache-read tokens over every input token, cache writes included;
`-` when the backend reported no token counts). A `~` marks a cost that is the api backend's estimate.
Batches group by repository; their cost sums every attempt, passed or not. Runs without
a result (still running or killed), batches with a task still running, and records that
cannot be read are counted under the tables and left out of them.
`--since` takes a window such as `7d`, `36h` or `90m`; `--json` prints the same rows as
JSON for scripts.

## Evals

An eval is a batch with two more dimensions: a matrix of variants to compare and a
number of repeats. Every cell is an ordinary recorded run, scored by the same gates, so
the question "which backend, model and effort for this kind of task" gets an answer with
numbers behind it.

```json
{
  "parallel": 3, "mode": "yolo",
  "gate": ["go build ./... && go test ./..."],
  "matrix": [
    {"name": "opus-high", "backend": "api", "model": "claude-opus-5", "effort": "high"},
    {"name": "opus-low",  "backend": "api", "model": "claude-opus-5", "effort": "low"},
    {"name": "claude",    "backend": "claude"}
  ],
  "repeats": 3,
  "timeout": "20m", "max_cost_usd": 3,
  "tasks": [{"id": "vet", "prompt": "Run go vet and fix every finding."}]
}
```

A variant's `name` defaults to `backend[-model][-effort]`. Each cell becomes a batch
task with the id `<evalid>.<task>.<variant>.r<n>`, so branches from different evals never
collide. Retries are off for every cell on purpose: an eval measures what one attempt
does, and a retried attempt would score the harness rather than the backend. Everything
else, gates and mode and the budgets, carries through from the spec.

`hrn eval eval.json` prints what each variant did and how each task scored under it:

```
  variant                pass  attempts  median cost median time   turns   tools   context
  sonnet                   0%         1        $0.15        13s       2       1    101061
  haiku                  100%         1        $0.05        14s       2       1     71814

  task                     sonnet      haiku
  greet                       0/1        1/1
── eval cb867 · batch 20260922-164017-1bbd7c · 2 cells · 14s ──
```

That run is real: asked for a file containing exactly `hrn rocks`, Sonnet wrote `hrn rocks.`
and failed the gate while Haiku passed at a third of the cost. The run records are still
there to say why, which is the point of keeping them.

`tools` is the median number of tool calls, counted the same way on every backend; a run
that takes eight greps to read one file shows up here and not in `turns`. Under a variant,
a dim line gives the run statuses when any cell did not finish `ok`, and how many passes
changed nothing: right for "find the bug, if there is one", suspicious for "fix the
typos".

The record goes to `~/.hrn/evals/<id>.json`; `--json` prints it, `hrn eval show <id>`
reprints it, `hrn eval list` finds it, `-v` streams every agent's tool calls. The worktrees are removed once the
record is safely written, unless `--keep`. A failing variant is data, not an error, so
the exit status is 0 whenever the eval itself ran.

Task ids and variant names take letters, digits, `_` and `-`: dots separate the parts of
a cell id. `retry` is refused in an eval spec, because a retried cell would score the
harness rather than the backend, and for the same reason an eval sets `no_switch`: a cell
whose backend reaches its usage limit ends `limited`. The `context` column counts prompt tokens and cache
reads together, which is what the model was given rather than what was billed at full
rate.

### The judge

Gates prove the build and `expect` proves an answer; neither can tell a typo fix from a
spelling change. A `judge` reads each passing cell's diff against a rubric and scores it
from 0 to 10:

```json
"judge": {
  "backend": "claude", "model": "haiku",
  "rubric": "Only real misspellings are fixed. British spellings such as colour stay.",
  "rubrics": {"notes": "The answer is correct and the repository is unchanged."}
}
```

`rubric` applies to every task and `rubrics` replaces it for the tasks it names; every
task needs one or the other, and both are checked before any agent starts. The judge is
shown the task, the rubric, the agent's final answer and the diff (the first 60 KB, and
the names of untracked files), and runs read-only in the cell's worktree so it can open
a file the diff only touches. `backend` defaults to the default backend, `timeout` to 5
minutes per verdict. Cells that failed are not judged: they already scored where it
counts, and judging them would only spend money.

The scores join the table as a `judge` column per variant (the mean) and after each
task's pass count (`1/1 10.0`). Every verdict is in the record under `judgements`, with
its reason and the run id of the judge's own recorded run; a judge that fails or answers
without a `{"score": ...}` object is recorded as an error rather than a zero, and the
footer counts those. On the `claude` backend the first verdicts cost more than the
answer: Claude Code writes its own system prompt to the cache in the read-only mode the
judge uses, about 64k tokens, once per judge that starts before the cache exists.

### Comparing evals

The records are kept so a change can be scored: run the same task set before and after
a new model, a reworded prompt or a harness change, then `hrn eval diff <before> <after>`.
Variants are matched by name and tasks by id; every cell reads before → after, and a
variant only one side ran gets its own row instead of disappearing. When a task's prompt
differs between the two specs, a note says so: often that is the change being measured,
but those rows do not compare like with like.

```
  eval cb867 → 847f6

  variant              pass             median cost        median time
  sonnet               0% → 100%        $0.15 → $0.14      13s → 13s
  haiku                100% → 100%      $0.05 → $0.05      14s → 16s

  task                 variant              passed
  greet                sonnet               0/1 → 1/1
  greet                haiku                1/1 → 1/1
  note: the prompt differs for task greet, so its rows compare different tasks
```

That is the greeting eval from above, run again with the target line fenced in
backticks. The first prompt ended "containing exactly the line: hx rocks. Do not run
git.", and Sonnet took the sentence's full stop as part of the line; with the text fenced
it passed. A judge score column appears when either eval was judged; `--json` prints the
pairs as data.

## Run records

`~/.hrn/runs/<id>/` (or `$HRN_HOME/runs`):

- `task.json` — prompt, dir, backend, and the run options by their flag names
  (`timeout` as a duration such as `20m0s`; records written before 0.3 hold it as
  `timeout_ns` and still load)
  and, for exec runs, the supervisor's `attempt_id`
- `events.jsonl` — text, thinking, tool calls, tool results, info, errors and session ids (timestamped),
  one per line, appended while the run goes (`hrn show -f` and `hrn watch` follow it)
- `messages.json` — the api backend's message history, for `hrn run --continue`. It
  repeats the tool results the transcript already holds, so an api run costs roughly
  twice the disk of its transcript
- `handoff.md` — for a follow-up, the transcript of earlier turns its backend was
  given before the prompt (see [Continuing a run](#continuing-a-run))
- `result.json` — status (`ok|error|refused|max_turns|timeout|budget|canceled|limited`), final answer, turns,
  tokens, cache, cost (`cost_is_estimate` for the api backend), duration, the
  claude or codex `session` id, and for a `limited` run `limit_resets`, when its plan's
  limit lifts

Beside `runs/`, `limits.json` holds the reset time of each backend whose usage limit is in
force, which every run consults before it starts. Deleting it forgets them.

`result.json` includes `model_reported: true` only when the backend reports its
model: api responses, claude init lines and gemini model stats. A requested/default
model alone does not set it; codex and opencode do not report one. The field is
omitted when false, and older records without it still load. Exec uses it to send
an empty `resolved_model` when the backend did not report its model.

When complete events fail to write or `events.jsonl` fails to close, `result.json`
includes `record_error` with the number of events not recorded, the first event error
and any close error; `hrn show` and MCP `show` report it and recover intact events
after broken lines. The result is still saved to mark the run finished, the field is
omitted on success, and older records still load.

`hrn runs prune --older-than 30d` removes runs, batch records and eval records that
started before the cutoff, and prints what it freed; `--dry-run` lists them instead and
removes nothing, and `--keep N` spares the newest N runs whatever their age. It refuses to
run without `--older-than`. Records cite each other, so it works from the top down and
never breaks a reference: an eval it keeps keeps its batch and its judge's runs, a batch it keeps keeps every
run it made, a run it keeps keeps the run it `--continue`d from, and a batch whose
worktrees are still on disk stays with its runs, because `hrn batch rm` needs that record
to remove them (it says so, and names the command). A run with no result that wrote in
the last hour is taken to be still running and stays; a directory in `runs/` that is not
a run id is never touched. If any batch or eval record cannot be read, prune refuses:
that record may cite anything. Records go before the runs they cite, so a failure partway
never leaves a record pointing at nothing.

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

## Status and what is open

The three layers from the original brief are all here: the agent core (`hrn run`), the
orchestrator (`hrn batch`, `hrn mcp`) and the scoring layer (`hrn eval`). What remains,
in the order it is worth taking:

1. **A real run of the `api` backend**, and with it **the context-editing measurement**.
   It is the only backend that has never spoken to its API. `scripts/api-check.sh` does
   both in one command once `ANTHROPIC_API_KEY` is set: a read-only run, a `--continue`
   of it, and one long task with context editing on and off, printed side by side with
   the server's reports of what it cleared. It costs a few dollars at most and records
   into a temporary `HRN_HOME`.

`PLAN.md` is the work queue and the decisions log: every increment so far, what was
deferred and why. It still calls the binary by its old name, `hx`, in entries written
before the rename.

## Contributing

- **Standard library only.** No third-party Go modules, and Windows syscalls go through
  `syscall.NewLazyDLL`, not `x/sys`. The CLI backends are external programs, not
  dependencies.
- **Tests first, seen failing.** Process-spawning code is tested by re-running the test
  binary as a helper; the API loop against a fake SSE server; MCP over in-memory pipes.
- **One pull request per increment**, small enough to review, with the plan updated in
  the same PR.

## License

MIT. See [LICENSE](LICENSE).
