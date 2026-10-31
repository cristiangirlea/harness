CREATE TABLE knowledge_admissions (
 tenant_id text NOT NULL, project_id text NOT NULL, version_id text NOT NULL,
 verdict text NOT NULL CHECK (verdict IN ('admitted','rejected','pending')),
 baseline_version_id text NOT NULL, eval_ids text[] NOT NULL,
 pass_rate_baseline double precision NOT NULL, pass_rate_candidate double precision NOT NULL,
 median_tokens_baseline double precision NOT NULL, median_tokens_candidate double precision NOT NULL,
 failed_attempt_tokens_baseline double precision NOT NULL, failed_attempt_tokens_candidate double precision NOT NULL,
 source_runs text[] NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,project_id,version_id),
 FOREIGN KEY (tenant_id,project_id,version_id) REFERENCES knowledge_versions(tenant_id,project_id,id),
 FOREIGN KEY (tenant_id,project_id,baseline_version_id) REFERENCES knowledge_versions(tenant_id,project_id,id)
);
