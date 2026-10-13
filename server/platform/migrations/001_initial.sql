CREATE TABLE IF NOT EXISTS tenants (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    token_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id),
    name text NOT NULL,
    repository_url text NOT NULL,
    base_ref text NOT NULL,
    backend text NOT NULL CHECK (backend IN ('api','claude','codex','gemini','opencode')),
    worker_token_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, name)
);
CREATE TABLE IF NOT EXISTS jobs (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    project_id text NOT NULL,
    prompt text NOT NULL,
    options jsonb NOT NULL,
    backend text NOT NULL,
    base_ref text NOT NULL,
    request_hash bytea NOT NULL,
    idempotency_key text,
    state text NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued','admitted','running','waiting_approval',
                         'recovery_required','succeeded','failed','canceled')),
    available_at timestamptz NOT NULL DEFAULT now(),
    admission_attempts integer NOT NULL DEFAULT 0,
    admitted_at timestamptz,
    admission_deadline timestamptz,
    owner_generation bigint NOT NULL DEFAULT 0,
    owner_id text,
    lease_until timestamptz,
    checkpoint_seq bigint NOT NULL DEFAULT 0,
    result jsonb,
    cancel_requested boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects(tenant_id, id),
    CONSTRAINT active_owner CHECK (
        (state = 'running' AND owner_id IS NOT NULL AND lease_until IS NOT NULL)
        OR state <> 'running'
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_idempotent
    ON jobs (tenant_id, project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs (state, available_at, created_at);
CREATE INDEX IF NOT EXISTS jobs_project_state ON jobs (project_id, state);
CREATE TABLE IF NOT EXISTS attempts (
    job_id text NOT NULL REFERENCES jobs(id),
    generation bigint NOT NULL,
    worker_id text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    PRIMARY KEY (job_id, generation)
);
CREATE TABLE IF NOT EXISTS checkpoints (
    job_id text NOT NULL REFERENCES jobs(id),
    sequence bigint NOT NULL,
    generation bigint NOT NULL,
    boundary text NOT NULL,
    digest bytea NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, sequence)
);
CREATE TABLE IF NOT EXISTS job_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL,
    project_id text NOT NULL,
    job_id text NOT NULL REFERENCES jobs(id),
    kind text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_events_scope ON job_events (tenant_id, project_id, id);
CREATE OR REPLACE FUNCTION reject_durable_record_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'durable records are append-only';
END;
$$;
DROP TRIGGER IF EXISTS immutable_checkpoints ON checkpoints;
CREATE TRIGGER immutable_checkpoints BEFORE UPDATE OR DELETE ON checkpoints
    FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
DROP TRIGGER IF EXISTS immutable_job_events ON job_events;
CREATE TRIGGER immutable_job_events BEFORE UPDATE OR DELETE ON job_events
    FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
