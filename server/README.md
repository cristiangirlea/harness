# PostgreSQL control service and project worker

`server/` is a separate Go module. The root `hrn` CLI remains standalone and
standard library only. This module uses pgx to keep tenants, projects, jobs,
ownership, events, and durable API checkpoints in PostgreSQL. The control
service accepts jobs over HTTP; a project worker claims and heartbeats them,
runs the API backend, and saves checkpoints. An accepted job survives the
submitting client's disconnect and a control service restart. Recovery after
worker loss requires operator confirmation that the old executor stopped.

The worker is a development fixture: it accepts only read-only API jobs with
a positive cost ceiling, uses a loopback fake model, and has no tools, shell,
or native CLI execution. It does not fetch the registered repository or provide
an isolation boundary. Its workspace manifests detect identity drift; they do
not make filesystem changes and database checkpoints atomic.

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

For a private local control service, set `HRN_DATABASE_URL` and distinct random
`HRN_OPERATOR_TOKEN` and `HRN_SCALER_TOKEN` values of at least 32 characters.
Run `go run ./cmd/hrn-server migrate`, then `go run ./cmd/hrn-server serve`
from `server/`. The default address is `127.0.0.1:8088`. Run
`go run ./cmd/hrn-fixture-model` for the local model and configure the worker
with `HRN_CONTROL_URL`, `HRN_FIXTURE_API_URL`, `HRN_PROJECT_ID`,
`HRN_WORKER_TOKEN`, and an existing absolute `HRN_WORKSPACE_ROOT`. The worker
command is `go run ./cmd/hrn-worker`.

## Schema and recovery

Migration 001 creates the control schema. Migration 002 snapshots the project
repository URL when a job is accepted. It refuses to upgrade a schema 1
database that contains jobs, because their accepted URLs cannot be recovered
safely. The integration tests cover both refusal and the empty schema upgrade.
The worker uses version 2 workspace manifests and refuses older or mismatched
manifests. An uncertain operation enters `recovery_required` instead of being
silently replayed. The control service does not independently verify that an
old worker or node has stopped.

## Later slices

- #138 adds `hrn remote` and its MCP tools.
- #139 adds the development charts and container image.
- #140 adds approved project knowledge to the control service.
