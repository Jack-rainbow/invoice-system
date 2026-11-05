-- Preview rendering starts here. The earlier profile recorded assets:null on
-- purpose; it is revoked rather than edited, and the new profile states the
-- exact template/font bytes it can actually render.
UPDATE render_profiles SET status='REVOKED', updated_at=now()
WHERE id='00000000-0000-4000-8000-000000000001' AND status='ACTIVE';
INSERT INTO render_profiles(id,name,manifest,manifest_hash,status) VALUES(
 '00000000-0000-4000-8000-000000000002',
 'mxn-iva16-raster-v1',
 '{"assets":{"bundle":"invoice-mxn-v1","sha256":{"NotoSans-Bold.ttf":"b3788af7d1d7d09f6227abb1c50185f95fa5b908f6a39343cf3a5913b656eb16","NotoSans-Regular.ttf":"b87e786b03ee28e3d014cbc717a3b3848abf4f650dada84b5a69ee21e48c3d2e","WatermarkCJK.ttf":"d011802f394bef9eb22ffd56a93a37d970e66860c10bf80a5f18fdc5f2ad46e7","template.json":"2d3d5688ec8bf83d5ce2e0e1e64e5799205598bc9e66086ad38c610bbae67eb7"}},"calculation_version":"mxn-iva16-halfup-v1","locale":"es-MX","paper":{"height_pt":"737.28","logical_height":2048,"logical_width":1582,"pt_per_pixel":"0.36","width_pt":"569.52"},"renderer_protocol":"goimage-raster/1","schema_version":"render-profile/1","timezone":"America/Mexico_City","typography":{"min_primary_pt":8,"min_secondary_pt":6},"watermark":{"alpha":0.32,"angle_degrees":30,"max_bytes":2097152,"style_version":"preview-watermark/1","text":"预览 未正式生成","width_px":1000}}'::jsonb,
 '6161df2da8f936c99a88e73f8987f87b5fea9eba82d811e1154a64c3c5791f2b',
 'ACTIVE');

-- FAILED/EXPIRED previews keep a stable machine code; the wording is client-side.
ALTER TABLE preview_requests ADD COLUMN error_code text;
ALTER TABLE preview_requests ADD CONSTRAINT preview_error_code CHECK(
 error_code IS NULL OR error_code IN ('TEXT_DOES_NOT_FIT','DEPENDENCY_UNAVAILABLE','PREVIEW_STALE','PROFILE_REVOKED'));

-- Watermarked preview pixels live in the database so retirement is one
-- transaction with the request state; they are never addressed by clients.
CREATE TABLE preview_objects (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), request_id uuid NOT NULL,
 jpeg bytea NOT NULL CHECK(octet_length(jpeg) BETWEEN 1 AND 2097152),
 sha256 text NOT NULL CHECK(sha256<>''), created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(request_id,owner_id), FOREIGN KEY(request_id,owner_id) REFERENCES preview_requests(id,owner_id)
);
CREATE FUNCTION reject_preview_object_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'preview object content is immutable' USING ERRCODE='55000';
END $$;
CREATE TRIGGER preview_object_immutable BEFORE UPDATE ON preview_objects FOR EACH ROW EXECUTE FUNCTION reject_preview_object_update();

-- One preview slot per owner: the worker claims the oldest queued request.
CREATE INDEX preview_requests_queue ON preview_requests(created_at, id) WHERE state='QUEUED';
CREATE INDEX preview_requests_owner_live ON preview_requests(owner_id, expires_at) WHERE state IN ('QUEUED','RUNNING','READY');

-- A draft that was previewed must still be deletable: nothing paid exists for
-- it, so the owner's preview objects, requests and confirmation manifests are
-- retired together. Content stays immutable; only deletion is permitted, and
-- never while a formal version still references the manifest.
DROP TRIGGER preview_manifest_immutable ON preview_manifests;
CREATE FUNCTION protect_preview_manifest() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  RAISE EXCEPTION 'preview manifest content is immutable' USING ERRCODE='55000';
 END IF;
 IF EXISTS(SELECT 1 FROM document_versions v WHERE v.manifest_id=OLD.id) THEN
  RAISE EXCEPTION 'preview manifest referenced by a formal version' USING ERRCODE='55000';
 END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER preview_manifest_immutable BEFORE UPDATE OR DELETE ON preview_manifests FOR EACH ROW EXECUTE FUNCTION protect_preview_manifest();
