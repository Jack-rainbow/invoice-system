CREATE TABLE artifact_objects (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), version_id uuid, preview_id uuid,
 type text NOT NULL CHECK(type IN ('CLEAN_PDF','CLEAN_PNG','PREVIEW')),
 reserve_attempt integer NOT NULL CHECK(reserve_attempt>0), attempt_no integer NOT NULL CHECK(attempt_no>0),
 storage_key text NOT NULL CHECK(storage_key<>''), object_version text, sha256 text, size bigint,
 state text NOT NULL CHECK(state IN ('UPLOADING','READY','PUBLISHED','RETIRED','GC_CLAIMED','DELETED')),
 protected_until timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(version_id,owner_id) REFERENCES document_versions(id,owner_id), FOREIGN KEY(preview_id,owner_id) REFERENCES preview_requests(id,owner_id),
 CHECK((type IN ('CLEAN_PDF','CLEAN_PNG') AND version_id IS NOT NULL AND preview_id IS NULL) OR (type='PREVIEW' AND preview_id IS NOT NULL AND version_id IS NULL)),
 CHECK(sha256 IS NULL OR sha256 ~ '^[a-f0-9]{64}$'), CHECK(size IS NULL OR size>=0),
 CHECK(state IN ('UPLOADING','GC_CLAIMED','DELETED') OR (nullif(object_version,'') IS NOT NULL AND sha256 IS NOT NULL AND size IS NOT NULL)),
 UNIQUE(id,version_id,type), UNIQUE(id,preview_id,type)
);
CREATE UNIQUE INDEX uq_object_key ON artifact_objects(storage_key);
CREATE INDEX artifact_objects_gc ON artifact_objects(state,protected_until);
CREATE TABLE artifacts (
 id uuid PRIMARY KEY, version_id uuid, preview_id uuid, type text NOT NULL CHECK(type IN ('CLEAN_PDF','CLEAN_PNG','PREVIEW')),
 current_object_id uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((type IN ('CLEAN_PDF','CLEAN_PNG') AND version_id IS NOT NULL AND preview_id IS NULL) OR (type='PREVIEW' AND preview_id IS NOT NULL AND version_id IS NULL)),
 FOREIGN KEY(current_object_id,version_id,type) REFERENCES artifact_objects(id,version_id,type),
 FOREIGN KEY(current_object_id,preview_id,type) REFERENCES artifact_objects(id,preview_id,type)
);
CREATE UNIQUE INDEX uq_final_format ON artifacts(version_id,type) WHERE type IN ('CLEAN_PDF','CLEAN_PNG');
CREATE UNIQUE INDEX uq_preview_format ON artifacts(preview_id,type) WHERE type='PREVIEW';
CREATE INDEX artifacts_current ON artifacts(current_object_id);
CREATE TABLE artifact_read_leases (
 id uuid PRIMARY KEY, object_id uuid NOT NULL REFERENCES artifact_objects(id), lease_until timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), CHECK(lease_until>created_at)
);
CREATE INDEX artifact_read_leases_object ON artifact_read_leases(object_id,lease_until);
CREATE FUNCTION protect_artifact_object() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'object metadata history cannot be deleted' USING ERRCODE='55000'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'UPLOADING' THEN RAISE EXCEPTION 'object starts UPLOADING' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF (NEW.id,NEW.owner_id,NEW.version_id,NEW.preview_id,NEW.type,NEW.reserve_attempt,NEW.attempt_no,NEW.storage_key,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.owner_id,OLD.version_id,OLD.preview_id,OLD.type,OLD.reserve_attempt,OLD.attempt_no,OLD.storage_key,OLD.created_at) THEN
  RAISE EXCEPTION 'object identity and storage key are immutable' USING ERRCODE='55000'; END IF;
 IF OLD.state<>'UPLOADING' AND (NEW.object_version,NEW.sha256,NEW.size) IS DISTINCT FROM (OLD.object_version,OLD.sha256,OLD.size) THEN
  RAISE EXCEPTION 'uploaded bytes metadata is immutable' USING ERRCODE='55000'; END IF;
 IF NEW.state<>OLD.state AND NOT (
  (OLD.state='UPLOADING' AND NEW.state IN ('READY','GC_CLAIMED')) OR
  (OLD.state='READY' AND NEW.state IN ('PUBLISHED','GC_CLAIMED')) OR
  (OLD.state='PUBLISHED' AND NEW.state='RETIRED') OR
  (OLD.state='RETIRED' AND NEW.state='GC_CLAIMED') OR
  (OLD.state='GC_CLAIMED' AND NEW.state='DELETED')) THEN
  RAISE EXCEPTION 'invalid artifact state transition' USING ERRCODE='23514'; END IF;
 IF NEW.state IN ('RETIRED','GC_CLAIMED','DELETED') AND EXISTS(SELECT 1 FROM artifacts WHERE current_object_id=NEW.id) THEN
  RAISE EXCEPTION 'current published reference prevents retirement/GC' USING ERRCODE='23514'; END IF;
 IF NEW.state='GC_CLAIMED' AND (
  NEW.protected_until>clock_timestamp() OR EXISTS(SELECT 1 FROM artifact_read_leases WHERE object_id=NEW.id AND lease_until>clock_timestamp()) OR
  EXISTS(SELECT 1 FROM render_jobs WHERE version_id=NEW.version_id AND reserve_attempt=NEW.reserve_attempt AND attempt_no=NEW.attempt_no AND state IN ('QUEUED','RUNNING','RETRY_WAIT')) OR
  EXISTS(SELECT 1 FROM preview_requests WHERE id=NEW.preview_id AND state IN ('QUEUED','RUNNING'))) THEN
  RAISE EXCEPTION 'object is still protected by a lease/job' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER artifact_object_guard BEFORE INSERT OR UPDATE OR DELETE ON artifact_objects FOR EACH ROW EXECUTE FUNCTION protect_artifact_object();
CREATE FUNCTION check_artifact_reference() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE object_row artifact_objects%ROWTYPE;
BEGIN
 IF TG_OP='UPDATE' AND (NEW.id,NEW.version_id,NEW.preview_id,NEW.type,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.version_id,OLD.preview_id,OLD.type,OLD.created_at) THEN
  RAISE EXCEPTION 'artifact identity/purpose is immutable; only current pointer may change' USING ERRCODE='55000'; END IF;
 SELECT * INTO object_row FROM artifact_objects WHERE id=NEW.current_object_id FOR UPDATE;
 -- The composite FK reports identity/type mismatches, including non-existent objects.
 IF FOUND AND object_row.type=NEW.type AND object_row.version_id IS NOT DISTINCT FROM NEW.version_id AND object_row.preview_id IS NOT DISTINCT FROM NEW.preview_id AND object_row.state<>'PUBLISHED' THEN
  RAISE EXCEPTION 'published reference requires a PUBLISHED object' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER artifact_reference_guard BEFORE INSERT OR UPDATE ON artifacts FOR EACH ROW EXECUTE FUNCTION check_artifact_reference();
CREATE FUNCTION check_artifact_read_lease() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE object_state text;
BEGIN
 SELECT state INTO object_state FROM artifact_objects WHERE id=NEW.object_id FOR UPDATE;
 IF object_state IS NOT NULL AND object_state NOT IN ('PUBLISHED','RETIRED') THEN RAISE EXCEPTION 'cannot lease unready/GC object' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER artifact_lease_guard BEFORE INSERT OR UPDATE ON artifact_read_leases FOR EACH ROW EXECUTE FUNCTION check_artifact_read_lease();
CREATE TABLE durability_probes (
 order_id uuid PRIMARY KEY REFERENCES generation_orders(id), probe_nonce uuid NOT NULL,
 checked_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION check_durability_probe() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND (NEW.order_id<>OLD.order_id OR NEW.probe_nonce=OLD.probe_nonce) THEN
  RAISE EXCEPTION 'probe requires same order and a fresh nonce' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT 1 FROM generation_orders WHERE id=NEW.order_id AND billing_state='COMPLETED') THEN
  RAISE EXCEPTION 'durability probe requires completed order' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER durability_probe_guard BEFORE INSERT OR UPDATE ON durability_probes FOR EACH ROW EXECUTE FUNCTION check_durability_probe();
CREATE TABLE delivery_receipts (
 version_id uuid PRIMARY KEY REFERENCES document_versions(id), order_id uuid NOT NULL UNIQUE,
 content_hash text NOT NULL CHECK(content_hash<>''), created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(order_id,version_id) REFERENCES generation_orders(id,version_id)
);
CREATE FUNCTION check_delivery_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM generation_orders o JOIN durability_probes p ON p.order_id=o.id
 JOIN document_versions v ON v.id=o.version_id JOIN preview_manifests m ON m.id=v.manifest_id
 WHERE o.id=NEW.order_id AND o.version_id=NEW.version_id AND o.billing_state='COMPLETED' AND m.content_hash=NEW.content_hash) THEN
  RAISE EXCEPTION 'receipt requires completed matching order, content and durability probe' USING ERRCODE='23514'; END IF;
 IF (SELECT count(*) FROM artifacts a JOIN artifact_objects o ON o.id=a.current_object_id
 WHERE a.version_id=NEW.version_id AND a.type IN ('CLEAN_PDF','CLEAN_PNG') AND o.state='PUBLISHED')<>2 THEN
  RAISE EXCEPTION 'receipt requires both published formal formats' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER delivery_receipt_guard BEFORE INSERT ON delivery_receipts FOR EACH ROW EXECUTE FUNCTION check_delivery_receipt();
CREATE TRIGGER delivery_receipt_immutable BEFORE UPDATE OR DELETE ON delivery_receipts FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
