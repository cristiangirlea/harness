DROP INDEX knowledge_text_gin;
ALTER TABLE knowledge_entries DROP COLUMN search_vector;
ALTER TABLE knowledge_entries ADD COLUMN search_vector tsvector GENERATED ALWAYS AS (setweight(to_tsvector('simple', entry_key), 'A') || setweight(to_tsvector('simple', content), 'D')) STORED;
CREATE INDEX knowledge_text_gin ON knowledge_entries USING gin(search_vector);
