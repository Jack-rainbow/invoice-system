CREATE TABLE document_versions (
 id uuid PRIMARY KEY, document_id uuid NOT NULL, owner_id uuid NOT NULL REFERENCES users(id),
 version_no integer NOT NULL CHECK(version_no>0), source_revision integer NOT NULL CHECK(source_revision>0),
 manifest_id uuid NOT NULL, renderer_manifest_id uuid NOT NULL,
 snapshot_json jsonb NOT NULL CHECK(jsonb_typeof(snapshot_json)='object'), unit_cents bigint NOT NULL, qty_micros bigint NOT NULL,
 subtotal_cents bigint NOT NULL, tax_cents bigint NOT NULL, total_cents bigint NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,owner_id), UNIQUE(id,document_id,owner_id),
 FOREIGN KEY(document_id,owner_id) REFERENCES documents(id,owner_id),
 FOREIGN KEY(manifest_id,document_id,owner_id,source_revision) REFERENCES preview_manifests(id,document_id,owner_id,revision),
 CONSTRAINT ck_totals CHECK(unit_cents>=0 AND qty_micros>=0 AND subtotal_cents>=0
 AND subtotal_cents::numeric=trunc((unit_cents::numeric*qty_micros::numeric+500000)/1000000)
 AND tax_cents::numeric=trunc((subtotal_cents::numeric*16+50)/100)
 AND total_cents::numeric=subtotal_cents::numeric+tax_cents::numeric AND total_cents<=99999999999)
);
CREATE UNIQUE INDEX uq_version_revision ON document_versions(document_id,source_revision);
CREATE UNIQUE INDEX uq_version_number ON document_versions(document_id,version_no);
CREATE TRIGGER version_immutable BEFORE UPDATE OR DELETE ON document_versions FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TABLE generation_orders (
 id uuid PRIMARY KEY, version_id uuid NOT NULL, owner_id uuid NOT NULL REFERENCES users(id), cost_points smallint NOT NULL,
 billing_state text NOT NULL CHECK(billing_state IN ('RESERVED','COMPLETED','FAILED_RELEASED')),
 reserve_attempt integer NOT NULL DEFAULT 1 CHECK(reserve_attempt>0), deadline timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT ck_cost CHECK(cost_points IN (0,1)), FOREIGN KEY(version_id,owner_id) REFERENCES document_versions(id,owner_id),
 UNIQUE(id,version_id), CHECK(deadline>created_at)
);
CREATE UNIQUE INDEX uq_order_version ON generation_orders(version_id);
CREATE FUNCTION protect_order_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'order entitlement history cannot be deleted' USING ERRCODE='55000'; END IF;
 IF TG_OP='UPDATE' THEN
  IF (NEW.id,NEW.version_id,NEW.owner_id,NEW.cost_points,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.version_id,OLD.owner_id,OLD.cost_points,OLD.created_at) THEN
   RAISE EXCEPTION 'order identity/cost cannot change' USING ERRCODE='55000'; END IF;
  IF OLD.billing_state='COMPLETED' AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'completed entitlement cannot change' USING ERRCODE='55000'; END IF;
  IF NEW.billing_state<>OLD.billing_state AND NOT (
   (OLD.billing_state='RESERVED' AND NEW.billing_state IN ('COMPLETED','FAILED_RELEASED')) OR
   (OLD.billing_state='FAILED_RELEASED' AND NEW.billing_state='RESERVED')) THEN
   RAISE EXCEPTION 'invalid order billing transition' USING ERRCODE='23514'; END IF;
  IF OLD.billing_state='FAILED_RELEASED' AND NEW.billing_state='RESERVED' THEN
   IF NEW.reserve_attempt::bigint<>OLD.reserve_attempt::bigint+1 THEN RAISE EXCEPTION 'retry requires next reserve attempt' USING ERRCODE='23514'; END IF;
  ELSIF NEW.reserve_attempt<>OLD.reserve_attempt THEN RAISE EXCEPTION 'attempt changes only on explicit retry' USING ERRCODE='23514'; END IF;
 ELSIF NEW.billing_state<>'RESERVED' OR NEW.reserve_attempt<>1 THEN
  RAISE EXCEPTION 'new order must start RESERVED at attempt 1' USING ERRCODE='23514';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM users WHERE id=NEW.owner_id AND ((role='USER' AND NEW.cost_points=1) OR (role='SUPERADMIN' AND NEW.cost_points=0))) THEN
  RAISE EXCEPTION 'cost does not match account role' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER order_identity BEFORE INSERT OR UPDATE OR DELETE ON generation_orders FOR EACH ROW EXECUTE FUNCTION protect_order_identity();
CREATE TABLE render_jobs (
 id uuid PRIMARY KEY, kind text NOT NULL CHECK(kind IN ('GENERATE','REPAIR')), order_id uuid, version_id uuid NOT NULL REFERENCES document_versions(id),
 state text NOT NULL CHECK(state IN ('QUEUED','RUNNING','RETRY_WAIT','SUCCEEDED','FAILED','CANCELLED')),
 reserve_attempt integer NOT NULL CHECK(reserve_attempt>0), attempt_no integer NOT NULL DEFAULT 0 CHECK(attempt_no>=0),
 lease_token uuid, lease_until timestamptz, next_attempt_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(order_id,version_id) REFERENCES generation_orders(id,version_id),
 CONSTRAINT ck_job_order_relation CHECK((kind='GENERATE' AND order_id IS NOT NULL) OR (kind='REPAIR' AND order_id IS NULL)),
 CHECK((lease_token IS NULL)=(lease_until IS NULL)), CHECK(state<>'RUNNING' OR (lease_token IS NOT NULL AND attempt_no>0))
);
CREATE UNIQUE INDEX uq_job_order ON render_jobs(order_id) WHERE kind='GENERATE';
CREATE UNIQUE INDEX uq_repair_active ON render_jobs(version_id) WHERE kind='REPAIR' AND state IN ('QUEUED','RUNNING','RETRY_WAIT');
CREATE INDEX render_jobs_recovery ON render_jobs(state,next_attempt_at,lease_until);
CREATE FUNCTION check_repair_entitlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind='REPAIR' AND NOT EXISTS(SELECT 1 FROM generation_orders WHERE version_id=NEW.version_id AND billing_state='COMPLETED') THEN
  RAISE EXCEPTION 'free REPAIR requires completed version entitlement' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER repair_entitlement BEFORE INSERT OR UPDATE ON render_jobs FOR EACH ROW EXECUTE FUNCTION check_repair_entitlement();
CREATE TABLE outbox_events (
 id uuid PRIMARY KEY, event_type text NOT NULL CHECK(event_type IN ('RENDER_GENERATE','RENDER_REPAIR','RENDER_PREVIEW','FINALIZE_DELIVERY')),
 aggregate_id uuid NOT NULL, payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object'),
 status text NOT NULL DEFAULT 'PENDING' CHECK(status IN ('PENDING','DELIVERED','FAILED')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_pending ON outbox_events(next_attempt_at,id) WHERE status='PENDING';
