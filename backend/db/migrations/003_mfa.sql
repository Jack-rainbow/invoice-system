CREATE TABLE mfa_factors (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), state text NOT NULL CHECK(state IN ('NONE','ENROLLING','VERIFIED')),
 seed_ciphertext bytea, factor_version integer NOT NULL CHECK(factor_version>0), last_accepted_step bigint CHECK(last_accepted_step>=0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(id,user_id),
 CHECK((state='NONE' AND seed_ciphertext IS NULL) OR (state<>'NONE' AND seed_ciphertext IS NOT NULL AND octet_length(seed_ciphertext)>0))
);
CREATE UNIQUE INDEX uq_factor_verified ON mfa_factors(user_id) WHERE state='VERIFIED';
CREATE TABLE mfa_challenges (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), context_id uuid NOT NULL,
 purpose text NOT NULL CHECK(purpose IN ('ENROLLMENT','LOGIN','STEP_UP','RECOVERY')),
 factor_version integer NOT NULL CHECK(factor_version>0), expires_at timestamptz NOT NULL, used_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), CHECK(expires_at>created_at)
);
CREATE FUNCTION check_mfa_context() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM auth_pre_sessions WHERE id=NEW.context_id AND user_id=NEW.user_id) AND
 NOT EXISTS(SELECT 1 FROM device_sessions WHERE id=NEW.context_id AND user_id=NEW.user_id) THEN
 RAISE EXCEPTION 'MFA context missing or belongs to another account' USING ERRCODE='23503'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER mfa_context BEFORE INSERT OR UPDATE ON mfa_challenges FOR EACH ROW EXECUTE FUNCTION check_mfa_context();
CREATE TABLE mfa_recovery_codes (
 id uuid PRIMARY KEY, factor_id uuid NOT NULL REFERENCES mfa_factors(id), code_hash bytea NOT NULL UNIQUE CHECK(octet_length(code_hash)>0),
 used_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER mfa_challenge_consumption BEFORE UPDATE ON mfa_challenges FOR EACH ROW EXECUTE FUNCTION protect_consumed_token();
CREATE TRIGGER recovery_code_consumption BEFORE UPDATE ON mfa_recovery_codes FOR EACH ROW EXECUTE FUNCTION protect_consumed_token();
CREATE FUNCTION check_session_live_context() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE account_role text;
BEGIN
 IF NEW.status<>'ACTIVE' THEN RETURN NEW; END IF;
 SELECT role INTO account_role FROM users WHERE id=NEW.user_id AND is_active AND auth_version=NEW.auth_version AND binding_epoch=NEW.binding_epoch;
 IF account_role IS NULL THEN RAISE EXCEPTION 'ACTIVE session requires current active account context' USING ERRCODE='23514'; END IF;
 IF account_role='SUPERADMIN' AND NOT EXISTS(SELECT 1 FROM mfa_factors WHERE user_id=NEW.user_id AND state='VERIFIED' AND factor_version=NEW.factor_version) THEN
  RAISE EXCEPTION 'administrator session requires actual verified MFA factor version' USING ERRCODE='23514'; END IF;
 IF EXISTS(SELECT 1 FROM device_bindings WHERE id=NEW.binding_id AND user_id=NEW.user_id AND epoch=NEW.binding_epoch AND status<>'ACTIVE') THEN
  RAISE EXCEPTION 'ACTIVE session cannot use revoked device binding' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER session_live_context BEFORE INSERT OR UPDATE ON device_sessions FOR EACH ROW EXECUTE FUNCTION check_session_live_context();
