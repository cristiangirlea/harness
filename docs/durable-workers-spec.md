# Implement durable project workers, tenant isolation, KEDA scaling, and evaluated memory in harness

You are working directly in my harness repository. This is an implementation request: inspect the current checkout, implement the changes, test them, and leave reviewable code and evidence. Do not stop after producing another architecture proposal.

## 1. Objective and intended use

Evolve the existing hrn Go CLI into a reliable execution platform that I can run on my own Linux server now and later offer to multiple tenants.

The system must:

- Keep accepted tasks running independently of the submitting laptop or HTTP connection.
- Preserve project workspaces, session history, and useful context across task boundaries and pod replacement.
- Recover from supported interruption points without silently repeating uncertain side effects.
- Run untrusted repository code and tools inside hardened project execution containers.
- Use Kubernetes and KEDA to start project workers on demand and scale idle workers to zero.
- Support all five existing backends: api, claude, codex, gemini, and opencode.
- Reuse verified lessons from prior runs, with automatic memory promotion only after evaluation.
- Keep operational complexity and model spending controlled and measurable.

The first acceptance milestone is an end-to-end durable execution slice: submit a task, disconnect the client, restart the control service, terminate a worker, and recover the correct task and workspace with an honest account of anything that cannot safely be repeated. Prove this before expanding the complete feature set.

Complete the later increments too; the first milestone is sequencing, not permission to silently reduce the final scope.

## 2. Working rules and authorization

Start by reading applicable AGENTS.md files, README.md, PLAN.md, module definitions, current CI, and the relevant implementation. Inspect Git status, branch, remotes, and recent history. Preserve unrelated work and verify that this is the intended repository before editing it.

The earlier review used cristiangirlea/harness at commit 634507a8f90bc615348da4b0fb1dd6fadd2025ac. Treat that as historical orientation, not as a requirement to reset, downgrade, or replace the current checkout. Reconcile this prompt with the current code and reuse anything already implemented.

You are authorized to make local implementation changes and run appropriate local tests, builds, and disposable development fixtures. Keep work within this repository and task-owned temporary environments. Do not alter existing production services, shared Kubernetes clusters, real user repositories, or existing databases as test fixtures.

Do not push, open pull requests, merge, tag, publish, deploy to my server, change DNS, delete production data, or make account changes without separate explicit authorization. Leave changes uncommitted unless I separately authorize commits. These boundaries override any older repository workflow that automatically publishes or merges increments.

Preserve the configured Git author identity. Do not add Co-Authored-By, Assisted-by, generated-by, or similar attribution to code, documentation, commits, or proposed PR text.

Continue autonomously through routine implementation decisions. Do not ask me to choose internal names or approve every reversible edit. Ask only for missing information that materially blocks safe progress, necessary credentials or spending limits, or actions outside the authorization above. Finish unaffected work while a genuine blocker remains.

Use current official documentation and locally installed CLI help to verify version-sensitive behavior. Do not assume that remembered flags, event formats, authentication paths, or session formats are still correct.

## 3. Existing contracts and architecture constraints

Inspect and extend the current implementation rather than introducing a second unrelated harness:

- cmd/hrn: existing CLI commands and rendering.
- internal/agent: Backend, Task, Event, Result, shared Options, the API loop, and native CLI adapters.
- internal/tools: filesystem tools, shell execution, process cleanup, and command guard.
- internal/runlog: task, event, result, and conversation records.
- internal/batch: parallel worktrees, gates, retries, commits, and batch records.
- internal/eval: evaluation matrices, scoring, comparison, and reporting.
- internal/mcp and internal/hrnmcp: existing client/server and delegation behavior.

Preserve the current local CLI. Running hrn locally must not start requiring Kubernetes, PostgreSQL, or a server connection.

Keep the root Go module dependency-free. A separate server/ Go module may use maintained PostgreSQL and Kubernetes clients, preferably pgx and client-go. This is an intentional exception for the new server module only. Do not hand-write a database driver to preserve a zero-dependency claim. Use explicit module boundaries and test both modules; root go test ./... does not automatically test a nested module.

Continue using agent.Backend, runlog, batch results, and the existing evaluation system. Add optional fields compatibly, with omitempty where appropriate. Historical run, batch, and evaluation records must remain readable.

Keep shared agent options consistent across CLI flags, batch specifications, MCP schemas, and remote submission. Do not create divergent copies of the option definitions.

Preserve append-only API message history. Never rewrite, reorder, summarize in place, or remove previously committed assistant messages and signed reasoning blocks. Memory is additional versioned context, not a rewrite of a prior conversation.

Separate orchestration lifecycle states from agent execution results. A paused job, pending approval, or recovery request must not be presented as a successful completed run.

## 4. Target architecture and defaults

Implement these relationships:

    Control service + scheduler + PostgreSQL
        -> tenant
            -> project
                -> KEDA ScaledObject
                -> project StatefulSet, zero or one pod
                -> retained persistent project volume
                -> isolated task worktrees and native sessions

The control service and PostgreSQL stay running. Project workers scale to zero.

Use one namespace per tenant. Each project gets its own StatefulSet, persistent storage, credentials, execution policy, and scaling object. A tenant can own multiple projects. All tenancy and project ownership checks are enforced server-side.

A project pod contains a trusted coordinator and a separate execution container. API reasoning and durable coordination belong in the trusted component; filesystem tools, shell commands, native CLIs, MCP subprocesses, and gates execute in the execution container. Treat execution-container output as untrusted data.

Native CLI backends own their internal model/tool loops. Do not claim that the harness can split those internals in the same way as its own API loop.

Initial scheduling defaults:

- Maximum running tasks across the installation: 2.
- Maximum running tasks per tenant: 2.
- Maximum running tasks per project: 2, in separate worktrees and session directories.
- Maximum project StatefulSet replicas: 1.
- Minimum project StatefulSet replicas: 0.
- KEDA polling interval: 10 seconds.
- Idle cooldown: 300 seconds.
- Worker heartbeat interval: 10 seconds.
- Ownership lease duration: 60 seconds.
- Kubernetes termination grace period: 120 seconds.

Make these explicit configuration values with validation. Preserve the existing local batch behavior. Respect a stricter backend/account concurrency limit when a native CLI or credential profile cannot safely support simultaneous sessions.

Two task slots and a five-minute cooldown are provisional defaults to benchmark, not claimed optimal settings.

Provide explicit resource requests, limits, volume quotas, and a documented small-server deployment profile. Derive final resource sizing from measured fixtures and report the tested hardware. Reject obviously invalid configurations; do not assume unlimited host memory, disk, or provider concurrency.

## 5. Durable queue, ownership, and storage

Use PostgreSQL as the authoritative store for tenants, projects, job admission, attempts, ownership, durable events, checkpoint metadata, approvals, usage reservations, and memory versions.

Use PostgreSQL transactional claims with row locking and SKIP LOCKED where appropriate. Do not add Redis or a separate message broker in the first implementation.

Use persistent project volumes for repository objects, worktrees, untracked workspace files, native CLI session data, and caches. Retain volumes when pods scale down, restart, or are upgraded. Keep durable run data distinct from disposable caches.

Define and document an explicit job state machine covering at least queued, admitted, running, waiting for approval, recovery required, succeeded, failed, and canceled. Document valid transitions and which transitions release a scheduling slot.

Required invariants:

- An accepted submission survives a control-service crash: commit it before acknowledging it.
- Idempotency keys are scoped to tenant and project. A repeat with the same content returns the existing job; conflicting content is rejected.
- Only the current attempt owner may advance a job or append authoritative execution results.
- Ownership includes a monotonically changing generation or equivalent fencing token.
- Expired ownership is not proof that the previous process stopped.
- Before reassignment, confirm that the previous executor is terminated or otherwise unable to continue. If this cannot be established, require recovery instead of starting a second writer.
- Deleting a Pod object or losing contact with a node is not sufficient proof of termination. Test this explicitly under node/network partition.
- On renewal failure, stop issuing new model/tool operations and stop execution before the lease expires. Reconcile any in-flight operation conservatively.
- Enforce concurrency and budget admission transactionally, including concurrent submissions and controller restarts.
- Use fair scheduling across tenants and FIFO ordering within a project. Run memory evaluation as lower-priority work within the same resource and spending limits.

Bound the time an admitted job can reserve capacity without a worker starting: default five minutes, configurable. On expiry, release the reservation transactionally and report the startup failure; use bounded retry/backoff so an unschedulable project cannot permanently block unrelated projects.

The trusted control plane accesses PostgreSQL. Execution containers must not receive database credentials. Worker credentials are scoped to their project and permitted operations.

Use immutable, versioned checkpoint records with checksums and monotonically ordered events. Store large payloads outside hot queue rows. For local file persistence, use a single writer, an append journal, flushes at durable boundaries, and atomic snapshot replacement where supported. Handle write and sync failures explicitly.

Avoid claiming that a database transaction and a filesystem mutation are atomic together. Use checkpoint manifests and operation records to identify incomplete boundaries and reconcile them.

Use versioned, repeatable database migrations with startup compatibility checks. Document upgrade, rollback limitations, backup, and restoration. Do not auto-run destructive migrations against an existing database.

## 6. Recovery and project continuity

For the API backend, persist:

1. The request intent and spending reservation before a model request.
2. A complete model response before dispatching any requested tools.
3. Each tool invocation intent before execution.
4. Each confirmed tool outcome immediately after execution.
5. The resulting conversation/checkpoint before advancing to the next model turn.

Streaming fragments can remain transient, but reconnect behavior must clearly distinguish transient output from durable events. Committed tool/model boundaries must not disappear after process failure.

A checkpoint must identify the exact backend/model and relevant CLI version, settings, deadline, cumulative usage, reservations, approval decisions, worktree/base commit, native session, and memory version.

Restore the actual workspace, including relevant untracked files and worktree metadata. Conversation history alone is not sufficient recovery.

Do not promise exactly-once execution of arbitrary shell commands, HTTP calls, commits, or provider requests. If a process dies after an effect could have happened but before its outcome is durably known, preserve the uncertainty and enter recovery_required. A timeout or missing output is not proof that nothing happened.

Permit automatic replay only when an operation is verified to be side-effect-free or has a tested idempotency mechanism. Treat generic shell and MCP execution as potentially side-effecting.

Recovery must preserve spent budget and unresolved reservations. It must not reset the clock, retry count, cost ceiling, or approval state to obtain a fresh allowance.

The API checkpoint store is the trust boundary for recovery. It must commit before acknowledging saves, reject stale owners, and preserve immutable versions. The task digest binds task settings; it does not authenticate checkpoint `Result.CostUSD`, `Deadline`, or `Started`. Recovery trusts these stored values, so changing them can reset spent budget or the clock. Untrusted execution tools and workspace/model content must not be able to modify stored checkpoints or access store credentials.

The API cost ceiling gates new model requests. A restored, already paid response may finish its requested tools even when its cost has reached the ceiling; committed tool outcomes are skipped and no further model request starts. The original deadline still applies to those tools.

A deadline or cancellation between confirmed operations reports `timeout` or `canceled` without `recovery_required` and leaves the checkpoint resumable. A definite, non-retryable HTTP 4xx clears the pending model operation at a `model_rejected` checkpoint and reports an error. A context-management 400 instead permits one retry without context management, with a new saved model intent. Retryable HTTP errors, interrupted requests, and failed persistence acknowledgements remain uncertain. Every newly handled tool call emits a call event before its result, including unknown, guard-refused, invalid-input, and permission-denied calls; durable `tool_calls` counts committed outcomes.

For native CLI backends:

- Capture explicit session identifiers from verified output.
- Persist the supported native session directories at stable paths.
- Resume the requested identifier, never an ambiguous global latest session.
- Prevent session collisions between concurrent tasks and projects.
- Test authentication refresh and concurrent credential-cache access.
- Add Gemini streaming if the pinned version supports the required event format.
- Preserve stdout/stderr and native exit information without exposing credentials.
- State the tested recovery guarantees separately for each backend. Unsupported or ambiguous continuation must produce an actionable error, not silently start a fresh session.

New independent tasks use separate worktrees from an explicit base revision. Explicit continuation resumes the selected session/worktree. Never automatically merge concurrent task branches or incorporate unfinished edits into a different task.

Carry information between tasks through scoped project memory, verified run summaries, explicit predecessor references, and retained artifacts. A shared pod is not the source of truth for continuity.

## 7. Execution isolation and tenant boundaries

Apply Kubernetes restricted pod security controls: non-root execution, no privilege escalation, dropped capabilities, an appropriate seccomp profile, read-only image filesystems where feasible, and explicit writable mounts.

Do not mount host homes, host repositories outside the assigned project, Docker/containerd sockets, or broad Kubernetes credentials. Disable automatic service-account token mounting on execution pods unless a narrowly justified component requires a separately scoped credential.

Use separate process namespaces for trusted coordination and execution. Keep control credentials and API-backend provider keys out of execution-container mounts and environment.

Native CLIs need their own provider credentials inside their execution environment. Scope those to the configured tenant/project/account, document that exposure honestly, and never share the owner's account implicitly across tenants.

Use default-deny network policy with a supporting network-policy implementation. Allow only required DNS, control endpoints, and configured outbound destinations. Implement domain-aware outbound restrictions through a controlled egress proxy or an explicitly configured policy-capable network layer; ordinary NetworkPolicy alone does not enforce arbitrary hostname rules.

Block unintended access to cloud metadata, other project services, cluster administration endpoints, databases, and private networks. Apply equivalent controls to MCP tools and gate commands.

Use role-based control-plane authorization and quotas. IDs supplied by a client are not authorization. Include tenant/project scoping in all relevant queries, object references, logs, events, artifacts, memory access, and worker RPCs.

Tasks within a project share the project trust boundary. Separate worktrees prevent accidental editing conflicts; they are not an OS security boundary between mutually hostile tasks. Do not describe namespaces or ordinary shared-kernel containers as equivalent to separate virtual machines.

Do not silently fall back to executing on the host when a sandbox fails. Required tools or dependencies belong in declared project images or approved setup steps.

## 8. KEDA integration and shutdown behavior

Use a KEDA ScaledObject targeting each project StatefulSet, with zero-to-one scaling.

Expose an authenticated internal scaling metric through the always-running control service. Use KEDA's metrics-api scaler so execution workers do not need database credentials and a second monitoring service is not required just to scale workers.

The metric must be based on admitted work plus actual in-flight work that still requires the worker. Do not activate every project merely because many unadmitted jobs are queued beyond global capacity.

Persist an admitted job before exposing demand to KEDA. Keep a worker active during execution, gates, checkpoint publication, and unresolved shutdown work. A popped queue entry must not make a running job invisible to scaling.

An API job that is durably suspended for approval can release its execution slot and allow scale-to-zero. Reapproval makes it eligible for admission again. A native process still waiting for input cannot be treated as durably suspended without a supported resume mechanism.

Handle a new submission racing with scale-down. A terminating worker stops claiming new jobs; accepted work remains durably queued and is picked up by a valid worker.

Distinguish metric-query errors from a genuine zero. Configure and test outage behavior so metric failures do not cause active jobs to be treated as idle or wake an unbounded number of workers.

On termination, stop claiming work, quiesce execution at a safe boundary when possible, persist outcomes, and release ownership only after execution stops. If the grace period expires, preserve the uncertain boundary for recovery. Do not rely on graceful shutdown as the only protection.

Retain PVCs explicitly. Avoid automatic rollouts that disrupt active work without draining. Backward-incompatible worker/session upgrades must not silently resume incompatible checkpoints.

Deliver a Helm chart, example values, and a reproducible single-node Linux/k3s development setup. Pin and test compatible Kubernetes, KEDA, image, database, and CLI versions. Avoid floating latest image tags.

Single-node local storage provides pod-restart continuity on that node; it does not provide multi-node storage failover. Document that limit and the supported off-server restore path.

## 9. Public interfaces and operator controls

Provide separate server and worker entry points from the server module. Add thin remote-client commands to the existing CLI using its standard-library HTTP support.

Required operator capabilities:

- Register/list tenants and projects with allowed repository references and backend profiles.
- Submit a task and obtain a durable job ID.
- List/show jobs and attempts.
- Watch events with reconnect and a durable cursor.
- Cancel a job.
- Continue a supported session or resolve an explicit recovery request.
- Inspect and decide pending approvals.
- Inspect, evaluate, disable, and roll back project memory.

Use a versioned HTTP API. At minimum provide equivalents of:

    POST /v1/projects/{project_id}/jobs
    GET  /v1/jobs/{job_id}
    GET  /v1/jobs/{job_id}/events
    POST /v1/jobs/{job_id}/cancel
    POST /v1/jobs/{job_id}/resume
    POST /v1/jobs/{job_id}/approvals/{approval_id}
    GET  /internal/scaling/projects/{project_id}

Include authenticated actor checks, scoped idempotency, consistent error responses, bounded request sizes, pagination where necessary, and SSE reconnection using durable event IDs. Reject cursor mismatches and cross-tenant access.

Resolve submitted work through registered projects and validated repository/base references. Remote callers must not pass arbitrary host paths, container mounts, credentials, images, or Kubernetes specifications.

Preserve existing synchronous MCP tools and add asynchronous submit/status/events/cancel operations that use the same server contracts and authorization.

Support durable remote approval for the API backend. For native CLIs, implement only verified noninteractive permission modes and reject unsupported interactive approval flows before execution. Never silently weaken a requested permission mode.

Default the server to private access. Provide authenticated TLS configuration or a documented loopback/SSH-tunnel setup. A public website, billing UI, signup flow, and commercial SaaS launch are outside this implementation.

## 10. Automatic memory, with evaluation before promotion

First deliver reliable runs, context persistence, and inspectable evidence. Then implement automatic memory consolidation and promotion; do not mistake stored transcripts or vector search for demonstrated learning.

Use project-scoped memory in PostgreSQL initially. Apply backend/model applicability and retrieve relevant approved entries with deterministic filtering and text search. Do not add embeddings, pgvector, or a separate vector database in the first implementation. Keep an interface that permits a measured pgvector extension later.

A daily consolidation job may propose candidates only when new eligible completed-run evidence exists. Record source runs, verified outcomes, repository revision, backend/model, extraction version, and evaluation results.

Treat all source text as untrusted. Filter secrets, private credential paths, unsupported claims, and instructions attempting to change permissions, spending rules, tenant scope, or system behavior. Failed runs may supply failure evidence, but must not become successful recommendations without verification.

Use the existing evaluation runner and comparison machinery:

- Compare the active memory against the candidate on the same held-out project tasks, revision, backend, model, settings, and deterministic gates.
- Keep evaluation tasks separate from the examples used to construct the candidate.
- Require at least 10 eligible tasks and 3 repeats per variant.
- Reject candidates introducing new deterministic correctness or isolation failures.
- Require higher aggregate success, or equal success with at least 10% lower median model-token usage.
- Apply the token-saving route only when comparable usage is actually available.
- Store all runs and comparisons; these thresholds are an initial promotion policy, not a statistical guarantee.
- If evidence, provider access, or evaluation budget is insufficient, leave the candidate pending.

Promote versions atomically, per project/backend/model. Pin the selected memory version for a running or resumed session. Limit injected approved memory to 8 KiB and keep it distinct from authoritative instructions.

Provide inspect, disable, evaluate, and rollback controls. Retain the previous version and automatically revert when a repeated controlled evaluation reproduces a deterministic regression attributable to the new version; do not blame every unrelated production failure on memory.

Do not change model weights or rewrite HRN.md, AGENTS.md, or CLAUDE.md as a learning mechanism. Do not share memories across projects or tenants by default.

## 11. Cost and budget controls

Use explicitly configured subscription-backed CLI profiles first where available. Do not silently switch to a paid API or a different billing account when a subscription is rate-limited or unavailable.

Require explicit positive job and tenant spending limits before enabling metered API execution or metered memory evaluation. Reserve estimated exposure before requests and reconcile known usage afterward. Preserve unresolved reservations after interruptions.

Do not claim a precise invoice-level hard cap when the provider cannot guarantee one. Validate the available pricing/usage basis, enforce the conservative controls the harness can actually support, and expose unknown or unenforceable fields honestly.

Native CLI subscription usage is not equivalent to zero cost. Report configured subscription/server costs separately from variable API spending. Do not invent per-task dollar costs when the provider exposes no usable billing data.

Enforce timeouts, concurrency limits, retry limits, queue admission, and available provider usage limits. Recovery and evaluation must consume the same relevant limits rather than bypassing them.

Bound live tests to a disposable repository and configured accounts. Do not make paid API calls without an operator-supplied budget. Missing credentials or budget block that live test, not unrelated implementation work.

## 12. Operations, monitoring, and backup

Emit structured, redacted logs and durable audit events with tenant/project/job/attempt identifiers. Provide health/readiness checks and useful counters/timings without making an external observability platform mandatory.

Measure queue delay, admitted-to-start delay, cold start, first streamed response, completion time, gate duration, checkpoint latency, lease failures, recovery events, CPU, peak memory, disk use, provider usage, and known spending.

Alert or expose actionable status for stalled jobs, repeated worker starts, database/scaler failures, expired leases, recovery requests, storage pressure, failed backups, and budget exhaustion.

Provide encrypted off-server backup and restore procedures for PostgreSQL and persistent project state, with a manifest relating the restored data to durable checkpoints. Validate restored native sessions and worktrees; a database-only restore is insufficient.

Never delete durable project volumes during idle scaling. Retention/pruning must protect active jobs, resumable sessions, and required memory provenance. Do not introduce automatic destructive pruning without an explicit retention policy.

Document the difference between process recovery, pod recovery, node reboot, complete disk/server loss, and restoration from backup. Do not claim high availability for a single-node deployment.

## 13. Implementation order and evidence checkpoints

Maintain a checked implementation plan and a concise decision/evidence log inside the repository. Use the existing PLAN.md convention without erasing its history. Record actual completed work, commands/results, known limits, and unfinished validation.

Implement in this order:

1. Current-state audit and baseline checks; define additive execution/persistence contracts and the state machine.
2. Durable queue, checkpoints, and the smallest server/worker execution path. Prove client independence and crash recovery with the API backend and deterministic fixtures.
3. Project persistence and hardened execution boundary; prove workspace restoration, fencing, cancellation, and tenant/project authorization.
4. KEDA, StatefulSets, admission, draining, quotas, and the Helm/reference deployment. Prove zero-to-one-to-zero behavior.
5. Complete and verify durable integration for all four native CLI backends, including live session tests where access exists.
6. Memory candidates, evaluations, promotion/rollback, and complete budget accounting.
7. Performance comparisons, soak/failure testing, backup restoration, documentation, and release-readiness review.

Keep each increment independently reviewable. Integrate the components; do not count isolated scaffolds, mocks, empty handlers, TODOs, or interface definitions as completed capabilities.

If current code already provides a capability, validate and reuse it. If a verified upstream limitation prevents a required behavior, record it precisely and implement an honest error/recovery path instead of inventing support.

## 14. Required automated tests

Use meaningful behavioral and fault-injection tests. Reuse the existing helper-process pattern and fake SSE/MCP fixtures where appropriate.

Compatibility:

- Historical run/batch/eval records still load.
- Existing local CLI and MCP behavior remains intact.
- Shared options remain consistent across entry points.
- Root module still has no external requirements.
- API history remains append-only, including reasoning data.

Persistence and queue:

- Crash around acknowledgement, admission, claims, heartbeat renewal, response persistence, tool intent, tool completion, final result, and checkpoint publication.
- Duplicate and conflicting submissions.
- Concurrent claims and stale-owner writes.
- Database outage and recovery.
- Torn file journal tails, checksum failures, interrupted snapshot replacement, disk-full errors, and failed flushes.
- No accepted-job loss, no duplicate authoritative owner, and no silent uncertain-operation replay.

Execution:

- Timeouts and cancellation terminate subprocess trees.
- A killed worker cannot continue mutating a workspace after safe reassignment.
- Restart preserves untracked files, branches, worktree metadata, session IDs, memory versions, and cumulative budgets.
- Concurrent tasks use distinct worktrees and cannot overwrite each other's registered session identity.
- Local-only or unsupported modes fail explicitly in remote execution.

Isolation:

- Cross-tenant/project reads and writes through every API and worker endpoint.
- ID substitution in logs, artifacts, events, memory, sessions, approvals, and recovery requests.
- Path traversal, symlink escape, host mounts, metadata access, prohibited network destinations, and secret leakage.
- Resource exhaustion and quota enforcement.
- Execution cannot obtain trusted coordinator/database credentials.

Kubernetes/KEDA:

- Zero-to-one startup, one-to-zero shutdown, and retained volume reuse.
- Active work remains visible after being claimed.
- New submission during scale-down.
- Admission pressure does not wake unlimited projects.
- Metric errors are not interpreted as zero.
- Approval suspension/resumption, cancellations, rolling upgrades, SIGTERM, forced termination, and node restart.
- Node/network partition where the old process may still be alive; no duplicate writer is started on the assumption that an expired lease means termination.
- A recreated pod resumes the correct project and supported session.

Memory and budgets:

- Secret/instruction contamination, incorrect candidate lessons, and cross-project retrieval attempts.
- Held-out task separation, candidate rejection, insufficient sample/budget, atomic promotion, session pinning, disablement, and reproducible rollback.
- Identical limits before and after retries/restarts.
- Unknown usage remains visible/reserved.
- Subscription exhaustion does not activate paid fallback.

## 15. Validation commands and live gates

Run appropriate checks as each increment changes behavior. At the final integration gate, run at least the existing equivalents of:

    gofmt -l .
    go vet ./...
    go build ./...
    go test -count=3 ./...

Run the server module's formatting, vet, build, unit, and PostgreSQL integration checks explicitly from its module directory.

Keep Linux and Windows CI for the root CLI/core. Run race detection on supported Linux CI and retain the existing SSE/MCP fuzz checks, shell-script checks, and release build dry run. Do not weaken tests to hide failures.

Validate Helm rendering and Kubernetes schemas, then execute integration tests against a disposable real cluster with PostgreSQL and KEDA. YAML inspection and fake clients alone do not prove scaling or storage behavior.

Perform bounded live smoke/continuation tests for api, claude, codex, gemini, and opencode against disposable repositories and pinned CLI versions. Report each backend separately as passed, failed, or blocked. Mock tests are not live-provider validation.

Run an identical workload comparison for one versus two project task slots and immediate idle shutdown versus the 300-second cooldown. Record hardware, versions, sample counts, correctness, throughput, p50/p95 latency, peak memory, CPU time, storage activity, and known model cost per successful task.

Keep the initial global concurrency limit of two unless measured evidence supports a change within the available resource and provider budgets. Report cold-start costs and infrastructure overhead. Do not claim universal cost efficiency from a small benchmark.

Complete a 24-hour mixed-workload soak in a disposable environment and a restore into a clean environment. Include client disconnection, control-service restart, worker termination, and node restart. Long tests must have durable progress and bounded resource/spending limits.

If a required environment, credential, paid budget, or long-running test is unavailable, complete all accessible implementation and validation, leave reproducible commands and fixtures, and explicitly mark that gate pending. Do not label pending tests as passed or the entire platform production-ready.

## 16. Deliverables and final report

Deliver working, integrated code; migrations; container definitions; Helm configuration; deterministic/integration tests; bounded live-test scripts; benchmark tooling; and operator documentation.

Include a local-development guide, single-server installation guide, backend capability matrix, configuration reference, security boundaries, recovery/approval procedure, backup/restore procedure, and upgrade/rollback notes.

Before finishing, inspect the complete diff, check for accidental secrets and unrelated changes, and run checks appropriate to the final changes.

The final report must state:

1. What was implemented, grouped by the milestones above.
2. What is compatible with the existing local CLI and records.
3. Exact validation performed and its results.
4. Live backend, Kubernetes, soak, and restoration results separately.
5. Measured performance/cost findings, including hardware and caveats.
6. Remaining limitations, missing credentials/environment, and unexecuted gates.
7. The exact next command(s) to exercise the implementation locally.

Do not claim automatic self-improvement, crash-safe external effects, strict tenant isolation, production readiness, or lower costs unless the relevant evidence supports the specific claim.

## 17. Primary references to verify during implementation

Use current primary documentation and pin what is actually tested:

- KEDA deployment/StatefulSet scaling: https://keda.sh/docs/latest/concepts/scaling-deployments/
- KEDA ScaledObject configuration: https://keda.sh/docs/latest/reference/scaledobject-spec/
- KEDA Metrics API scaler: https://keda.sh/docs/latest/scalers/metrics-api/
- Kubernetes StatefulSets and volume retention: https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/
- Kubernetes multi-tenancy: https://kubernetes.io/docs/concepts/security/multi-tenancy/
- Kubernetes pod security: https://kubernetes.io/docs/concepts/security/pod-security-standards/
- PostgreSQL transactions: https://www.postgresql.org/docs/current/tutorial-transactions.html
- PostgreSQL SELECT and SKIP LOCKED: https://www.postgresql.org/docs/current/sql-select.html
- pgvector, for a later evaluated retrieval extension: https://github.com/pgvector/pgvector
- Claude Code headless usage: https://code.claude.com/docs/en/headless
- Codex noninteractive usage: https://developers.openai.com/codex/noninteractive
- Gemini session management: https://geminicli.com/docs/cli/session-management/
- Gemini headless usage: https://geminicli.com/docs/cli/headless/
- OpenCode CLI: https://opencode.ai/docs/cli/

Begin with repository inspection and baseline checks, then implement the first durable execution milestone. Continue through the remaining increments within the authorization and evidence requirements above.
