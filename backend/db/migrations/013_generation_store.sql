-- Formal generation store. artifact_blobs is this deployment's private object
-- store: the unique storage_key gives conditional-create semantics (a second PUT
-- of the same key fails instead of overwriting), and rows are append-only so a
-- late upload can never replace a purchased file. Bytes leave only through the
-- download endpoint after COMPLETED order + delivery receipt checks.
CREATE TABLE artifact_blobs (
 storage_key text PRIMARY KEY,
 data bytea NOT NULL CHECK(octet_length(data) BETWEEN 1 AND 20971520),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION reject_artifact_blob_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'artifact blob content is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER artifact_blob_immutable BEFORE UPDATE OR DELETE ON artifact_blobs
 FOR EACH ROW EXECUTE FUNCTION reject_artifact_blob_rewrite();

-- Formal lanes exist once per deployment; capacity planning serializes on the
-- lane row. Fixed identifiers keep repeated migrations idempotent by intent.
INSERT INTO scheduler_lanes(id, queue_name, enabled) VALUES
 ('00000000-0000-4000-8000-0000000000f1', 'formal', true),
 ('00000000-0000-4000-8000-0000000000f2', 'preview', true)
ON CONFLICT (id) DO NOTHING;

-- Formal settlement audit: the worker's terminal record for one order attempt.
-- Without it, "why did my point come back" is only reconstructable from jobs.
CREATE TABLE generation_events (
 id uuid PRIMARY KEY, order_id uuid NOT NULL REFERENCES generation_orders(id),
 kind text NOT NULL CHECK(kind IN ('RESERVE','CONSUME','RELEASE','RETRY_RESERVE')),
 detail jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(detail)='object'),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX generation_events_order ON generation_events(order_id, created_at);
