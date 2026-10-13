# hrn — a small agent harness

Run coding tasks with Claude Code, Codex, Gemini, opencode, or hrn's own Anthropic API
loop. Keep a consistent record of each run, follow its progress, and compare results —
from one Go binary.

## What you can do

- **Run and revisit tasks.** Save prompts, tool calls, answers, and usage; continue a
  run on the same backend or hand it to another.
- **Watch progress.** The built-in local dashboard shows runs, batches, and tool calls
  as they happen.
- **Split work across agents.** Run a batch in separate Git worktrees, with build/test
  gates and retries.
- **Compare results.** Evaluate backends and models against the same tasks and checks.
- **Connect through MCP.** Use MCP tools in the API loop, or expose hrn as an MCP server
  for another agent to delegate work.

The Go code uses only the standard library. Agent CLIs are optional external programs;
install the ones you want to use.

## Quick start

You need **Go 1.26.4 or later** and a configured backend. For this example, install
the Codex CLI (`npm install -g @openai/codex`) and sign in with `codex login` first.

```bash
go install github.com/cristiangirlea/harness/cmd/hrn@latest
hrn backends
```

Make sure Go's binary directory is on your `PATH`. Then run a task from your project:

```bash
cd path/to/your/project
hrn run -b codex --mode readonly "Explain this project and its main entry points."
```

Use `-b claude`, `-b gemini`, `-b opencode`, or `-b api` to choose another backend.
Each uses its own login or credentials and consumes that provider's plan or API usage.
See [backend setup and behavior](docs/backends.md).

To browse recorded runs and follow new ones, open another terminal:

```bash
hrn watch
```

Open **http://127.0.0.1:8089** in your browser. The dashboard is included in `hrn`;
there is no separate watcher repository or service to install. It shows runs started
through hrn.

Prefer to be told? `hrn bot` opens Ada, hrn's bot, in a small window of her own: she
says what hrn is doing in plain UK English, out loud once you click her page, and as
notifications. See [Live dashboard and Ada](docs/watch.md).

Prefer the terminal?

```bash
hrn runs                 # list recent runs
hrn show <run-id>         # replay one run
hrn show -f <run-id>      # follow a run live
```

## Choose your next step

| I want to… | Read |
|---|---|
| Find commands, flags, and budgets | [Command reference](docs/commands.md) |
| Choose a backend or continue a run | [Backends and continuation](docs/backends.md) |
| Understand what an agent is allowed to do | [Permissions and command guard](docs/permissions.md) |
| Connect hrn to an MCP host or tools | [MCP integration](docs/mcp.md) |
| Run parallel tasks and manage their branches | [Batches and worktree cleanup](docs/batches.md) |
| Use the dashboard, phone view, or Ada bot | [Live dashboard and Ada](docs/watch.md) |
| See the dashboard and Ada in pictures | [hrn watch and Ada, in pictures](docs/presentation.md) |
| Score tasks or compare models | [Evaluations and reports](docs/evaluations.md) |
| Inspect or prune saved data | [Run records](docs/run-records.md) |
| Integrate hrn with a supervising program | [Exec protocol](docs/exec-protocol.md) |

## Permissions and privacy

Start with `--mode readonly` and review the [backend-specific permission
modes](docs/permissions.md#permission-modes). The command guard is a guardrail, not
an operating-system sandbox; the same mode does not mean identical permissions on
all backends.

Run records can contain source code, prompts, and tool output. The dashboard listens
on loopback by default. Enabling `--lan` lets anyone on the selected network read those
records without a login. See [security boundaries and vulnerability reporting](SECURITY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for source builds, checks, repository layout,
and release development. [PLAN.md](PLAN.md) records the work queue and past decisions.

## License

[MIT](LICENSE).
