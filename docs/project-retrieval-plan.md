# Project retrieval and measured LLM savings

Status: landed on main on 2026-10-14 through the `durable` feature branch (#142). Increments 1–3 and the increment 4 fixture/comparison interface are implemented and opt-in; the real held-out pilot is not complete. Increments 5–6 remain gated. The implementation evidence is in PLAN.md's Decisions log entries for #140 and #141 and in the synthetic fixtures under `internal/eval/testdata/`. This document does not enable retrieval or authorize a deployment.

## Objective and decision

Help Harness complete recurring project tasks with less repeated investigation and fewer unsuccessful attempts. Measure total model usage and known cost per successful task while preserving correctness and project isolation.

Start with approved project knowledge stored in PostgreSQL and retrieved through deterministic filters, exact identifier matching and full-text search. Add pgvector only after an experiment demonstrates a benefit over this baseline. Keep local CLI operation independent of a server, PostgreSQL and embedding services.

This plan expands section 10 of [the durable-worker specification](durable-workers-spec.md#10-automatic-memory-with-evaluation-before-promotion). Its promotion thresholds, append-only history, 8 KiB context limit and project boundaries remain applicable.

## Implementation starting point

The inspected development checkout contains PostgreSQL projects, jobs, attempts, checkpoints and events in `server/platform/`; the separate server module already uses pgx. The root module remains dependency-free. Evaluated project memory is still listed as unfinished in `server/README.md`.

Existing integration surfaces include `internal/agent`, `internal/runlog`, `internal/batch`, `internal/eval`, `internal/report`, `internal/remote` and `internal/hrnmcp`. Verify their current versions before implementation: this checkout contains uncommitted durable-worker work, and the local CLI checkout can have newer continuation features. Do not copy one checkout over the other.

Remote execution currently uses a fake model. Retrieval storage and protocol tests can proceed independently; this plan does not make live remote execution available. End-to-end savings must be measured on an actually supported execution route.

## Proposed workflow

1. Receive a task with an authorized tenant/project identity and selected backend/model.
2. Resolve and pin an approved project knowledge version.
3. Build a search query from the task and, for a repair attempt, the bounded real check failure. Initial query construction uses deterministic code, with no model call.
4. Filter eligible records by authorization, pinned version and applicability before returning any content. Rank exact identifiers and full-text matches, deduplicate, and select a small packet.
5. Attach source-labelled context to the new task or repair prompt. Preserve the original task, acceptance checks and current workspace evidence.
6. Run the configured assistant and existing checks. Record exactly which context was supplied, its size, retrieval latency and the resulting usage.
7. Completed-run evidence may later produce a candidate lesson. Candidates become retrievable only through the explicit approval or evaluated-promotion process below.

Only the trusted control service accesses PostgreSQL. Workers and opted-in local clients obtain authorized context through the service. A model-supplied project ID is never sufficient authorization.

## Knowledge and storage contracts

Start with architecture decisions, build/test instructions, verified fixes and concise verified run summaries. Import only explicitly selected project sources. Do not bulk-import entire transcripts, credentials or unrelated repositories. A failed run can describe a failure; it cannot establish that its proposed solution works.

Proposed additive tables in the next available server migration:

| Table | Responsibility |
| --- | --- |
| `knowledge_entries` | Immutable content revisions, tenant/project scope, kind, source references and digests, repository revision, backend/model applicability, validation evidence and creation time |
| `knowledge_versions` and membership | Immutable sets of approved entry revisions; retain evaluation references |
| `knowledge_active_versions` | Transactionally updated active version for a project and applicable backend/model scope |
| `retrieval_records` | Job/run and attempt references, pinned version, query digest, selected entry IDs, ordering, packet digest, byte size, timing and empty/error reason |
| `knowledge_evaluations` | Dataset/version identity, candidate and baseline versions, settings, run references, measurements and promotion decision |

Use composite tenant/project constraints and scoped queries for all access. Separate curated-source approval from automatic lesson promotion. Allow an authorized operator to seed a version from reviewed project documentation; a generated lesson must pass evaluation before automatic promotion.

Index eligible content with PostgreSQL `tsvector` and a GIN index. Use the `simple` text configuration initially for technical vocabulary, plus explicit matching for paths, symbols and error codes that tokenization can split. Fix ranking weights and tie-breaking in a versioned retrieval configuration. An empty or weak result must be allowed to return no context.

Proposed service operations: search/preview context, inspect sources, import a candidate, inspect an evaluation, activate a version, disable retrieval and roll back. Final endpoint and CLI names should follow existing project conventions. Authenticated identity is resolved by the server; client filters can narrow its allowed scope but never widen it.

## Context and continuity

- Initial defaults: at most five deduplicated excerpts, with the entire injected packet capped at 8 KiB including labels and source references. These are proposed tuning defaults; measure them.
- Record UTF-8 bytes exactly. Record token counts only when a compatible tokenizer or provider reports them; otherwise label estimates and missing data explicitly.
- Label retrieved content as reference material. It cannot modify permissions, spending limits, tenant/project scope or acceptance criteria. Delimiters and secret checks reduce risk but do not establish immunity to prompt injection; enforce policy outside the model.
- Include source revision and verification evidence. Exclude superseded entries and enforce explicit applicability rules. Advice from another revision requires an applicability check against the current checkout; age alone is not proof of correctness.
- Persist the selected packet or immutable content references and hashes before execution. Pin the knowledge version for the run/session. A new repair attempt can retrieve additional excerpts from that version using its new failure evidence and must record its own packet.
- Resume with the recorded packet and version. Do not repeat retrieval silently, duplicate the same context on every turn, or rewrite committed assistant messages or signed reasoning blocks. If the required pinned context cannot be recovered, report an explicit resume error.
- Add context only at a supported new-input boundary for each adapter. Verify API continuation, native session continuation and cross-backend handoff independently where those features exist. Do not claim all adapters have identical continuation behavior.
- Proposed modes are `off`, `optional` and `required`. In optional mode, timeout/unavailability yields a recorded empty packet and execution can proceed. Required mode stops before execution if retrieval fails. Unauthorized access always returns an error; it never falls back to broader scope.

## Delivery increments and acceptance gates

### 1. Baseline and evaluation fixtures

Extend existing run/evaluation reporting rather than creating an independent benchmark engine. Select at least ten eligible held-out tasks with deterministic checks and three repeats per variant, using a fixed project revision, backend/model, settings and limits. Include recurring errors, architecture questions, exact symbols and cases where retrieval should return nothing. Keep task solutions out of the knowledge corpus.

Start with retrieval disabled and record input/output/cache usage, calls where observable, tool activity, attempts, latency and final check outcome. Unknown native usage stays unknown. Establish per-backend usage semantics before comparing counts. Fixture runs prove wiring; they do not prove LLM savings.

Acceptance: repeatable task manifest, captured baselines, explicit usage coverage and an agreed bounded budget before metered experiments. Use configured subscription profiles when suitable; never silently switch billing accounts or providers.

### 2. PostgreSQL knowledge and text search

Implement migrations, immutable entries/versions, reviewed seed import, scoped search and inspect/disable/rollback operations. Add a bounded HTTP client and a root-module retrieval contract without adding dependencies to the root module. Search requires no model calls.

Acceptance: real PostgreSQL integration tests for cross-project and cross-tenant denial, invalid IDs, version membership, deterministic ranking, no-match results, superseded entries, idempotent ingestion and transactional activation. Verify search queries do not hold locks needed for job claims or heartbeats.

### 3. Task and repair integration

Add optional retrieval configuration to the supported CLI/MCP/batch entry points using existing option validation. Preserve original task text separately from the composed prompt and record the context packet. Add fake-backend tests for prompt composition, failed-check retrieval, bounded size, deduplication, timeouts and required/optional behavior. Keep job checkpoints and native histories authoritative for execution continuity.

Acceptance: retrieval off reproduces existing behavior; context reaches each claimed adapter exactly once at the intended input boundary; resumed work uses its pinned content; malicious reference text cannot change enforced permissions. Run root and server module checks separately, plus real PostgreSQL integration tests. Respect the repository's Windows/Linux race-test constraints.

### 4. Controlled pilot and savings report

Compare retrieval off against text retrieval on the same held-out tasks. Counterbalance run order and record cache conditions. Use the existing evaluation runner's no-retry matrix for initial capability comparisons; add a separately labelled workflow experiment with the same finite retry policy in both variants to measure savings from fewer attempts.

Acceptance: no new deterministic correctness or isolation failures, plus the promotion criterion below. If results are inconclusive, retain an opt-in pilot and collect more evidence. Increase task coverage and repeats before making broad claims.

### 5. Evaluated memory consolidation

After the pilot is useful, add a low-priority, idempotent consolidation job that runs only when new eligible completed-run evidence exists. Begin with structured extraction of task, failure, applied change and passing checks. Any model-based summarization is separately configured, budgeted and counted.

Store candidates with source runs, revision, backend/model, extraction version and evaluation evidence. Compare the candidate version with the active version on held-out tasks. Promote atomically, retain the prior version, and provide inspect, disable and rollback controls. Automatically revert only when repeated controlled evaluation reproduces a deterministic regression attributable to the new version.

Acceptance: rejected/pending candidates never enter active retrieval; duplicate source events do not create duplicate lessons; concurrent promotion is safe; running sessions retain their version; rollback and retention preserve active-run provenance.

### 6. Optional pgvector experiment

Proceed when labelled retrieval examples show useful knowledge missed because the task and source use different wording, or text retrieval leaves measurable repeated investigation. Retain text search as the baseline and use the same PostgreSQL service.

Choose an embedding model using project data and deployment/privacy constraints. Cache embeddings by tenant/project, normalized content digest, model ID/version, dimension and preprocessing version. Generate embeddings only for changed eligible content; separately count query embedding work. Store embeddings in versioned rows without mutating source entries. Do not compare vectors from incompatible model versions.

Start with exact vector search on the authorized candidate set. Combine text and vector rankings, for example with reciprocal rank fusion. Keep all scope and applicability filters mandatory, then apply the same context budget. Avoid an LLM reranker initially. Add an approximate index only if measured size/latency requires it; test recall under tenant/project filters.

Acceptance: hybrid retrieval beats text retrieval under the same quality and net-usage criteria after counting embedding preparation, query cost and any summarization. Otherwise keep vectors disabled. Extension installation, a new embedding service or a migration to the deployed database is a separate implementation/rollout step.

## Savings and promotion measurements

Report these independently:

- Task success rate and deterministic regressions, including isolation failures.
- Per-task input/output tokens and provider cache categories, with coverage and estimation flags. Avoid double-counting backend totals that already include cached tokens.
- Total known model tokens across all attempts divided by the number of successfully completed tasks. Include failed tasks in the numerator. If there are zero successes, report the ratio as undefined.
- Total known model cost across all tasks divided by successes, including failed attempts, online retrieval calls and consolidation/embedding preparation. Also show preparation cost separately and its amortized cost at explicit reuse counts. Embedding tokens and generation tokens have different prices; do not present their raw sum as monetary savings.
- Reduction between baseline and candidate median model-token usage over the same complete comparable task-repeat cells for the promotion gate. Median paired percentage savings is a separate diagnostic. Also report p50/p95 latency, attempts, retrieval size and relevant-context hit rate.
- Cached/uncached usage and actual versus estimated prices. Subscription usage improvements do not imply a lower subscription bill. Missing usage prevents a token-saving claim for that variant.

The existing minimum promotion policy is ten tasks and three repeats: reject new deterministic correctness or isolation failures; require higher aggregate success, or equal success with at least 10% lower median model-token usage when usage is comparable. For the stated cost-saving objective, also require the full-workflow cost-per-success report to support the savings claim after preparation overhead. These are pilot thresholds, not statistical guarantees.

For repeated use, the break-even calculation is:

`preparation cost / (baseline generation cost per comparable task - retrieval-enabled generation cost per task - online retrieval cost per task)`

Use this only when the denominator is positive and task quality is comparable. Show first-run and steady-state results separately.

## Rollout and completion

Deliver increments 1–4 as the first useful release: curated PostgreSQL retrieval with a measured pilot. Increments 5–6 remain separate gates. Enable one selected project/backend first, provide a visible retrieval record and disable control, then expand only after the comparison passes. Bound search time and concurrency independently from the job queue so retrieval load cannot starve lease maintenance.

Complete the first release when an engineer can inspect why context was selected, reproduce the selected version, compare runs with retrieval off/on, and disable the feature without changing the original task. Preserve the existing human decision over merge and deployment.

Next gated stage: bind reviewed corpus provenance and deterministic check/isolation evidence to real held-out runs, then conduct an explicitly authorized pilot with counterbalanced cache conditions. The comparison interface validates local run identity but labels measured inputs supplied/unverified and cannot support measured-savings claims until that binding exists. No real savings pilot, automatic consolidation or vector implementation is complete.

## References

- [Accepted durable-worker specification](durable-workers-spec.md)
- Current server scope and limitations: `server/README.md` (issue #137)
- [PostgreSQL full-text search](https://www.postgresql.org/docs/current/textsearch-intro.html)
- [pgvector hybrid search and filtering](https://github.com/pgvector/pgvector#hybrid-search)
