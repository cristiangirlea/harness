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
| 3 backends | `agent` 5,694 | gitenv, mcp, tools, **prefs, purpose, remote, retrieval, route/table** |
| 4 records | `runlog` 934 | **agent**, gitenv, owner, **retrieval, route/table**, runpaths |
| 5 events, budgets | `flow` 490, `quota` 442 | **agent**, runlog (+ mask) |
| | `followup` 663 | agent, gitenv, runlog, tools, **ws** |
| | `limits` 298 | agent, **followup**, runlog |
| 6 coding pack | `batch` 3,570 | agent, flow, gitenv, gitretry, limits, mcp, owner, purpose, runlog, tools, ws |
| 7 views, packs | `report` 644, `flowview` 298 | agent/flow, **batch**, purpose, runlog, owner, ws |
| | `eval` 2,264 | agent, batch, report, purpose, runlog, tools, gitenv |
| | `gc` 2,877 | batch, gitretry, report, runlog, ws |
| | `route` 198 | agent, **eval**, purpose, route/table, runlog |
| | `game` 1,872 | agent, batch, eval, mask, runlog, scripthint, gitenv |
| 8 surfaces | `execproto` 582 | agent, followup, mcp, purpose, runlog, tools |
| | `hrnmcp` 1,577 | almost everything, including remote and gc |
| | `watch` 5,071 (+ assets) | almost everything, including game, quota, prefs |
| | `update` 1,669 | **watch** |
| | `cmd/hrn` 4,259 | everything |
| separate module | `server/` 2,928 | root `internal/agent`, `internal/retrieval` via `replace => ..` |

Where #574's names sit today: **Ada** is the bot of hrn watch (`watch/bot.go`, `watch/voice.go`,
`cmd/hrn/bot.go`) and a track in `game`; **adanext-exec** is an external repository, the command
gate of [terminal-through-the-gate](../terminal-through-the-gate.md), reached as a binary with a
JSON protocol; `execproto` (`hrn exec`) is hrn's own supervisor JSONL protocol; **memory** is
approved project knowledge: `retrieval` (wire contract), `remote` (client),
`agent/retrieval.go` (packet into the prompt), `server/platform/knowledge*.go` (store and HTTP);
**MCP** is `mcp` (protocol, client) plus `hrnmcp` (host tools); the **dashboard** is `watch`.
**adanext-map** has no reference in this repository (see open questions).

### Violations

The bold edges above point the wrong way for a kernel:

1. **`agent` imports `remote`, `retrieval`, `prefs`, `purpose`, `route/table`.** The backend layer
   fetches knowledge over the network (`agent/retrieval.go`), reads the owner's preference file
   (`agent/preferences.go`) and the routing table (`agent/options.go`). Resolution of what to run
   is mixed into how to run it.
2. **`mcp` imports `retrieval`** (`mcp/config.go`, `mcp/client.go`), and `tools/exec.go` does too: a
   wire protocol and the tool layer know the knowledge contract.
3. **`runlog`, `flow`, `quota`, `limits` import `agent`.** `Task`, `Event` and `Result` live in the
   backend package, so records and events depend on every backend adapter. `runlog` also imports
   `retrieval` and `route/table`.
4. **`limits` imports `followup`, which imports `ws`**: a budget package reaches worktree slots.
5. **`route` imports `eval`**: routing policy depends on a domain pack's scores.
6. **`update` imports `watch`** only for `watch.DefaultAddr` (`update/watch.go:48`); the log name
   `watch-<port>.log` is a literal copied from watch's convention.
7. **`server/` is not independent**: it imports the root's `internal/agent` and
   `internal/retrieval` through a `replace` directive.
8. **Size, not direction:** `batch/batch.go` is 2,883 lines holding spec checks, slots, attempts,
   gate output decoding, git plumbing, removal, persistence and merge, with a 312-line `runTask`
   (lines 704–1015); `APIBackend.Run` is 363 lines (`agent/api.go:117–479`).

Accepted as they are: `watch`, `hrnmcp` and `cmd/hrn` import nearly everything. They are the top
layer; their fan-in is the point of a delivery surface.

## Decision

hrn becomes a **capability kernel** with **domain packs** on top and **delivery surfaces** on the
outside. This is a direction for moves of existing code, not new architecture to build first.

### The kernel

Domain-independent, standard library only, no knowledge of git, worktrees, code review or any
model vendor:

| Concern | Contract | Where it comes from |
|---|---|---|
| Task lifecycle | `Task`, `Event`, `Result`, `Usage`, and the `Backend` interface `Run(ctx, Task, emit) Result` | moved from `agent` into a new leaf `internal/kernel`; `agent` keeps type aliases so callers and JSON stay identical |
| State | run records (`runlog`), the event stream and step tree (`flow`), paths (`runpaths`) | as now, importing `kernel` instead of `agent` |
| Scheduling | O_EXCL slot leases and the queue | the lease primitive of `ws`; `server/platform/queue.go` for remote work |
| Policy | the guard, guarded environment, secret files, commit policy | `tools/guard.go`, `guardenv.go`, `secretfiles.go`, `agent/commitpolicy.go`, later one `internal/policy` |
| Budgets | turn, token and cost limits, quota windows | `limits`, `quota` |
| Evidence | one `Evidence` record (kind, source, hash, bounded excerpt) carried on `Result` and events | today spread over `retrieval.Entry.Evidence`, batch gate results and eval judging |

Common task contracts carry no code-specific fields: a branch, a worktree, a gate command or an
issue number is pack data in an opaque, namespaced extension (`Task.Pack`, `omitempty`), never a
kernel field. Existing fields stay where they are on disk; new ones follow this rule.

### Behind interfaces

The kernel defines small interfaces and the surfaces wire the implementations in:

- **Knowledge** (`retrieval`, `remote`): `Knowledge.Packet(ctx, Scope) (Packet, error)`. `agent`,
  `mcp` and `tools` take the interface; only surfaces import `remote`.
- **Routing**: `Router.Pick(Task) (backend, model string)`, fed by `route/table`; `route` (which
  needs `eval`) becomes part of the eval pack and writes the table.
- **Preferences**: resolved by the surface before a task is built; `agent` sees resolved options.
- **Providers**: the backend adapters (api, claude, codex, gemini, opencode, agy) stay in `agent`
  as implementations of `kernel.Backend`.

### Domain packs

| Pack | Packages | Depends on |
|---|---|---|
| coding | `batch` (worktrees, gates, merge), `ws` issue leases and git, `followup` handover, `gc` | kernel |
| eval | `eval`, `route`, `report` | kernel, coding (runs variants as batches) |
| game | `game` | kernel, reads eval and batch records |

A pack may import the kernel and the packs listed for it, nothing above it.

### Delivery surfaces

`cmd/hrn` (CLI), `hrnmcp` (MCP host), `watch` (dashboard, Ada), `execproto` (`hrn exec`),
`update`, and `server/` (control service, worker). Surfaces may import anything; nothing imports a
surface. External gates and services (adanext-exec, the control service seen from the CLI) are
reached as **subprocesses or over HTTP**, never as Go libraries: the root module takes no
`require`.

## Rules

1. Dependencies point inward: surface → pack → kernel → leaf. A leaf imports no module package.
2. The kernel imports no pack and no surface; a pack imports no surface; no package imports
   `cmd/hrn`, `watch`, `hrnmcp`, `execproto` or `update`.
3. Cross-pack edges are only the ones in the pack table.
4. Enforced by a test, `internal/archtest`, standard library only (`go/build`): it assigns every
   package to a layer, walks the imports and fails on any forbidden edge. Today's violations sit
   in a baseline in the test that may only shrink, like `scripts/comment-check.sh`. A new package
   without a layer fails the test, so placing it is a reviewed decision.

## Migration

Small, independently mergeable steps, in this order. Each one is behaviour-neutral unless it says
so, keeps every CI check green, and shrinks the archtest baseline.

1. **archtest with the full baseline.** Adds the test and the layer map; it fails only on a new
   violation. No code moves.
2. **update ↔ watch constants.** `DefaultAddr` and the log file name move to a leaf
   (`internal/watchaddr`); both `watch` and `update` use it. Removes edge 6.
3. **Knowledge behind an interface for `mcp` and `tools`.** The two packages take a small interface
   for what they read from `retrieval`; surfaces pass the implementation. Removes edge 2.
4. **Knowledge behind an interface for `agent`.** `agent/retrieval.go` stops importing `remote`;
   `cmd/hrn`, `hrnmcp` and `server` wire the client. Covered by `agent/retrieval_test.go`.
5. **Split `batch/batch.go` by concern**, moves only: `spec.go` (load, normalize, checks), `slots.go`
   (take and give back), `attempt.go` (`runAttempt`), `gate.go` (run and decode gate output),
   `git.go` (agent git and commit), `remove.go`, `store.go` (save, list, load), `merge.go` (diff,
   merge). Same package, same functions; the diff is reviewed as moves. `runTask` is split into
   named phases in a second pull request.
6. **`internal/kernel` with `Task`, `Event`, `Result`, `Usage`.** Moved from `agent`, aliases left
   behind; `runlog`, `flow`, `quota`, `limits` import `kernel`. Old records load unchanged
   (`runlog` tests on stored fixtures). Removes most of edge 3.
7. **Preferences and routing resolved at the surface.** `agent/options.go` gets resolved values;
   `prefs` and `route/table` leave `agent`'s imports. Removes edge 1.
8. **`limits` without `followup`.** Whatever `limits` needs from continuation becomes a kernel
   type or a parameter. Removes edge 4.
9. **`route` into the eval pack** writing `route/table`; the kernel reads only the table. Edge 5.
10. **Split `APIBackend.Run` into phases** (request, stream, tools, stop) with the append-only
    history unchanged; the fake-SSE tests in `agent/api_test.go` must pass untouched.
11. **`kernel.Evidence`** added (additive, `omitempty`) and filled by batch gates and knowledge
    packets; eval and game read it.
12. **`server/` stops importing root internals**: the worker runs `hrn` as a subprocess or speaks
    the record and knowledge wire formats from its own copies, with golden-file tests in both
    modules. Edge 7; the precondition for extraction.

Steps 1–5 are cheap and independent of each other after step 1; 6 is the hinge for 7–11.

## Repository extraction

No extraction before the contracts it crosses are stable: written down in `docs/`, versioned,
and covered by golden-file tests that both sides run. Because the root module takes no `require`,
an extracted part talks to hrn as a **binary** (subprocess, JSONL or JSON), over **HTTP**, or
through **files on disk** (run records), never as an imported Go library.

| Candidate | Why it could leave | Preconditions | When |
|---|---|---|---|
| `server/` (control service, worker) | already its own module and deploy | step 12; knowledge and run APIs versioned; its CI on its own runner | first |
| `watch` (dashboard, Ada) | 5,071 lines plus assets, its own release rhythm | reads only records, flow events and a stable `hrn` JSON output; archtest shows no internal reach | after step 11 |
| `game` | a pack reading records | `Evidence` and record formats stable | after watch |
| `eval` | large, depends on batch | batch's result format stable, a batch run reachable as `hrn batch` JSON | last, if ever |

Kept in harness: the kernel, the provider adapters, the coding pack, `cmd/hrn`, `hrnmcp`,
`execproto`. adanext-exec stays an external binary behind the policy contract.

Release compatibility: hrn keeps one version (`Version` in `internal/agent/agent.go`); every
cross-repository contract carries its own version field, readers accept the previous version for
one release, and records stay additive as now. CI: each repository runs its own checks plus the
other side's golden files; harness CI never depends on an extracted repository being green.

## Zero-provider operation

hrn must start and be useful with no API key and no agent CLI on `PATH`: listing and showing
runs, reports, `flow`, `watch`, `gc`, `prune`, batch `rm`/`diff`/`merge`, the read-only MCP
tools, and knowledge commands against a reachable service. Only a
command that executes a task needs a provider, and it fails with one clear error naming the
missing provider. A test runs the CLI's read paths with an empty `PATH` and no key; whether any of
them probes a backend today is checked in step 1 and fixed where found. The fixture model
(`server/cmd/hrn-fixture-model`) and `TestHelper*` processes remain the way tests exercise task
execution without a provider.

## Consequences

- New code has an obvious home, and the archtest says when it does not.
- Records, events and policy stop depending on every backend adapter, so they can be tested and,
  later, read by other repositories without pulling in the providers.
- Interfaces add a little wiring in each surface; that is the cost of moving knowledge, routing
  and preferences out of `agent`.
- Aliases in `agent` stay until no caller uses the old names; removing them is a later cleanup.
- Twelve pull requests of moves will touch busy files (`batch.go`, `api.go`, `options.go`); they
  should land between feature work, not under it.

## Open questions for the owner

1. **adanext-map**: which repository and contract does #574 mean? Nothing here references it.
2. Should `server/` keep a `replace` to the root during step 12, or copy the wire contracts now?
3. `purpose` labels prompts with coding-flavoured kinds: kernel leaf, or coding-pack data?
4. Is `game` reading batch and eval records allowed as a permanent cross-pack edge?
5. `ws` mixes the generic slot lease with issue leases and git: split it in this plan, or later?
6. Does extraction of `watch` (with Ada) matter enough to plan, or should it stay in harness?
