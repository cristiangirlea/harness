-- Version 1 did not record the repository accepted with each job. Refuse to
-- infer historical job identity from a project row that may have changed.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jobs) THEN
        RAISE EXCEPTION 'migration 2 requires an empty jobs table; review and export existing jobs before upgrade';
    END IF;
END;
$$;

ALTER TABLE jobs ADD COLUMN repository_url text NOT NULL;
