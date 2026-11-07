# Repository extraction: plugins first, repositories second

[← README](../README.md) · [ADR 0001](adr/0001-capability-kernel-boundaries.md)

Status: draft, 2026-11-07, for the extraction part of #574 under epic #580 (workstream A),
tracked by #878. This
spec turns ADR 0001's extraction table into a plugin model, a contract inventory and an ordered
set of issues. Nothing here changes ADR 0001's rules; it adds the mechanism and the gates.

## Objective and decision

Anyone should be able to take one part of hrn and use it on its own, in two senses:

- **Run it standalone**: a part is a program with its own release, and hrn finds, installs and
  runs it the same way a stranger's program would be found, installed and run.
- **Import it as code**: the types a part speaks are a small, public, versioned Go module that any
  repository can import, including ours.

The decision is **plugins first, repositories second**. A part leaves this repository the PR after
hrn loads it as a plugin, never before. Measured on `main` at `056c21f` (2026-11-07), a split today
would save nothing and cost coordination: CI wall time is the root test suite (Windows 11.5 min,
Linux 9 min; the server jobs finish in under 3 min in parallel), the server and hrn must ship from
one revision (worker protocol v1 decodes checkpoints strictly), the knowledge fixtures are copied
between the two modules with identity tests, and 13 of the last 179 merged PRs touched both. No
`v*` tag or release exists yet, so no contract has a version anchor. The plugin model removes each
of those couplings before the move, so the move itself is a history filter and a module path.

## How hrn already does this

hrn is a plugin host in three places, each with a different protocol:

| Today | Mechanism | Contract |
|---|---|---|
| Backends (claude, codex, gemini, opencode, agy) | external program on `PATH`, stdio | each CLI's own output; adapters in `internal/agent` |
| adanext-exec, supervisors | `hrn exec --protocol v1`, JSONL on stdio | [exec protocol v1](exec-protocol.md): `hello`, versioned, additive |
| MCP servers and hosts | JSON-RPC on stdio | [MCP](mcp.md); `hrn mcp` is a server too |
| Knowledge service, durable jobs | HTTP `/v1/...` | wire types in `internal/retrieval` and `server/platform`, no schema version |
| Readers of hrn's data | files and `--json` | run records (additive), `hrn bot stats --json` (versioned); the other `--json` outputs unversioned |

The exec protocol's compatibility rule is the model for every contract below: a version grows only
by adding optional fields; a reader ignores fields it does not know; anything else is a new version
negotiated in `hello`.

## The plugin model

A **plugin** is a program `hrn-<name>` that hrn starts as a subprocess and speaks to over JSONL on
stdio, the same framing as the exec protocol. Go's own `plugin` package is not used: it is Linux
only and needs an identical toolchain. In-process loading is never required.

- **Discovery**: `$HRN_HOME/plugins/<name>/hrn-<name>[.exe]`, then `hrn-<name>` on `PATH`, in that
  order, as git and kubectl find subcommands. A plugin named in a run but not found fails the run
  naming the plugin and the install command, under the zero-provider rule of ADR 0001.
- **Handshake**: hrn sends `hello` with its version and the protocol versions it speaks; the plugin
  answers with its name, version, the protocol version it chose and the capabilities it offers
  (`search`, `knowledge`, `dashboard`, ...). A plugin that answers nothing hrn understands is
  refused before any task data is sent.
- **Install**: `hrn plugin install <name>[@version]` downloads the release asset for the platform
  from the plugin's repository and checks its `SHA256SUMS`, exactly as `hrn update` does for hrn
  (`internal/update`). `hrn plugin list` shows installed plugins, versions and what they offer. No
  plugin is installed without being asked.
- **Policy**: a plugin runs under the same command guard and child-environment filter
  (`internal/envpolicy`) as every child hrn starts; credentials reach it only through its filtered
  environment. A plugin never gets `allow_dangerous` or MCP servers hrn itself would refuse.
- **Trust**: an installed plugin is any program; hrn records its name, version and checksum in the
  run record so a run says which plugin produced which evidence.

### Import as code: the contracts module

The plugin protocol's Go types live in **`contracts/`, a nested module in this repository**
(`github.com/cristiangirlea/harness/contracts`), standard library only, the same arrangement
`server/` already has. Anyone imports it with one `go get`; a plugin author in another repository
gets the frames, the `hello` negotiation and the JSON golden fixtures without hrn itself. It holds
only wire types and their golden JSON, never behaviour.

The root module keeps its no-`require` rule and does not import `contracts/`: the types it needs
are copied, and an identity test (as `internal/retrieval/fixture_copies_test.go` does today for
the knowledge fixtures) fails when the copies drift. The owner may instead allow a `require` of
the repository's own nested modules; that is decision (b) below, and the rest of this spec is the
same either way.

## Contract inventory

Each contract gets a version, a compatibility rule and golden fixtures run by both sides before the
part behind it leaves.

| # | Contract | Today | Needed |
|---|---|---|---|
| C1 | exec protocol | v1, documented, negotiated | nothing; it is the model |
| C2 | worker protocol | v1, same-revision rule, strict checkpoint decoding | a `version` on the checkpoint and result frames; the server reads the previous version for one release; the worker image installs a released hrn, not the checkout |
| C3 | knowledge wire (`/v1/knowledge/*`) | knowledge `version_id` and `RankingVersion`, no schema version | a schema version on request and packet, the exec rule, fixtures owned by the service |
| C4 | job API (`/v1/projects/.../jobs`, `/v1/jobs/...`) | `JobOptions`, `JobResult`, strict decoding | the same as C2 and C3 |
| C5 | run records on disk | additive `omitempty`, documented | a `record_version` field, written now, read leniently |
| C6 | `hrn --json` outputs (`show`, `flow`, `batch show`, `eval diff`, `report`, `ws`, `retrieval`) | unversioned, except `bot stats` | a `version` in each object, as `bot stats` has; one page in `docs/` listing them |
| C7 | plugin protocol | does not exist | the model above, v1, in `contracts/` |
| C8 | releases | `Version` 0.2.0 in code, no tag, `hrn update` reads GitHub's latest release | the first tag; from then on every part's release is a tag with assets and `SHA256SUMS` |

## Candidates, in order

| Part | Plugin shape | Gates | Status |
|---|---|---|---|
| **search** (#572, program synthesis, 165 lines, no harness imports) | `hrn-search`, capability `search` | C7, C8 | born in its own repository as the first plugin; nothing to extract |
| **server** (control service, worker, charts, 4,180 lines) | not a plugin: a service hrn talks to over HTTP, and a worker that runs hrn | C2, C3, C4, C8, infra `P4` reaching the cluster | Go decoupled since PR #765; waits for C2–C4 and the deployment |
| **knowledge** (`/v1/knowledge/*` inside the server) | stays in the server unless a second store appears | C3 | not planned separately |
| **watch** (dashboard and Ada, 6,529 lines and assets) | `hrn-watch`, capability `dashboard`, reading records and `--json` | C5, C6, C7, and `watch` importing only leaf and kernel packages (today it calls `agent`, `batch`, `runlog`, `game`, `flow` and `workmap` directly, about 170 references) | furthest away; ADR 0001 open question 5 |
| **game** | data read by watch | C5 | after watch |
| **eval** | `hrn eval`, depends on batch | batch's result format in C6 | last, if ever |

Kept in harness, as ADR 0001 says: the kernel, the provider adapters, the coding pack, `cmd/hrn`,
`hrnmcp`, `execproto`, and now the plugin host.

## Move procedure

For every part, one PR in each repository, in this order, each with CI green:

1. The part answers the plugin handshake (or, for the server, speaks the versioned contracts) while
   still in this repository; harness CI runs it as a plugin from the build, not as an import.
2. The new repository is created from a history filter of the part's paths (`git filter-repo`,
   kept paths only), with its own AGENTS.md, CI on GitHub's runners (public), release workflow and
   a first tag. Its tests include the golden fixtures of every contract it speaks, pinned to the
   contracts version.
3. harness removes the code and adds the plugin to its CI matrix at the pinned release, so the
   harness test suite still exercises the plugin. harness CI never depends on the other repository
   being green: a broken plugin release is a pin that does not move.
4. Rollback is a revert of step 3 for one release; the code is still in history.

Version policy across repositories: hrn keeps one `Version`; each plugin has its own; `hello`
carries both and the protocol version; a reader accepts the previous protocol version for one
release. A compatibility table in the contracts module's README names which hrn releases each
protocol version shipped with.

## Non-goals

No microservices, no network boundary where a subprocess works, no in-process plugin loading, no
rewrite of a part to extract it, no `require` of a third-party module in the root module. A
repository is not created to hold a design; it is created when a plugin release can be installed.

## Decisions for the owner

- (a) Repository names. The epic lists candidates (`hrn-search`, `hrn-contracts`, `hrn-exec`,
  `hrn-memory`, `hrn-mcp`, Ada); this spec needs two now: the search plugin and, later, the server.
- (b) The contracts module: nested in this repository and copied into the root (recommended, keeps
  the no-`require` rule), or allowed as a `require` of the repository's own modules.
- (c) The search pilot (#572): born in its own repository as the first plugin (recommended: no
  code in harness depends on it, and it proves the model with the smallest surface), or landed in
  `internal/search` first. #568's registry overlaps the kernel ADR 0001 delivered; it needs a
  decision to rebase onto `internal/kernel` or to close.
- (d) Whether watch ever leaves (ADR 0001 open question 5). This spec keeps it until C5–C7 exist.
- (e) The trigger for the server's move: the first real deployment (infra `P4`), a second consumer
  of its API, or a worker protocol v2.

## Issues of the sub-epic

Each issue is one branch and one PR under AGENTS.md's rules; titles follow the repository's form.
The epic issue #878 links #580, #574, #555 and #556 and lists these in order.

1. #878 **epic: repository extraction, plugins first** — this spec's tracker; done when every issue
   below is closed or decided against, with the decision logged in PLAN.md.
2. #879 **release: tag v0.2.0 and prove the release workflow and hrn update** (C8) — the release
   workflow has never run; `hrn update` has nothing to find. Tagging is the owner's step; the
   issue covers the dry run, the asset names `hrn update` expects and the install test.
3. #880 **contracts: the plugin protocol v1 and the contracts module** (C7) — `contracts/` with
   `hello`, frames, errors, golden JSON, a README with the compatibility table; the root copies and
   identity test; `docs/plugins.md`.
4. #881 **plugins: discovery, handshake, install and list in hrn** — `hrn plugin install|list`,
   `$HRN_HOME/plugins`, `PATH` lookup, policy and run-record fields; a `TestHelper` plugin in
   tests; the zero-provider test gains the missing-plugin path.
5. #882 **search: the first plugin in its own repository** (#572, decision (c)) — the synthesizer
   behind capability `search`, its repository, CI, first release; harness CI runs it from the
   pinned release; a benchmark gate as #558 asks.
6. #883 **records: a version on run records and on every --json output** (C5, C6) — written now, read
   leniently, listed in one `docs/` page; `bot stats` is the model.
7. #884 **worker: a versioned checkpoint and result frame, and a worker image from a released hrn**
   (C2, C4) — ends the same-revision rule; the server reads the previous version for one release;
   golden transcripts on both sides.
8. #885 **knowledge: a schema version on the wire and fixtures owned by the service** (C3) — the
   request and packet carry a version; `internal/retrieval` and `internal/remote` replay the
   service's fixtures at a pinned version instead of byte-identical copies.
9. #886 **server: move to its own repository** — the move procedure above; gated on 2, 7, 8 and
   decision (e); updates infra's `P4` reference and the deploy README.
10. #887 **watch: read only records and --json** — the archtest layer change for `watch`, replacing
    the direct calls with record reads and `hrn ... --json`; the precondition for a later
    `hrn-watch`, done or decided against under (d).
11. #888 **docs: ADR 0001 status, triggers and this spec** — a Markdown-only PR: the extraction table
    gains a status column and the triggers; open question 5 gets its answer; README indexes this
    spec.

Order: 2 and 3 first, in parallel; 4 after 3; 5 after 4; 6, 7 and 8 independent after 3; 9 after
7 and 8 and the trigger; 10 any time after 6; 11 with the first merged step.

## References

- [ADR 0001](adr/0001-capability-kernel-boundaries.md): layers, archtest, the extraction table.
- [Exec protocol v1](exec-protocol.md) and [worker protocol v1](worker-protocol.md).
- [Run records](run-records.md), [Experience](experience.md) (`bot stats --json`, the versioned
  reader model).
- [Durable workers spec](durable-workers-spec.md) and infra `PLAN.md` item P4.
- Issues #580 (epic), #574, #555, #556, #558; PRs #568, #572, #765 (server off the root module).
