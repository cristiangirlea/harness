# ADR 0001: hrn as a capability kernel, and where its boundaries are

[← README](../../README.md)

Status: proposed, 2026-10-31, for issues #574 (audit packages and boundaries) and #555 (hrn as a
domain-independent capability kernel). Nothing here is built; each migration step below is its
own pull request once this record is approved.

## Context

hrn grew from one loop into a harness with batches, evals, a game, a dashboard, an MCP host, a
supervisor protocol and a control service. Two questions are open:

- **#574**: which packages own what, which way the dependencies point, and which parts should be
  in-process libraries, subprocesses or remote services, so repositories can be split out one at a
  time without breaking harness.
- **#555**: what the domain-independent core is (task lifecycle, scheduling, state, policy,
  budgets, evidence), how coding-specific behaviour becomes one domain pack among others, and how
  hrn keeps working with no model provider at all. No rewrite: every step moves existing code.

Constraints that shape every answer below: standard library only (no `require` in the root
`go.mod`), records stay loadable (additive `omitempty` fields), the api history is append-only,
and each pull request keeps all CI checks green.

## Current structure

Measured on `main` at `120f5ed` with `go list -f '{{.ImportPath}} <- {{join .Imports " "}}' ./...`
and line counts of non-test `.go` files. Test code adds roughly the same again.

| Layer | Package (non-test lines) | Imports from this module |
|---|---|---|
| 0 leaves | `gitenv` 157, `gitretry` 58, `retrieval` 177, `purpose` 435, `prefs` 114, `owner` 184, `mask` 103, `shape` 801, `qr` 431, `runpaths` 44, `scripthint` 350, `route/table` 74 | none |
| 1 protocols | `mcp` 1,211 | gitenv, **retrieval** |
| | `remote` 553 | retrieval |
| | `ws` 2,104 | gitretry |
| 2 tools | `tools` 2,594 | gitenv, mcp, **retrieval**, runpaths, shape |
| 3 backends | `agent` 5,694 | gitenv, mcp, tools, retrieval, **prefs, purpose, remote, route/table** |
| 4 records | `runlog` 934 | **agent**, gitenv, owner, retrieval, route/table, runpaths |
| 5 events, budgets | `flow` 490, `quota` 442 | **agent**, runlog (+ mask) |
| | `followup` 663 | agent, gitenv, runlog, tools, ws |
| | `limits` 298 | **agent, followup**, runlog |
| 6 coding pack | `batch` 3,570 | agent, flow, gitenv, gitretry, limits, mcp, owner, purpose, runlog, tools, ws |
| 7 views, packs | `report` 644, `flowview` 298 | agent/flow, batch, purpose, runlog, owner, ws |
| | `eval` 2,264 | agent, batch, report, purpose, runlog, tools, gitenv |
| | `gc` 2,877 | batch, gitretry, report, runlog, ws |
| | `route` 198 | agent, eval, purpose, route/table, runlog |
| | `game` 1,872 | agent, batch, eval, mask, runlog, scripthint, gitenv |
| 8 surfaces | `execproto` 582 | agent, followup, mcp, purpose, runlog, tools |
| | `hrnmcp` 1,577 | almost everything, including remote and gc |
| | `watch` 5,071 (+ assets) | almost everything, including game, quota, prefs |
| | `update` 1,669 | **watch** |
| | `cmd/hrn` 4,259; `scripts/tool-shapes` | everything; agent, shape |
| separate module | `server/` 2,928 | root `internal/agent`, `internal/retrieval` (tests also `remote`) via `replace => ..` |

Where #574's names sit today: **Ada** is the bot of hrn watch (`watch/bot.go`, `watch/voice.go`,
`cmd/hrn/bot.go`) and a track in `game`; **adanext-exec** is an external repository, the command
gate of [terminal-through-the-gate](../terminal-through-the-gate.md), reached as a binary with a
JSON protocol; `execproto` (`hrn exec`) is hrn's own supervisor JSONL protocol; **memory** is
approved project knowledge: `retrieval` (wire contract), `remote` (client),
`agent/retrieval.go` (packet into the prompt), `server/platform/knowledge*.go` (store and HTTP);
**MCP** is `mcp` (protocol, client) plus `hrnmcp` (host tools); the **dashboard** is `watch`.
**adanext-map** has no reference in this repository (see open questions).

### Violations

1. **`agent` resolves what to run, not only how.** It fetches knowledge over the network
   (`agent/retrieval.go:120,144`: `remote.NewFromEnv`, `remote.RetrievalUnavailable`), reads the
   backend preference file (`agent/preferences.go`), routes from the table and `purpose`
   (`Options.RouteWithNote`, `options.go:90`) and picks a turn default by purpose
   (`Task.ApplyTurnDefault`, `options.go:162`).
2. **A security policy lives in the knowledge contract.** `retrieval.ExecutorEnv`
   (`retrieval/contracts.go:166`) drops `HRN_DATABASE_URL` and every `HRN_*_TOKEN` (any case) from
   an environment. It guards every child process hrn starts: MCP servers (`mcp/client.go:79`),
   `${VAR}` expansion in MCP configs (`mcp/config.go:189`), `run_command` (`tools/exec.go:52`) and
   agent CLIs (`agent/cli.go:442`). `mcp` and `tools` import `retrieval` only for it.
3. **`runlog`, `flow`, `quota`, `limits` import `agent`.** `Task`, `Event` and `Result` live in
   the backend package, so records and events link every backend adapter. `runlog` also calls
   `agent.ResolveSetup` (`runlog.go:123`) and `agent.ValidateContinuationPin` (`runlog.go:441,466`).
4. **`limits` reaches the registry and `followup`**: `Partner` reads preferences and probes
   backends (`limits.go:24–35`), and `After` plans a carry-on with `followup.Make` (`limits.go:286`).
5. **`update` imports `watch`** only for `watch.DefaultAddr` (`update/watch.go:48`).
6. **`server/` is not independent**: it imports root internals through a `replace` directive, and
   its worker embeds `*agent.APIBackend` with in-process durable checkpoints (`worker.go:26,140`).
7. **Size, not direction:** `batch/batch.go` is 2,883 lines with a 312-line `runTask`
   (lines 704–1015); `APIBackend.Run` is 363 lines (`agent/api.go:117–479`).

## Decision

hrn becomes a **capability kernel** with **domain packs** on top and **delivery surfaces** on the
outside. This is a direction for moves of existing code, not new architecture to build first.

### The kernel

Domain-independent, standard library only, no knowledge of git, worktrees or code review:

| Concern | Contract | Where it comes from |
|---|---|---|
| Task lifecycle | `Task`, `Options`, `Event`, `Result`, `Usage`, `Conversation`, the `Backend` interface | moved from `agent` into a new `internal/kernel` (steps 10–14); `agent` keeps aliases |
| State | run records (`runlog`), events and step tree (`flow`), paths (`runpaths`) | as now, importing `kernel` |
| Scheduling | O_EXCL slot leases and the queue | the lease primitive of `ws`; `server/platform/queue.go` remotely |
| Policy | guard, guarded environment, secret files, child-environment filter | `tools/guard.go`, `guardenv.go`, `secretfiles.go`, `internal/envpolicy` (step 4) |
| Budgets | turn, token and cost limits, limit windows, quota | `limits` (window store only, step 8), `quota` |
| Evidence | one `Evidence` record (kind, source, hash, bounded excerpt) on `Result` and events | today spread over `retrieval.Entry.Evidence`, batch gates and eval judging |

`retrieval` stays a leaf: after step 4 it is a pure wire contract (types, digests,
`ValidatePacket`), so `kernel`, `agent` and `runlog` may import it like any leaf. New code-specific
data (a branch, a gate, an issue) goes in an opaque, namespaced `Task.Pack` (`omitempty`), never a
kernel field; existing fields stay where they are on disk.

### Behind interfaces

- **Knowledge fetching** (step 5): `retrieval.Searcher` with one method,
  `SearchKnowledge(ctx, retrieval.Search) (retrieval.Packet, error)`, the name `*remote.Client`
  already has. An adapter, `remote.Searcher(*Client)`, calls it and wraps an error as
  `fmt.Errorf("%w: %w", retrieval.ErrUnavailable, err)` exactly when `RetrievalUnavailable` says
  so today (5xx, 429, network errors, deadlines; never a TLS verification, unknown-authority,
  hostname, invalid-certificate or record-header error, nor another 4xx), keeping the cause.
  `agent` gets it through `Task.Knowledge func() (retrieval.Searcher, error)` (`json:"-"`),
  called exactly where `remote.NewFromEnv` is called today, so a replayed pin still constructs no
  client; its error keeps today's `retrieval transport configuration invalid`.
- **Child-environment policy** (step 4): `envpolicy.Filter(env []string) []string`, a leaf with
  today's deny rules byte for byte. Not an interface: a security rule is not swappable.
- **Resolution** (step 9): preferences, the routing table and purpose-based defaults move to a
  new `internal/resolve`; surfaces and packs call it before building a task, `agent` sees
  resolved options.
- **Providers**: the adapters (api, claude, codex, gemini, opencode, agy) and their registry stay
  in `agent` as implementations of `kernel.Backend`.

## Rules

1. Every package has one layer; it may import only the layers listed for it, plus the named edges.

| Layer | Packages | May import |
|---|---|---|
| leaf | `gitenv`, `gitretry`, `mask`, `owner`, `prefs`, `purpose`, `qr`, `retrieval`, `route/table`, `runpaths`, `scripthint`, `shape`; new `envpolicy`, `watchaddr` | no module package |
| kernel | new `kernel`, `runlog`, `flow`, `quota`, `limits` | leaf; inside the layer only `runlog → kernel` and `flow`, `quota`, `limits → kernel, runlog` (`kernel` imports no kernel package) |
| infrastructure | `mcp`, `tools`, `remote`, `ws` | leaf, kernel; `tools → mcp` |
| providers | `agent` | leaf, kernel, infrastructure |
| resolution | new `resolve` | the layers above |
| coding pack | `batch`, `followup`, `gc`, `report`, `flowview` | the layers above |
| eval pack | `eval`, `route` | the layers above, coding |
| game pack | `game` | the layers above, coding, eval (reads records and scores) |
| surfaces | `cmd/hrn`, `hrnmcp`, `watch`, `execproto`, `update`, `scripts/...` | everything above; only `cmd/hrn` imports another surface |
| server module | `server/...` | nothing from the root module |

2. Named denials on top of the layers: `agent` imports no `remote`, `prefs`, `purpose` or
   `route/table`; `mcp` and `tools` import no `retrieval`.
3. Enforced by `internal/archtest`, standard library only. It parses the import block of every
   `.go` file in the repository with `go/parser` (`ImportsOnly`), test files included and
   `testdata/` excluded, so no `GOOS`/`GOARCH` suffix or build tag can hide an edge (today's
   variants: `batch`, `gc`, `remote`, `tools`, `update`, `ws`). A file's package is its directory
   under the nearest `go.mod`, so the nested `server` module, which the root `go test ./...` never
   builds, is checked from the root test. An unclassified package fails. A `_test.go` file counts
   for its directory's package whether it declares `package x` or `package x_test`, under the same
   rules with no test allowance; an edge that only tests use is its own baseline line marked
   `(test)`, so production and test edges can go in different PRs.
4. Existing violations sit in `internal/archtest/baseline.txt`, one `from -> to` per line. The
   test fails on a forbidden edge missing from it and on a line whose edge no longer exists, so the
   PR that removes an edge deletes its line. "Only shrinks" is checked against the base branch,
   not the file alone: `scripts/archtest-baseline.sh <base-ref>`, in CI and `/check`, fails when
   the head's baseline holds a line the base's (`git show <base-ref>:...`) does not, and prints any
   change to the layer map so a reclassification is a visible, reviewed decision.

Baseline at step 1, and the PR that removes each line:

| Edge | Why it exists | Removed by |
|---|---|---|
| `update → watch` | `DefaultAddr` | 3 |
| `mcp → retrieval`, `tools → retrieval` | `ExecutorEnv` | 4 |
| `agent → remote` | packet fetch | 5 |
| `limits → followup` | carry-on planning | 8 |
| `agent → prefs`, `agent → route/table`, `agent → purpose` | preferences, routing, `ApplyTurnDefault` | 9 |
| `flow → agent`, `quota → agent`, `quota → agent (test)` | `Event`, `EventKind`, `Status`, `Quota` | 11 |
| `flow → agent (test)` | one `Task`, one `Result` | 14 |
| `runlog → agent`, `runlog → agent (test)` | `Task`, `Result`, `Setup`, pin check; tests also `Conversation`, `ResolveSetup` | 15 |
| `limits → agent`, `limits → agent (test)` | registry (gone in 8), `Result`, `StatusLimited`, `MaxLimitWindow` | 16 |
| `server/cmd/hrn-worker → internal/agent` | `NewAPIBackend` | 20 |
| `server/platform → internal/agent`, `server/platform → internal/agent (test)` | execution wire types (step 21a) | 21a |
| `server/platform → internal/retrieval`, `… (test)`, `server/platform → internal/remote (test)` | knowledge types, client round trip | 21b |

Not in the baseline: `agent → retrieval` (a contract leaf once step 4 takes the policy out),
`route → eval` (both in the eval pack; this reclassification is all that is left of old edge 5),
`gc → report` (both coding), `game → batch`, `game → eval` (permitted).

## Migration

Each step is one pull request, behaviour-neutral unless it says so, with every CI check green.
Steps marked **neutral** leave the baseline as it is; the others delete the lines named above.

1. **archtest, layer map, baseline and the base-branch script.** No code moves.
2. **Zero-provider test** (below), test only, **neutral**. A path that fails it today is left out
   and gets its own fix PR (2a, 2b, ...) that adds it to the test.
3. **`internal/watchaddr`**: `DefaultAddr` and the `watch-<port>.log` name; `watch` and `update`
   use it.
4. **`internal/envpolicy`**: `Filter` moves from `retrieval.ExecutorEnv` unchanged; the four call
   sites and any in `server/` switch, `ExecutorEnv` is deleted. Its test moves with it and gains
   cases for mixed case, `HRN_TOKEN_X` (kept), a name without `=`, and `${VAR}` expansion of a
   denied name in an MCP config; the `mcp`, `tools` and `agent` tests that assert filtering stay.
5. **Knowledge searcher for `agent`** (interface above). Unchanged: the scope
   (tenant, project, revision, backend, model), the version pin (`RetrievalVersion`, or the pinned
   packet's version on repair), the bounded query and `RunRef`, `retrieval.json` written with
   O_EXCL, and the outcomes: optional mode plus unavailable records
   `optional retrieval unavailable` and runs on; cancellation records `retrieval canceled`; any
   other error fails as today, and a retrieval-scoped task with no `Knowledge` fails naming it.
   `agent/retrieval_test.go` keeps its coverage with its scoped tasks given a fake `Knowledge`
   searcher in place of `remote.NewFromEnv`, plus a fake-searcher test per outcome and adapter
   tests for each class `RetrievalUnavailable` separates. The seven execution entry points of
   step 15 set `Knowledge` (`remote.NewFromEnv` plus the adapter), each tested against a loopback
   fake; the server worker sets no retrieval scope. Fixtures building scoped tasks set it too:
   `cmd/hrn/retrieval*_test.go`, `batch/retrieval_test.go`, `hrnmcp/project_test.go`,
   `runlog/retrieval*_test.go`, `server/platform/knowledge_execution_integration_test.go`.
6. **Split `batch/batch.go` by concern**, moves only, **neutral**: `spec.go`, `slots.go`,
   `attempt.go`, `gate.go`, `git.go`, `remove.go`, `store.go`, `merge.go`.
7. **Split `runTask` into named phases**, **neutral**, a separate PR after 6.
8. **`limits` keeps the window store** (`Note`, `Until`, `Status`, the state file). Whatever
   consults the registry, preferences, billing variables or `followup` (`Ready`, `Spent`,
   `MeteredEnv`, `Partner`, `Pick`, `After`, `CarryOn`, the default-backend lookup) moves to
   `followup`, which already imports `agent` and nothing of `limits`. `Note`'s preference check
   moves to its callers with the same error. The tests that register fake backends
   (`limits/preferences_test.go`, the `Register`/`Get` cases of `limits_test.go`) move with the
   code. Before step 9, so `limits` never imports `resolve`.
9. **`internal/resolve`**: `BackendPreferences`, `DefaultBackendNameIn`, `Options.Route` and
   `RouteWithNote` (as functions taking `*agent.Options`), `Task.ApplyTurnDefault` (as
   `ApplyTurnDefault(*agent.Task, backend)`) and `TurnDefaultInfo` move with their tests; callers
   in `cmd/hrn`, `execproto`, `hrnmcp`, `batch`, `eval`, `followup`, `watch` switch.
10. **Golden JSON**, test only, **neutral**: encodings of `Task` (with embedded `Options`),
    `Result`, `Event`, `Conversation`, `APICheckpoint` and `runlog.Record`, and decoding of stored
    fixtures (legacy `timeout_ns`, records with and without `retrieval`). Steps 11–16 keep them
    byte-identical, and the `runlog`, `followup` and continuation-pin tests pass untouched.
11. **`kernel` value types** with their methods: `Mode` (`ParseMode`, `String`, `Set`), `Duration`,
    `CompactWindow` (JSON, `Set`; `check` exported), `EffortLevels`, `ApprovalRequest`, `EventKind`,
    `Event`, `Quota` and the `QuotaWindow` it holds, `Usage`, `Status`, `UsageMetadata`, and the
    `Modes` list. `agent` keeps `type X = kernel.X` and its constants. `flow` and `quota` import
    `kernel`, and `quota_test.go` switches to kernel names.
12. **`kernel.Options`**, **neutral**, with `IsLean`, `Over` and `Schema`; `Schema` needs only
    step 11's types and `jsonName`, which moves as `kernel.JSONName` (`agent`'s `flagName` calls
    it), so `hrnmcp`'s `agent.Options{}.Schema()` compiles unchanged through the alias and
    `options_test.go`'s schema cases pass untouched. `Validate` calls `tools.Guarded`,
    which the kernel may not import: the pure checks become `kernel.Options.ValidateShape`, and
    `agent.ValidateOptions` adds the guard. The rename makes every old `.Validate()` call fail to
    compile until it is switched, so none can silently lose the guard.
13. **`kernel.Result`**, **neutral**, with `ModelTokens`, `KnownCost`, `ValidateUsage`,
    `UsageMissing`; the unexported `addUsage` becomes an `agent` function. A method cannot be
    defined on an alias of another package's type, which is why every method above moves or
    becomes a function before its type does.
14. **`kernel.Task`, `Conversation`, `Message`, durable types, `Backend`**.
    `Message` (`apiMessage` today: role and raw blocks) moves and `agent` aliases it, so the
    append-only history code is untouched. `DurableExecution`, `APICheckpoint`, `PendingOperation`
    and `ErrRecoveryRequired` are data and move; `prepareDurable` stays. `Task.UnmarshalJSON` moves.
    The registry (`Register`, `Get`, `Names`) and `Capabilities` stay in `agent`. `flow_test.go`
    switches to kernel names.
15. **`runlog` off `agent`**: `ValidateContinuationPin` (retrieval types and `Conversation` only)
    and `Setup` move to `kernel`; `ResolveSetup` stays in `agent`. No registration and no init
    order: the four `runlog.Create*` functions take a `*kernel.Setup` after `task`, and the seven
    production callers pass `agent.ResolveSetup` of the backend and task they pass today, so
    digests are unchanged (`cmd/hrn/main.go` run and carry-on, `batch.go`, `eval/judge.go`,
    `execproto/protocol.go`, `hrnmcp/server.go` run and carry-on); tests pass nil or a fixture.
    Each caller gets a test that its record's setup equals `ResolveSetup`; `runlog`'s tests switch
    to kernel names and `setup_test.go`'s `ResolveSetup` case moves to `cmd/hrn`. `agent` never
    imports `runlog`, so no test cycle forms.
16. **`limits` off `agent`**: `MaxLimitWindow` moves to `kernel`; `Result` and `StatusLimited`
    already come from it, and `limits_test.go` switches to kernel names. Records and events no
    longer link the adapters.
17. **Split `APIBackend.Run` into phases** (request, stream, tools, stop), **neutral**; the
    fake-SSE tests in `agent/api_test.go` pass untouched.
18. **`kernel.Evidence`**, **neutral**, additive and `omitempty`; filled by gates and knowledge
    packets, read by eval and game.
19. **Worker subprocess contract**, **neutral**, before the worker stops embedding
    `*agent.APIBackend`. Written in `docs/`, versioned, built on `execproto`'s JSONL: start (the task
    and, on resume, its checkpoint), events, the final result, cancellation (a `cancel` frame; the worker answers `canceled` only when nothing is in flight,
    otherwise `RecoveryRequired`; a kill after the grace period yields no result at all, and the
    service reaches recovery through the job lease expiring, as for a dead worker), and durable checkpoints. Credentials reach
    the child only through its environment, filtered by `envpolicy`; checkpoints carry none.
    - **Restrictions as today**: the worker starts `hrn exec --worker`, a mode that only narrows:
      backend `api`, an `http` loopback or `localhost` base URL with no user info (`RunOnce`'s
      check, also kept in the worker before it spawns), no tools whatever the start frame says.
      Plain `hrn exec` keeps its rules; neither path gains a backend, endpoint or tool.
    - **Checkpoints**: hrn sends a numbered `checkpoint` frame and waits. The worker rechecks the
      workspace (`prepareWorkspace`), posts with the job's generation and answers `ack` only once
      the service accepts; otherwise `nack`. A `nack`, a timeout or a closed pipe ends the run with
      `ErrRecoveryRequired` and `RecoveryRequired`; a dead worker reports nothing, and the service
      reaches recovery when its lease expires, as a failed `Save` does
      today: never continued, never resent. A resumed checkpoint with `Pending` fails the same way.
    - **Fencing**: workspace validation before the start (and `finishWorkspaceRecovery`) stays
      in the worker; only the worker calls the control service, always with the generation; a
      fenced or cancelled heartbeat sends `cancel`.
    Golden transcripts run in both modules (`execproto` produces them, server tests replay them
    against a `TestHelper` fake) for each case: saved, `nack`, timeout, pipe closed before the
    answer, pending on resume, workspace changed, fenced generation, non-loopback endpoint, tools.
20. **Worker runs `hrn exec`** over that contract; `worker.go` and `cmd/hrn-worker` stop importing
    `agent`. The PostgreSQL integration tests cover cancellation, retry and recovery as today.
    Behaviour change: the worker needs an `hrn` binary.
21. **`server/` off root internals**, in two PRs; the precondition for extraction.
    - **21a, execution wire types** owned by `server/platform`: `JobOptions` (`agent.Options` and
      `ModeReadOnly` in `types.go`), `JobResult` (`agent.Result` and statuses in `types.go`,
      `store.go`, `queue.go`, `http.go`, `recovery.go`), `Checkpoint` (`agent.APICheckpoint` in
      `queue.go`, `http.go`, `recovery.go`: the fields read plus the raw JSON, kept byte for
      byte). Unknown fields are rejected, mode, status and sizes validated, one function converts
      to the start frame; server tests decode step 10's golden JSON and stop importing `agent`.
    - **21b, knowledge**: `retrieval` wire types used by `knowledge*.go` and their tests are copied
      with golden tests on both sides; `remote_client_integration_test.go` becomes fixtures the
      server produces and `remote`'s tests replay. The `replace` directive goes.

Dependencies: 3–8 are independent after 1; 5 after 4; 7 after 6; 9 after 8; 10–16 in order, after 9;
19–21b in order, after 4 and 14.

## Repository extraction

No extraction before the contracts it crosses are stable: written down in `docs/`, versioned,
and covered by golden-file tests that both sides run. Because the root module takes no `require`,
an extracted part talks to hrn as a **binary** (subprocess, JSONL or JSON), over **HTTP**, or
through **files on disk** (run records), never as an imported Go library.

| Candidate | Why it could leave | Preconditions | When |
|---|---|---|---|
| `server/` (control service, worker) | already its own module and deploy | steps 19–21b; knowledge and run APIs versioned; its CI on its own runner | first |
| `watch` (dashboard, Ada) | 5,071 lines plus assets, its own release rhythm | reads only records, flow events and stable `hrn` JSON; archtest shows no internal reach | after step 18 |
| `game` | a pack reading records | `Evidence` and record formats stable | after watch |
| `eval` | large, depends on batch | batch's result format stable, a batch run reachable as `hrn batch` JSON | last, if ever |

Kept in harness: the kernel, the provider adapters, the coding pack, `cmd/hrn`, `hrnmcp`,
`execproto`. adanext-exec stays an external binary behind the policy contract.

Release compatibility: hrn keeps one version (`Version` in `internal/agent/agent.go`); every
cross-repository contract carries its own version field, readers accept the previous version for
one release, and records stay additive as now. CI: each repository runs its own checks plus the
other side's golden files; harness CI never depends on an extracted repository being green.

## Zero-provider operation

hrn must start and be useful with no provider: listing and showing runs, reports, `flow`,
`watch`, `gc`, `prune`, batch `rm`/`diff`/`merge`, the read-only MCP tools, and knowledge commands
against a reachable service. Only a command that executes a task needs a provider.

The test (step 2) builds `hrn` once and runs it with:

- **`PATH`** set to a temporary directory holding links (copies on Windows) to `git` and the
  system tools hrn shells out to, found with `exec.LookPath` before the environment is cleared.
  The test first asserts that `claude`, `codex`, `gemini`, `opencode` and `agy` do not resolve.
- **No credentials**: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CONFIG_DIR` and
  `CODEX_HOME` unset; `HOME`, `USERPROFILE`, `APPDATA` and `HRN_HOME` on a fixture home with fake
  runs, a batch, an eval and a git repository; the knowledge service a loopback fake.
- **Read paths** exit 0 with the expected output. A second pass puts a shim named for each
  provider on `PATH` that records any call: read paths must leave no record.
- **Execution paths** (`hrn run` with each `-b` and with none, `hrn batch`, `hrn eval`, an
  `hrn exec` start frame, the MCP run tool) fail within a timeout, exit non-zero or return an error
  result, name the missing provider (executable or key), and leave no child process behind.

The fixture model (`server/cmd/hrn-fixture-model`) and `TestHelper*` processes remain the way
tests exercise task execution without a provider.

## Consequences

- New code has an obvious home, and the archtest says when it does not.
- Records, events and policy stop linking the backend adapters, so they can be tested and, later,
  read by other repositories without pulling in the providers.
- The child-environment filter becomes a named security rule instead of a side effect of the
  knowledge contract.
- Interfaces and `resolve` add a little wiring in each surface; that is the cost of moving
  knowledge, routing and preferences out of `agent`.
- Aliases in `agent` stay until no caller uses the old names; removing them is a later cleanup.
- Twenty-two pull requests (plus any 2a, 2b fixes) touch busy files (`batch.go`, `api.go`,
  `options.go`); they should land between feature work, not under it.

## Open questions

1. **adanext-map**: which repository and contract does #574 mean? Nothing here references it.
2. `purpose` labels prompts with coding-flavoured kinds; it is a leaf here. Should it become
   coding-pack data once `resolve` is its only kernel-side reader?
3. `ws` mixes the generic slot lease with issue leases and git: split it in this plan, or later?
4. `APICheckpoint` names one provider inside the kernel; rename it to a generic checkpoint later?
5. Does extraction of `watch` (with Ada) matter enough to plan, or should it stay in harness?
