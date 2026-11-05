-- PostgreSQL 16/17; UUIDs are supplied by the server application, not an extension.
CREATE TABLE users (
 id uuid PRIMARY KEY, username text NOT NULL UNIQUE,
 role text NOT NULL CHECK(role IN ('USER','SUPERADMIN')),
 is_active boolean NOT NULL DEFAULT true,
 onboarding_state text NOT NULL DEFAULT 'NEEDS_SETUP' CHECK(onboarding_state IN ('NEEDS_SETUP','ENROLLMENT_REQUIRED','READY')),
 customer_verification_ref text,
 auth_version integer NOT NULL DEFAULT 1 CHECK(auth_version>0),
 binding_epoch integer NOT NULL DEFAULT 1 CHECK(binding_epoch>0),
 device_state text NOT NULL CHECK(device_state IN ('NOT_REQUIRED','INITIAL_PENDING','ACTIVE','RESET_PENDING')),
 recovery_state text NOT NULL DEFAULT 'NORMAL' CHECK(recovery_state IN ('NORMAL','RECOVERY_HOLD')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(username ~ '^[a-z0-9][a-z0-9._-]{2,63}$')
);
CREATE TABLE password_credentials (
 user_id uuid PRIMARY KEY REFERENCES users(id), password_phc text NOT NULL CHECK(password_phc LIKE '$argon2id$%'),
 version integer NOT NULL CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE password_tokens (
 id uuid PRIMARY KEY, token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)>0), user_id uuid NOT NULL REFERENCES users(id),
 purpose text NOT NULL CHECK(purpose IN ('INITIAL_SETUP','PASSWORD_RESET','ADMIN_BOOTSTRAP')),
 auth_version integer NOT NULL CHECK(auth_version>0), expires_at timestamptz NOT NULL, used_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), CHECK(expires_at>created_at)
);
CREATE INDEX password_tokens_user ON password_tokens(user_id,purpose);
CREATE TABLE auth_pre_sessions (
 id uuid PRIMARY KEY, token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)>0), user_id uuid NOT NULL REFERENCES users(id),
 purpose text NOT NULL CHECK(purpose IN ('INITIAL_SETUP','PASSWORD_RESET','ADMIN_BOOTSTRAP','ENROLLMENT','MFA','PAIR','BIND','LOGIN')),
 auth_version integer NOT NULL CHECK(auth_version>0), binding_epoch integer NOT NULL CHECK(binding_epoch>0),
 expires_at timestamptz NOT NULL, used_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), CHECK(expires_at>created_at)
);
CREATE INDEX auth_pre_sessions_user ON auth_pre_sessions(user_id,expires_at);
-- All immutable records use this trigger even if a caller has table UPDATE privileges.
CREATE FUNCTION reject_immutable_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION '% is append-only/immutable', TG_TABLE_NAME USING ERRCODE='55000'; END $$;

CREATE FUNCTION initialize_user_device_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.device_state IS NULL THEN NEW.device_state := CASE NEW.role WHEN 'SUPERADMIN' THEN 'NOT_REQUIRED' ELSE 'INITIAL_PENDING' END; END IF;
 IF NEW.role='SUPERADMIN' AND NEW.device_state<>'NOT_REQUIRED' THEN RAISE EXCEPTION 'administrator does not use customer device binding' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND (NEW.id<>OLD.id OR NEW.role<>OLD.role) THEN RAISE EXCEPTION 'account identity and role are immutable' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER user_identity BEFORE INSERT OR UPDATE ON users FOR EACH ROW EXECUTE FUNCTION initialize_user_device_state();
CREATE FUNCTION protect_consumed_token() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (to_jsonb(NEW)-'used_at') IS DISTINCT FROM (to_jsonb(OLD)-'used_at') OR (OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at) THEN
  RAISE EXCEPTION 'token identity is immutable and consumption is one-way' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER password_token_consumption BEFORE UPDATE ON password_tokens FOR EACH ROW EXECUTE FUNCTION protect_consumed_token();
CREATE TRIGGER pre_session_consumption BEFORE UPDATE ON auth_pre_sessions FOR EACH ROW EXECUTE FUNCTION protect_consumed_token();
CREATE FUNCTION valid_single_line_text(value text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT value !~ '[[:cntrl:]]' AND translate(value,
 chr(8234)||chr(8235)||chr(8236)||chr(8237)||chr(8238)||chr(8294)||chr(8295)||chr(8296)||chr(8297),'')=value
$$;
