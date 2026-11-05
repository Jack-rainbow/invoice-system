CREATE FUNCTION valid_public_jwk(j jsonb) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT coalesce(jsonb_typeof(j)='object' AND j->>'kty'='EC' AND j->>'crv'='P-256'
 AND j->>'x' ~ '^[A-Za-z0-9_-]{43}$' AND j->>'y' ~ '^[A-Za-z0-9_-]{43}$'
 AND NOT (j ? 'd') AND (j - ARRAY['kty','crv','x','y'])='{}'::jsonb,false)
$$;
CREATE TABLE device_bindings (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), public_jwk jsonb NOT NULL CHECK(valid_public_jwk(public_jwk)),
 key_thumbprint text NOT NULL CHECK(key_thumbprint<>''), epoch integer NOT NULL CHECK(epoch>0),
 status text NOT NULL CHECK(status IN ('ACTIVE','REVOKED')), created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,user_id,epoch)
);
CREATE UNIQUE INDEX uq_binding_active ON device_bindings(user_id) WHERE status='ACTIVE';
CREATE TABLE device_pairings (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), public_jwk jsonb NOT NULL CHECK(valid_public_jwk(public_jwk)),
 key_thumbprint text NOT NULL CHECK(key_thumbprint<>''), proof_verified boolean NOT NULL DEFAULT false,
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,user_id), CHECK(expires_at>created_at)
);
CREATE INDEX device_pairings_user ON device_pairings(user_id,expires_at);
CREATE TABLE binding_transfers (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), pairing_id uuid NOT NULL, target_key_hash text NOT NULL,
 target_epoch integer NOT NULL CHECK(target_epoch>0), purpose text NOT NULL CHECK(purpose IN ('INITIAL','RESET')),
 status text NOT NULL DEFAULT 'PENDING' CHECK(status IN ('PENDING','CONSUMED','CANCELLED','EXPIRED')),
 approved_by uuid REFERENCES users(id), verification_ref text, expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(pairing_id,user_id) REFERENCES device_pairings(id,user_id), CHECK(expires_at>created_at),
 CHECK(purpose<>'RESET' OR (approved_by IS NOT NULL AND nullif(btrim(verification_ref),'') IS NOT NULL))
);
CREATE UNIQUE INDEX uq_transfer_pending ON binding_transfers(user_id) WHERE status='PENDING';
CREATE FUNCTION check_transfer_target() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM device_pairings p WHERE p.id=NEW.pairing_id AND p.user_id=NEW.user_id AND p.key_thumbprint=NEW.target_key_hash AND p.proof_verified) THEN
 RAISE EXCEPTION 'transfer requires matching proven pairing' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER transfer_target BEFORE INSERT OR UPDATE ON binding_transfers FOR EACH ROW EXECUTE FUNCTION check_transfer_target();
CREATE TABLE device_sessions (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)>0),
 binding_id uuid, generation uuid NOT NULL UNIQUE, auth_version integer NOT NULL CHECK(auth_version>0), binding_epoch integer NOT NULL CHECK(binding_epoch>0),
 factor_version integer CHECK(factor_version>0), mfa_verified_at timestamptz, last_active_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL, status text NOT NULL CHECK(status IN ('ACTIVE','REVOKED','EXPIRED')),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,user_id),
 FOREIGN KEY(binding_id,user_id,binding_epoch) REFERENCES device_bindings(id,user_id,epoch), CHECK(expires_at>created_at),
 CHECK((factor_version IS NULL)=(mfa_verified_at IS NULL))
);
CREATE UNIQUE INDEX uq_session_active ON device_sessions(user_id) WHERE status='ACTIVE';
CREATE FUNCTION check_session_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE account_role text;
BEGIN
 SELECT role INTO account_role FROM users WHERE id=NEW.user_id;
 IF account_role='USER' AND NEW.binding_id IS NULL THEN RAISE EXCEPTION 'USER session requires device binding' USING ERRCODE='23514'; END IF;
 IF account_role='SUPERADMIN' AND NEW.status='ACTIVE' AND NEW.factor_version IS NULL THEN RAISE EXCEPTION 'administrator session requires MFA' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER session_identity BEFORE INSERT OR UPDATE ON device_sessions FOR EACH ROW EXECUTE FUNCTION check_session_identity();
CREATE TABLE device_challenges (
 id uuid PRIMARY KEY, nonce_hash bytea NOT NULL UNIQUE CHECK(octet_length(nonce_hash)>0),
 purpose text NOT NULL CHECK(purpose IN ('INITIAL_SETUP','PAIR','BIND','LOGIN','REQUEST')),
 subject_id uuid NOT NULL, subject_kind text NOT NULL CHECK(subject_kind IN ('PASSWORD_TOKEN','PRE_SESSION','SESSION')),
 binding_transfer_id uuid REFERENCES binding_transfers(id), key_hash text NOT NULL CHECK(key_hash<>''), auth_version integer NOT NULL CHECK(auth_version>0),
 epoch integer NOT NULL CHECK(epoch>0), generation uuid, expires_at timestamptz NOT NULL, used_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), CHECK(expires_at>created_at), CHECK(generation IS NOT NULL),
 CHECK((purpose='INITIAL_SETUP' AND subject_kind='PASSWORD_TOKEN') OR (purpose IN ('PAIR','BIND','LOGIN') AND subject_kind='PRE_SESSION') OR (purpose='REQUEST' AND subject_kind='SESSION')),
 CHECK((purpose='BIND')=(binding_transfer_id IS NOT NULL))
);
CREATE INDEX device_challenges_subject ON device_challenges(subject_id,purpose,expires_at);

CREATE FUNCTION check_device_challenge_subject() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE account_id uuid; context_auth integer; context_epoch integer; context_generation uuid; expected_key text;
BEGIN
 IF NEW.purpose='INITIAL_SETUP' THEN
  SELECT p.user_id,p.auth_version,u.binding_epoch,p.id INTO account_id,context_auth,context_epoch,context_generation
   FROM password_tokens p JOIN users u ON u.id=p.user_id WHERE p.id=NEW.subject_id AND p.purpose='INITIAL_SETUP';
 ELSIF NEW.purpose='REQUEST' THEN
  SELECT s.user_id,s.auth_version,s.binding_epoch,s.generation,b.key_thumbprint INTO account_id,context_auth,context_epoch,context_generation,expected_key
   FROM device_sessions s JOIN device_bindings b ON b.id=s.binding_id AND b.user_id=s.user_id WHERE s.id=NEW.subject_id AND s.status='ACTIVE' AND b.status='ACTIVE';
 ELSE
  SELECT p.user_id,p.auth_version,p.binding_epoch,p.id INTO account_id,context_auth,context_epoch,context_generation
   FROM auth_pre_sessions p WHERE p.id=NEW.subject_id AND
   (p.purpose=NEW.purpose OR (NEW.purpose='PAIR' AND p.purpose='LOGIN'));
  IF NEW.purpose='BIND' THEN
   SELECT target_key_hash INTO expected_key FROM binding_transfers WHERE id=NEW.binding_transfer_id AND user_id=account_id AND target_epoch=NEW.epoch AND status='PENDING';
   IF expected_key IS NULL THEN RAISE EXCEPTION 'BIND requires matching pending target transfer' USING ERRCODE='23503'; END IF;
  ELSIF NEW.purpose='LOGIN' THEN
   SELECT key_thumbprint INTO expected_key FROM device_bindings WHERE user_id=account_id AND epoch=NEW.epoch AND status='ACTIVE';
   IF expected_key IS NULL THEN RAISE EXCEPTION 'LOGIN requires current account binding' USING ERRCODE='23503'; END IF;
  END IF;
 END IF;
 IF account_id IS NULL THEN RAISE EXCEPTION 'challenge subject does not exist or has wrong purpose' USING ERRCODE='23503'; END IF;
 IF NOT EXISTS(SELECT 1 FROM users WHERE id=account_id AND is_active AND auth_version=NEW.auth_version AND binding_epoch=NEW.epoch) THEN
  RAISE EXCEPTION 'challenge account is revoked or context versions are stale' USING ERRCODE='23514'; END IF;
 IF (NEW.auth_version,NEW.epoch,NEW.generation) IS DISTINCT FROM (context_auth,context_epoch,context_generation) OR (expected_key IS NOT NULL AND expected_key<>NEW.key_hash) THEN
  RAISE EXCEPTION 'challenge context/key/version mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER challenge_subject BEFORE INSERT OR UPDATE ON device_challenges FOR EACH ROW EXECUTE FUNCTION check_device_challenge_subject();
CREATE TRIGGER challenge_consumption BEFORE UPDATE ON device_challenges FOR EACH ROW EXECUTE FUNCTION protect_consumed_token();
CREATE FUNCTION protect_device_binding_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (to_jsonb(NEW)-ARRAY['status','updated_at']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','updated_at']) THEN
  RAISE EXCEPTION 'device binding key/owner/epoch is immutable' USING ERRCODE='55000'; END IF;
 IF OLD.status='REVOKED' AND NEW.status<>'REVOKED' THEN RAISE EXCEPTION 'revoked binding cannot be reactivated' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER binding_identity BEFORE UPDATE ON device_bindings FOR EACH ROW EXECUTE FUNCTION protect_device_binding_identity();
CREATE FUNCTION protect_device_session_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (to_jsonb(NEW)-ARRAY['status','last_active_at','factor_version','mfa_verified_at','updated_at']) IS DISTINCT FROM
 (to_jsonb(OLD)-ARRAY['status','last_active_at','factor_version','mfa_verified_at','updated_at']) THEN
  RAISE EXCEPTION 'session identity and authorization versions are immutable' USING ERRCODE='55000'; END IF;
 IF OLD.status<>'ACTIVE' AND NEW.status='ACTIVE' THEN RAISE EXCEPTION 'old session cannot be reactivated' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER session_content BEFORE UPDATE ON device_sessions FOR EACH ROW EXECUTE FUNCTION protect_device_session_identity();
