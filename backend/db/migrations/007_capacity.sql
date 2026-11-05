CREATE TABLE scheduler_lanes (
 id uuid PRIMARY KEY, queue_name text NOT NULL CHECK(queue_name IN ('formal','preview')), enabled boolean NOT NULL DEFAULT true,
 booked_until timestamptz, plan_version integer NOT NULL DEFAULT 1 CHECK(plan_version>0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE capacity_tickets (
 id uuid PRIMARY KEY, lane_id uuid NOT NULL REFERENCES scheduler_lanes(id), owner_id uuid NOT NULL REFERENCES users(id),
 job_id uuid NOT NULL UNIQUE REFERENCES render_jobs(id), slot_start timestamptz NOT NULL, slot_end timestamptz NOT NULL,
 deadline timestamptz NOT NULL, status text NOT NULL CHECK(status IN ('BOOKED','RUNNING','COMPLETED','RELEASED','CANCELLED')),
 plan_version integer NOT NULL CHECK(plan_version>0), created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(slot_start<slot_end AND slot_end<=deadline)
);
CREATE INDEX capacity_lane_active ON capacity_tickets(lane_id,slot_start,slot_end) WHERE status IN ('BOOKED','RUNNING');
-- Serialize every ticket mutation on its lane, even for a caller bypassing the planner.
-- READ COMMITTED service transactions acquire this lane lock before computing slots.
CREATE FUNCTION check_capacity_ticket() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW.lane_id<>OLD.lane_id THEN RAISE EXCEPTION 'ticket lane is immutable' USING ERRCODE='55000'; END IF;
 PERFORM 1 FROM scheduler_lanes WHERE id=NEW.lane_id FOR UPDATE;
 IF NOT EXISTS(SELECT 1 FROM render_jobs j JOIN document_versions v ON v.id=j.version_id WHERE j.id=NEW.job_id AND v.owner_id=NEW.owner_id) THEN
  RAISE EXCEPTION 'capacity ticket owner differs from job owner' USING ERRCODE='23503'; END IF;
 IF NEW.status IN ('BOOKED','RUNNING') AND EXISTS(SELECT 1 FROM capacity_tickets t WHERE t.lane_id=NEW.lane_id AND t.id<>NEW.id
 AND t.status IN ('BOOKED','RUNNING') AND tstzrange(t.slot_start,t.slot_end,'[)') && tstzrange(NEW.slot_start,NEW.slot_end,'[)')) THEN
  RAISE EXCEPTION 'lane capacity overlap' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER capacity_ticket_guard BEFORE INSERT OR UPDATE ON capacity_tickets FOR EACH ROW EXECUTE FUNCTION check_capacity_ticket();
CREATE TABLE batches (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), state text NOT NULL CHECK(state IN ('QUEUED','RUNNING','COMPLETED','PARTIAL_FAILED','FAILED')),
 expected_count integer NOT NULL CHECK(expected_count BETWEEN 1 AND 100), request_hash text NOT NULL CHECK(request_hash<>''),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,owner_id)
);
CREATE TABLE batch_drafts (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), row_document_ids jsonb NOT NULL,
 source_hash text NOT NULL CHECK(source_hash<>''), created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(jsonb_typeof(row_document_ids)='array' AND jsonb_array_length(row_document_ids) BETWEEN 1 AND 100), UNIQUE(owner_id,source_hash)
);
CREATE FUNCTION check_batch_draft_documents() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE document_text text;
BEGIN
 FOR document_text IN SELECT jsonb_array_elements_text(NEW.row_document_ids) LOOP
  IF NOT EXISTS(SELECT 1 FROM documents WHERE id=document_text::uuid AND owner_id=NEW.owner_id) THEN
   RAISE EXCEPTION 'batch draft document owner mismatch' USING ERRCODE='23503'; END IF;
 END LOOP;
 RETURN NEW;
END $$;
CREATE TRIGGER batch_draft_guard BEFORE INSERT OR UPDATE ON batch_drafts FOR EACH ROW EXECUTE FUNCTION check_batch_draft_documents();
CREATE TABLE batch_items (
 id uuid PRIMARY KEY, batch_id uuid NOT NULL REFERENCES batches(id), row_no integer NOT NULL CHECK(row_no BETWEEN 1 AND 100),
 document_id uuid NOT NULL REFERENCES documents(id), version_id uuid NOT NULL, order_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(batch_id,row_no), UNIQUE(batch_id,order_id),
 FOREIGN KEY(order_id,version_id) REFERENCES generation_orders(id,version_id)
);
CREATE FUNCTION check_batch_item_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM batches b JOIN document_versions v ON v.owner_id=b.owner_id WHERE b.id=NEW.batch_id AND v.id=NEW.version_id AND v.document_id=NEW.document_id AND NEW.row_no<=b.expected_count) THEN
  RAISE EXCEPTION 'batch row owner/document/version mismatch' USING ERRCODE='23503'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER batch_item_guard BEFORE INSERT OR UPDATE ON batch_items FOR EACH ROW EXECUTE FUNCTION check_batch_item_owner();
