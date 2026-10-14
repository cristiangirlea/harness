CREATE TABLE knowledge_entries (
 tenant_id text NOT NULL, project_id text NOT NULL, id text NOT NULL,
 entry_key text NOT NULL, content text NOT NULL, source text NOT NULL,
 evidence text NOT NULL, identifiers text[] NOT NULL,
 search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
 PRIMARY KEY(tenant_id,project_id,id),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
CREATE INDEX knowledge_text_gin ON knowledge_entries USING gin(search_vector);
CREATE INDEX knowledge_identifiers_gin ON knowledge_entries USING gin(identifiers);
CREATE TABLE knowledge_versions (
 tenant_id text NOT NULL, project_id text NOT NULL, id text NOT NULL,
 backend text NOT NULL, model text NOT NULL, revision text NOT NULL,
 reviewed_by text NOT NULL, review_evidence text NOT NULL,
 created_tx bigint NOT NULL DEFAULT txid_current(), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,project_id,id),
 UNIQUE(tenant_id,project_id,backend,model,revision,id),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
CREATE TABLE knowledge_members (
 tenant_id text NOT NULL, project_id text NOT NULL, version_id text NOT NULL,
 entry_id text NOT NULL, entry_key text NOT NULL,
 PRIMARY KEY(tenant_id,project_id,version_id,entry_id),
 UNIQUE(tenant_id,project_id,version_id,entry_key),
 FOREIGN KEY(tenant_id,project_id,version_id) REFERENCES knowledge_versions(tenant_id,project_id,id),
 FOREIGN KEY(tenant_id,project_id,entry_id) REFERENCES knowledge_entries(tenant_id,project_id,id)
);
CREATE FUNCTION knowledge_membership_creation_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM knowledge_versions WHERE tenant_id=NEW.tenant_id AND project_id=NEW.project_id AND id=NEW.version_id AND created_tx=txid_current()) THEN
  RAISE EXCEPTION 'version membership is sealed';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER sealed_membership BEFORE INSERT ON knowledge_members FOR EACH ROW EXECUTE FUNCTION knowledge_membership_creation_only();
CREATE TRIGGER immutable_knowledge_entries BEFORE UPDATE OR DELETE ON knowledge_entries FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
CREATE TRIGGER immutable_knowledge_versions BEFORE UPDATE OR DELETE ON knowledge_versions FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
CREATE TRIGGER immutable_knowledge_members BEFORE UPDATE OR DELETE ON knowledge_members FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
CREATE TABLE knowledge_active_versions (
 tenant_id text NOT NULL, project_id text NOT NULL, backend text NOT NULL, model text NOT NULL, revision text NOT NULL,
 version_id text, previous_id text, generation bigint NOT NULL DEFAULT 0,
 PRIMARY KEY(tenant_id,project_id,backend,model,revision),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id),
 FOREIGN KEY(tenant_id,project_id,backend,model,revision,version_id) REFERENCES knowledge_versions(tenant_id,project_id,backend,model,revision,id),
 FOREIGN KEY(tenant_id,project_id,backend,model,revision,previous_id) REFERENCES knowledge_versions(tenant_id,project_id,backend,model,revision,id)
);
CREATE TABLE retrieval_records (
 id text PRIMARY KEY, tenant_id text NOT NULL, project_id text NOT NULL,
 version_id text, run_ref text NOT NULL, attempt integer NOT NULL,
 packet jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id),
 FOREIGN KEY(tenant_id,project_id,version_id) REFERENCES knowledge_versions(tenant_id,project_id,id)
);
CREATE TRIGGER immutable_retrieval_records BEFORE UPDATE OR DELETE ON retrieval_records FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
CREATE TABLE knowledge_changes (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, tenant_id text NOT NULL, project_id text NOT NULL,
 change jsonb NOT NULL, active jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
CREATE TRIGGER immutable_knowledge_changes BEFORE UPDATE OR DELETE ON knowledge_changes FOR EACH ROW EXECUTE FUNCTION reject_durable_record_mutation();
