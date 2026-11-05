CREATE TABLE renderer_manifests (
 id uuid PRIMARY KEY, image_digest text NOT NULL CHECK(image_digest<>''), asset_hashes jsonb NOT NULL CHECK(jsonb_typeof(asset_hashes)='object'),
 algorithm_versions jsonb NOT NULL CHECK(jsonb_typeof(algorithm_versions)='object'), locale text NOT NULL CHECK(locale<>''),
 page_settings jsonb NOT NULL CHECK(jsonb_typeof(page_settings)='object'), archive_ref text NOT NULL CHECK(archive_ref<>''),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER renderer_manifest_immutable BEFORE UPDATE OR DELETE ON renderer_manifests FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
ALTER TABLE document_versions ADD CONSTRAINT version_renderer_fk FOREIGN KEY(renderer_manifest_id) REFERENCES renderer_manifests(id);
CREATE TABLE audit_logs (
 id uuid PRIMARY KEY, actor_id uuid REFERENCES users(id), action text NOT NULL CHECK(action<>''), target_type text NOT NULL CHECK(target_type<>''),
 target_id uuid NOT NULL, request_id uuid NOT NULL, redacted_meta jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(redacted_meta)='object'),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_actor_time ON audit_logs(actor_id,created_at,id);
CREATE INDEX audit_target_time ON audit_logs(target_type,target_id,created_at,id);
CREATE TRIGGER audit_immutable BEFORE UPDATE OR DELETE ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TABLE recovery_cases (
 id uuid PRIMARY KEY, incident_id uuid NOT NULL, account_id uuid NOT NULL REFERENCES credit_accounts(user_id), operation_id uuid NOT NULL,
 evidence_refs jsonb NOT NULL CHECK(jsonb_typeof(evidence_refs)='array'),
 status text NOT NULL CHECK(status IN ('PENDING','APPROVED','APPLIED','REJECTED')), approved_by uuid REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(incident_id,operation_id),
 CHECK(status NOT IN ('APPROVED','APPLIED') OR approved_by IS NOT NULL)
);
CREATE FUNCTION check_adjust_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind='ADJUST' AND NOT EXISTS(SELECT 1 FROM recovery_cases c JOIN users u ON u.id=c.approved_by
 WHERE c.operation_id=NEW.operation_id AND c.account_id=NEW.account_id AND c.status='APPROVED' AND u.role='SUPERADMIN' AND u.is_active) THEN
  RAISE EXCEPTION 'ADJUST requires matching approved recovery case' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER credit_adjust_approval BEFORE INSERT ON credit_entries FOR EACH ROW EXECUTE FUNCTION check_adjust_approval();
-- Polymorphic references must still name a real aggregate of the declared purpose.
CREATE FUNCTION check_outbox_aggregate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.event_type='FINALIZE_DELIVERY' AND NOT EXISTS(SELECT 1 FROM generation_orders WHERE id=NEW.aggregate_id)) OR
 (NEW.event_type='RENDER_PREVIEW' AND NOT EXISTS(SELECT 1 FROM preview_requests WHERE id=NEW.aggregate_id)) OR
 (NEW.event_type IN ('RENDER_GENERATE','RENDER_REPAIR') AND NOT EXISTS(SELECT 1 FROM render_jobs WHERE id=NEW.aggregate_id AND kind=CASE NEW.event_type WHEN 'RENDER_GENERATE' THEN 'GENERATE' ELSE 'REPAIR' END)) THEN
  RAISE EXCEPTION 'outbox aggregate/purpose mismatch' USING ERRCODE='23503'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER outbox_aggregate_guard BEFORE INSERT OR UPDATE ON outbox_events FOR EACH ROW EXECUTE FUNCTION check_outbox_aggregate();
-- Explicit privilege floor: deployment grants a separate app role only necessary rights.
-- Triggers protect owner-role accidental UPDATE/DELETE too; superusers remain outside this boundary.
REVOKE UPDATE, DELETE, TRUNCATE ON credit_entries, document_versions, preview_manifests, renderer_manifests, delivery_receipts, audit_logs FROM PUBLIC;

-- UPDATE/DELETE triggers do not see TRUNCATE; protect that separate operation too.
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON credit_entries FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER version_no_truncate BEFORE TRUNCATE ON document_versions FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER preview_manifest_no_truncate BEFORE TRUNCATE ON preview_manifests FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER renderer_manifest_no_truncate BEFORE TRUNCATE ON renderer_manifests FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER profile_no_truncate BEFORE TRUNCATE ON render_profiles FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER receipt_no_truncate BEFORE TRUNCATE ON delivery_receipts FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER audit_no_truncate BEFORE TRUNCATE ON audit_logs FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER order_no_truncate BEFORE TRUNCATE ON generation_orders FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER object_no_truncate BEFORE TRUNCATE ON artifact_objects FOR EACH STATEMENT EXECUTE FUNCTION reject_immutable_change();
