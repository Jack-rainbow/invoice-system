CREATE TABLE render_profiles (
 id uuid PRIMARY KEY, name text NOT NULL CHECK(btrim(name)<>''), manifest jsonb NOT NULL CHECK(jsonb_typeof(manifest)='object'),
 manifest_hash text NOT NULL UNIQUE CHECK(manifest_hash<>''), status text NOT NULL CHECK(status IN ('ACTIVE','REVOKED')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION protect_profile_content() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'profile history cannot be deleted' USING ERRCODE='55000'; END IF;
 IF (to_jsonb(NEW)-ARRAY['status','updated_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','updated_at']) THEN
 RAISE EXCEPTION 'profile content is immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER profile_immutable BEFORE UPDATE OR DELETE ON render_profiles FOR EACH ROW EXECUTE FUNCTION protect_profile_content();
CREATE TABLE documents (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), draft_json jsonb NOT NULL CHECK(jsonb_typeof(draft_json)='object'),
 revision integer NOT NULL DEFAULT 1 CHECK(revision>0), render_profile_id uuid NOT NULL REFERENCES render_profiles(id), title text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,owner_id)
);
CREATE INDEX documents_owner_time ON documents(owner_id,created_at,id);
CREATE TABLE preview_manifests (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), document_id uuid NOT NULL, revision integer NOT NULL CHECK(revision>0),
 profile_id uuid NOT NULL REFERENCES render_profiles(id), canonical_bytes bytea NOT NULL CHECK(octet_length(canonical_bytes)>0),
 content_hash text NOT NULL CHECK(content_hash<>''), amounts jsonb NOT NULL CHECK(jsonb_typeof(amounts)='object'), expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY(document_id,owner_id) REFERENCES documents(id,owner_id),
 UNIQUE(id,document_id,owner_id,revision), UNIQUE(id,document_id,owner_id,revision,profile_id,content_hash), UNIQUE(id,owner_id), CHECK(expires_at>created_at)
);
CREATE TRIGGER preview_manifest_immutable BEFORE UPDATE OR DELETE ON preview_manifests FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TABLE preview_requests (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), document_id uuid NOT NULL, revision integer NOT NULL CHECK(revision>0),
 profile_id uuid NOT NULL REFERENCES render_profiles(id), content_hash text NOT NULL CHECK(content_hash<>''),
 state text NOT NULL CHECK(state IN ('QUEUED','RUNNING','READY','FAILED','EXPIRED')), manifest_id uuid,
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,owner_id), FOREIGN KEY(document_id,owner_id) REFERENCES documents(id,owner_id),
 CONSTRAINT preview_manifest_content_fk FOREIGN KEY(manifest_id,document_id,owner_id,revision,profile_id,content_hash) REFERENCES preview_manifests(id,document_id,owner_id,revision,profile_id,content_hash),
 CHECK(state<>'READY' OR manifest_id IS NOT NULL), CHECK(expires_at>created_at)
);
CREATE UNIQUE INDEX uq_preview_running ON preview_requests(owner_id) WHERE state='RUNNING';
CREATE UNIQUE INDEX uq_preview_waiting ON preview_requests(owner_id) WHERE state='QUEUED';
CREATE UNIQUE INDEX uq_preview_content ON preview_requests(owner_id,document_id,revision,profile_id,content_hash) WHERE state IN ('QUEUED','RUNNING','READY');
CREATE TABLE product_presets (
 id uuid PRIMARY KEY, owner_id uuid NOT NULL REFERENCES users(id), name text NOT NULL CHECK(btrim(name)<>''), fields_json jsonb NOT NULL CHECK(jsonb_typeof(fields_json)='object'),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(owner_id,name)
);
-- Draft ownership and monotonically increasing revisions are persisted invariants.
CREATE FUNCTION protect_document_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.owner_id<>OLD.owner_id THEN RAISE EXCEPTION 'document identity/owner is immutable' USING ERRCODE='55000'; END IF;
 IF (NEW.draft_json,NEW.render_profile_id,NEW.title) IS DISTINCT FROM (OLD.draft_json,OLD.render_profile_id,OLD.title) THEN
  IF NEW.revision<>OLD.revision+1 THEN RAISE EXCEPTION 'changed draft requires next revision' USING ERRCODE='23514'; END IF;
 ELSIF NEW.revision<>OLD.revision THEN RAISE EXCEPTION 'revision cannot change without draft change' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_revision_guard BEFORE UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION protect_document_revision();
ALTER TABLE render_profiles ADD CONSTRAINT profile_name_single_line CHECK(valid_single_line_text(name));
ALTER TABLE documents ADD CONSTRAINT document_title_single_line CHECK(valid_single_line_text(title));
ALTER TABLE product_presets ADD CONSTRAINT preset_name_single_line CHECK(valid_single_line_text(name));
