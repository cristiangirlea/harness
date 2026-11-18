ALTER TABLE tenants ADD COLUMN budget_usd numeric CHECK (budget_usd > 0);
ALTER TABLE jobs ADD COLUMN reserved_usd numeric NOT NULL DEFAULT 0 CHECK (reserved_usd >= 0);
ALTER TABLE jobs ADD COLUMN spent_usd numeric CHECK (spent_usd >= 0);

UPDATE jobs
SET reserved_usd = (options->>'max_cost_usd')::numeric
WHERE CASE WHEN jsonb_typeof(options->'max_cost_usd') = 'number'
           THEN (options->>'max_cost_usd')::numeric > 0 END;

UPDATE jobs
SET spent_usd = 0
WHERE state IN ('succeeded','failed','canceled')
  AND NOT EXISTS (SELECT 1 FROM attempts WHERE job_id = jobs.id);

UPDATE jobs
SET spent_usd = LEAST(GREATEST((result->>'cost_usd')::numeric, 0), reserved_usd)
WHERE state IN ('succeeded','failed','canceled')
  AND EXISTS (SELECT 1 FROM attempts WHERE job_id = jobs.id)
  AND CASE WHEN jsonb_typeof(result->'cost_usd') = 'number'
           THEN (result->>'cost_usd')::numeric <> 0 OR result->'cost_is_estimate' = 'true'::jsonb END;

CREATE INDEX jobs_tenant_exposure ON jobs (tenant_id) INCLUDE (state, reserved_usd, spent_usd);
