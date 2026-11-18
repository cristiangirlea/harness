# PostgreSQL control service and project worker

`server/` is a separate Go module. The root `hrn` CLI remains standalone and
standard library only. This module uses pgx to keep tenants, projects, jobs,
ownership, events, and durable API checkpoints in PostgreSQL. The control
service accepts jobs over HTTP; a project worker claims and heartbeats them,
runs jobs through the hrn binary, and saves checkpoints. An accepted job
survives the submitting client's disconnect and a control service restart. Recovery after
worker loss requires operator confirmation that the old executor stopped.

The worker is a development fixture: it accepts only read-only API jobs with
a positive cost ceiling, uses a loopback fake model, and has no tools, shell,
or external agent CLI execution. It does not fetch the registered repository
or provide an isolation boundary. Its workspace manifests detect identity drift; they do
not make filesystem changes and database checkpoints atomic.

Both submission and the durable API run path refuse MCP servers and
`allow_dangerous`. A stored job that bypassed submission with either option
finishes as `failed`, with the refusal in its result, before any model call.

## Local checks

Run from `server/`:

```text
go vet ./...
go build ./...
go test -count=1 ./...
```

PostgreSQL tests skip unless `HRN_TEST_DATABASE_URL` is set. For a disposable
fixture, start PostgreSQL using `compose.dev.yaml` from the repository root:

```text
docker compose -f server/compose.dev.yaml up -d
```

Then set `HRN_TEST_DATABASE_URL` to
`postgres://postgres:fixture-only@127.0.0.1:54329/harness_test?sslmode=disable`
and run `go test -count=1 ./...` in `server/`. Tests create and remove their own
schemas. Stop the fixture with `docker compose -f server/compose.dev.yaml down`.

`TestKnowledgeLabelledQueries` reports hit rate (any expected entry among the packet's at most five entries),
MRR and the share of no-answer queries that got context per ranking version, and fails when a query that hit
before stops hitting or the current ranking's recorded hits or metrics drift. `HRN_UPDATE_LABELLED=1` records
the current ranking's numbers; a deliberate loss goes in `accepted_losses`, read from the newest element.

Regenerate the knowledge goldens with `HRN_UPDATE_WIRE_FIXTURES=1`, and the
client recording with `HRN_UPDATE_FIXTURES=1` and `HRN_TEST_DATABASE_URL` set.
Copy `server/platform/testdata/knowledge` to `internal/retrieval/testdata/wire`
and `server/platform/testdata/remote` to `internal/remote/testdata/server`; the
fixture identity test fails until the copies match.

For a private local control service, set `HRN_DATABASE_URL` and distinct random
`HRN_OPERATOR_TOKEN` and `HRN_SCALER_TOKEN` values of at least 32 characters.
`hrn-server` also accepts `HRN_DATABASE_URL_FILE`, `HRN_OPERATOR_TOKEN_FILE`,
and `HRN_SCALER_TOKEN_FILE`; `hrn-worker` accepts `HRN_WORKER_TOKEN_FILE`.
Each `_FILE` variable names a regular file of at most 64 KiB instead of a
non-empty direct value; exactly one trailing LF or CRLF is removed, and an
empty result is rejected.
Run `go run ./cmd/hrn-server migrate`, then `go run ./cmd/hrn-server serve`
from `server/`. The default `HRN_LISTEN` address is `127.0.0.1:8088`.
Without TLS, only loopback IP literals (`127.0.0.0/8` or `::1`, for example
`[::1]:8088`) are allowed. Host names (including `localhost`), empty hosts
such as `:8088`, and non-loopback addresses require both `HRN_TLS_CERT_FILE`
and `HRN_TLS_KEY_FILE`; otherwise startup is refused before listening. Run
`go run ./cmd/hrn-fixture-model` for the local model and configure the worker
with `HRN_CONTROL_URL`, `HRN_FIXTURE_API_URL`, `HRN_PROJECT_ID`,
`HRN_WORKER_TOKEN`, and an existing absolute `HRN_WORKSPACE_ROOT`. The worker
command is `go run ./cmd/hrn-worker`.
The worker requires the `hrn` binary to run jobs and checks `hrn version` at
startup, with a 10-second timeout. Set
`HRN_WORKER_HRN` or pass `-hrn` to select the binary; the flag takes precedence.
The default is `hrn` on PATH. The container image supplies the binary and sets
`HRN_WORKER_HRN` to its absolute path.

A tenant budget caps total committed spending. The committed total sums every
job the tenant has ever submitted. It has no reset period. The operator can
raise it as needed. Each new job commits its `max_cost_usd`. A terminal job
with known spend counts at that amount, and a
terminal job that never started an attempt counts at zero. All other jobs
keep their reservation. Jobs that finished before the upgrade are counted by
the same rule using their stored results. Worker-reported spend is an estimate
for budget accounting, clamped between zero and the job's reservation.
Invoices are separate.

Use `PUT /v1/tenants/{tenant}/budget` with the operator token and a
`budget_usd` number, or use JSON `null` to remove the cap. Lowering the
budget can block new jobs and leaves accepted jobs in place. The operator
and the tenant can use `GET /v1/tenants/{tenant}/budget` to see the budget
and its committed, reserved, and spent amounts. A new submission that would
exceed the budget returns 422 `tenant_budget`. Set
`HRN_REQUIRE_TENANT_BUDGET=1` to also refuse new submissions from tenants
without a budget. Empty or unset disables this requirement. Other values
stop startup.

`HRN_MAX_COST_USD_PER_JOB` still caps each job's ceiling. Set it to a
positive number of USD. A submission above it returns 422 `cost_limit`, and
an invalid value stops startup. Unset means no server per-job cap. Clients can
add their own with `HRN_REMOTE_MAX_COST_USD`.

## Signing in with OpenID Connect

Leave all five variables unset to keep static-token authentication only.
Set `HRN_OIDC_ISSUER` to the issuer URL and `HRN_OIDC_AUDIENCE` to the
expected audience. Configure at least one role mapping:

- Set both `HRN_OIDC_OPERATOR_CLAIM` and `HRN_OIDC_OPERATOR_VALUE` to grant
  operator access when the claim matches that value or contains it in an
  array of strings.
- Set `HRN_OIDC_TENANT_CLAIM` to grant access to the exact tenant IDs in
  that claim. It can contain one string or an array. Empty strings and
  non-string entries are ignored, and at most 256 strings are used.

Claim names must be 1 to 128 characters and name top-level claims. Empty
settings and incomplete configurations stop startup. The issuer URL must
use HTTPS, or HTTP with a loopback IP literal (127.0.0.0/8 or ::1), and
cannot include a query, fragment, or user information. The service compares
`iss` and the discovery document's issuer exactly with the configured
string, including any trailing slash.

Send a signed token as an HTTP bearer credential. Tokens are at most 8192
bytes and contain three non-empty, unpadded base64url segments. A token
must identify a key with a 1-to-256-character `kid`. The service fetches
keys through the issuer's discovery document and ignores keys intended for
other uses or algorithms. The JWKS URL follows the same transport and
user-information restrictions as the issuer. It can include a query.
Discovery and key-set requests refuse redirects, have a 10-second timeout
each, and limit responses to 1 MiB. At most 100 keys are considered. Keys
are cached and a missing key can trigger a refresh at most once a minute.
Failed loads can be retried after one minute.

The service verifies RS256 signatures with RSA keys of at least 2048 bits
and ES256 signatures with P-256 keys and a 64-byte signature. Tokens with
`crit` or header fields that specify keys or certificates are refused.
The audience must match a string in `aud`, which can be a string or an
array of strings. A token must have a numeric expiry and a non-empty `sub`
of at most 256 characters, excluding ASCII control characters. The service
allows 60 seconds of clock skew for expiry and for the optional numeric
`nbf` and `iat` claims.
The service imposes no separate token type check beyond issuer, audience,
and the claims above, so use an audience issued exclusively for this service.

Every operator route, including the knowledge routes, accepts an operator's
signed token. Existing static tokens remain valid. Worker and scaler routes
accept only their static tokens. A job event produced by a signed-in person's
submission, cancellation, or recovery resolution includes the token's
subject as `actor` and its issuer as `actor_issuer`. Worker and timer
events have no actor. Revoke a person's access at the identity provider.
Previously issued tokens remain usable through expiry and the clock-skew
allowance, so keep token lifetimes short.

## Job events

`GET /v1/jobs/{job}/events?tenant_id=...&project_id=...` requires the tenant's
bearer token. By default it returns a JSON page of up to 100 events. Set
`Accept: text/event-stream` to receive the backlog and then live events, polled
once per second. Resume after the last received event ID using `after` or
`Last-Event-ID`; if both are supplied they must match. A job's event IDs
become visible in increasing order, because each event is written while its job's
row is locked, so a cursor never skips an event that commits later.

Each control service instance allows at most four open SSE streams per tenant,
shared across its projects and jobs. Further streams receive HTTP 429 with a
JSON `stream_limit` error. A slot is released whenever its stream ends, including
client disconnects. JSON event requests do not consume stream slots.

A stream drains the backlog and closes when the job is `succeeded`, `failed`,
or `canceled`, including when it receives that terminal event live. An already
terminal job with no events after the cursor closes immediately. States such as
`waiting_approval` and `recovery_required` remain live. Streams also close after
30 minutes; reconnect with the last received cursor to continue without replaying
earlier events.

## Schema and recovery

Migration 001 creates the control schema. Migration 002 snapshots the project
repository URL when a job is accepted. It refuses to upgrade a schema 1
database that contains jobs, because their accepted URLs cannot be recovered
safely. Migration 003 adds scoped, immutable project knowledge and retrieval
records. The integration tests cover both refusal and the empty schema upgrade.
Migration 007 adds tenant budgets and backfills existing job reservations from
their stored `max_cost_usd`.
Opening an older schema requests the explicit `migrate` command; startup never
migrates it automatically.
The worker uses version 2 workspace manifests and refuses older or mismatched
manifests. An uncertain operation enters `recovery_required` instead of being
silently replayed. The control service does not independently verify that an
old worker or node has stopped.

The checkpoint store is a trust boundary: it must commit before acknowledging a
save, fence stale owners, and preserve immutable versions. The API checkpoint's
task digest binds task settings, not the stored `Result.CostUSD`, `Started`, or
`Deadline`. Recovery trusts those values from the store; changing them can reset
spent budget or the clock. Keep checkpoint write access and store credentials
outside execution tools and untrusted workspace/model content.

The cost ceiling prevents the next model request. Tools from an already paid,
checkpointed response still run on recovery even if that response reached the
ceiling; their committed outcomes are not repeated. The original deadline still
applies. Cancellation or timeout between committed operations reports `canceled`
or `timeout` without `recovery_required`. A definite non-retryable HTTP 4xx is
an error without recovery; a context-management 400 retries without that feature
after saving a new model intent. Interrupted requests, retryable HTTP errors,
and failed checkpoint saves remain uncertain and require recovery.

## Approved project knowledge

Only the control service connects to PostgreSQL. Operators import reviewed
source material with `HRN_OPERATOR_TOKEN`; model-generated candidates are
rejected. Read clients use `HRN_SERVER_URL`, `HRN_TENANT_TOKEN` (or the exact
project's worker token), and optionally `HRN_SERVER_CA_FILE`. Knowledge
commands use `HRN_OPERATOR_TOKEN` for mutations and never fall back to the
read credential. Credentials stay outside request files and packets.
Operator review is the control: the import deny list screens every text field
but catches obvious mistakes only, including private credential paths, key
headers, common token shapes, and a few instruction-injection phrases.
The `.env` path check matches whole segments (split on `/`, whitespace, quotes,
`=`, `,` and `;`) and `.env.<suffix>` files; `.envrc` and `docs/.environment.md`
are allowed. Path checks apply to paths, keys and identifiers, not to the prose
of `content`, `evidence` and `review_evidence`, so a note such as "copy
.env.example to .env" imports; key headers and token shapes are refused in every field.
Import refusals name the field and deny-list pattern that matched, without
including the submitted text.

The API uses these scoped JSON operations:

| Route | Operation | Credential |
|---|---|---|
| `POST /v1/knowledge/import` | Import a reviewed, immutable version | Operator |
| `POST /v1/knowledge/change` | Activate, disable, or roll back a version | Operator |
| `POST /v1/knowledge/admission` | Record a verdict; activate an admitted candidate while its baseline stays active | Operator |
| `POST /v1/knowledge/search` | Return an audited, bounded packet | Tenant, project worker, or operator |
| `POST /v1/knowledge/inspect` | Inspect a version | Tenant, project worker, or operator |
| `POST /v1/knowledge/active` | Read active and previous pointers | Tenant, project worker, or operator |

A reviewed import request in `seed.json` has this shape:

```json
{
  "tenant_id": "t_<32 hex>", "project_id": "p_<32 hex>",
  "backend": "codex", "model": "<configured model>",
  "revision": "<full reviewed commit SHA>",
  "origin": "reviewed_source", "reviewed_by": "<reviewer>",
  "review_evidence": "<source review reference>",
  "entries": [{
    "key": "build-guide", "content": "<reviewed project guidance>",
    "source": "docs/build.md", "evidence": "<verification reference>",
    "identifiers": ["BuildProject", "src/build.go"]
  }]
}
```

```text
hrn knowledge import seed.json
hrn knowledge active scope.json
hrn knowledge activate change.json
hrn knowledge search search.json
hrn knowledge inspect inspect.json
hrn knowledge disable change.json
hrn knowledge rollback change.json
```

`scope.json` contains tenant_id/project_id/backend/model/revision. `change.json`
adds version_id for activate and expected_generation from `active`; disable and
rollback need scope and expected_generation. `search.json` adds query and
optionally version_id; `inspect.json` adds version_id. Inputs reject unknown
fields, including credential fields. Search matches exact scope and ranks
identifiers, paths, and errors before `simple` text matches, with stable ties.
Ranking `simple-rare-floor-v4` weights keys above content and, for versions of
at least 20 entries, ignores words in more than 25% of entries, weights rarer
words more and returns `empty_reason=below_threshold` below a normalised score
of 1.31 (about one word unique to the key or two fairly rare words) unless an
identifier-shaped exact identifier matches; smaller versions rank as
before. `TestKnowledgeLabelledQueries` set the share and the minimum score.
Search materializes the version's members before matching their entries.
Identifiers may include spaces for multi-word errors. Exact query candidates
also omit trailing sentence periods and colons, preserving internal path punctuation.
Joined candidates contain at most 8 words; each search sends at most 256 exact
candidates, each limited to 256 bytes.
The optional search `attempt` must be between 0 and 2147483647 inclusive.
An empty or no-match packet is valid; packets contain at most 8192 bytes of
reference text and an audit ID.

## Later slices

- #138 adds `hrn remote` and its MCP tools.
- #139's development charts and container image are documented in [the deploy guide](deploy/README.md).
- #141 wires retrieval into execution and adds fixture comparison.
