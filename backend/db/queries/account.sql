-- name: AccountClock :one
SELECT clock_timestamp()::timestamptz;

-- name: AccountByName :one
SELECT * FROM users WHERE username=$1;

-- name: PasswordByUser :one
SELECT * FROM password_credentials WHERE user_id=$1;

-- name: BootstrapTokenByHash :one
SELECT * FROM password_tokens WHERE token_hash=$1 AND purpose='ADMIN_BOOTSTRAP';

-- name: LockPasswordToken :one
SELECT * FROM password_tokens WHERE id=$1 FOR UPDATE;

-- name: ConsumePasswordToken :execrows
UPDATE password_tokens SET used_at=clock_timestamp() WHERE id=$1 AND used_at IS NULL AND expires_at>clock_timestamp();

-- name: InsertAccount :exec
INSERT INTO users(id,username,role,onboarding_state,customer_verification_ref)
VALUES($1,$2,$3,$4,$5);

-- name: InsertZeroWallet :exec
INSERT INTO credit_accounts(user_id) VALUES($1);

-- name: InsertPasswordToken :exec
INSERT INTO password_tokens(id,token_hash,user_id,purpose,auth_version,expires_at)
VALUES($1,$2,$3,$4,$5,$6);

-- name: InvalidatePasswordTokens :exec
UPDATE password_tokens SET used_at=clock_timestamp() WHERE user_id=$1 AND used_at IS NULL;

-- name: InvalidatePreSessions :exec
UPDATE auth_pre_sessions SET used_at=clock_timestamp() WHERE user_id=$1 AND used_at IS NULL;

-- name: ResetUnreadyAdmin :execrows
UPDATE users SET auth_version=auth_version+1,updated_at=clock_timestamp()
WHERE id=$1 AND role='SUPERADMIN' AND onboarding_state='ENROLLMENT_REQUIRED';

-- name: CountAdministrators :one
SELECT count(*) FROM users WHERE role='SUPERADMIN';

-- name: DisableEnrollingFactors :exec
UPDATE mfa_factors SET state='NONE',seed_ciphertext=NULL,updated_at=clock_timestamp()
WHERE user_id=$1 AND state='ENROLLING';

-- name: UpsertPassword :exec
INSERT INTO password_credentials(user_id,password_phc,version) VALUES($1,$2,1)
ON CONFLICT(user_id) DO UPDATE SET password_phc=excluded.password_phc,version=password_credentials.version+1,updated_at=clock_timestamp();

-- name: InsertPreSession :exec
INSERT INTO auth_pre_sessions(id,token_hash,user_id,purpose,auth_version,binding_epoch,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: PreSessionByHash :one
SELECT * FROM auth_pre_sessions WHERE token_hash=$1;

-- name: LockPreSession :one
SELECT * FROM auth_pre_sessions WHERE id=$1 FOR UPDATE;

-- name: ConsumePreSession :execrows
UPDATE auth_pre_sessions SET used_at=clock_timestamp() WHERE id=$1 AND used_at IS NULL AND expires_at>clock_timestamp();

-- name: InsertEnrollingFactor :exec
INSERT INTO mfa_factors(id,user_id,state,seed_ciphertext,factor_version)
VALUES($1,$2,'ENROLLING',$3,1);

-- name: LockEnrollingFactor :one
SELECT * FROM mfa_factors WHERE user_id=$1 AND state='ENROLLING' FOR UPDATE;

-- name: LockVerifiedFactor :one
SELECT * FROM mfa_factors WHERE user_id=$1 AND state='VERIFIED' FOR UPDATE;

-- name: VerifyEnrollingFactor :execrows
UPDATE mfa_factors SET state='VERIFIED',last_accepted_step=$2,updated_at=clock_timestamp()
WHERE id=$1 AND state='ENROLLING';

-- name: AcceptFactorStep :execrows
UPDATE mfa_factors SET last_accepted_step=$2,updated_at=clock_timestamp()
WHERE id=$1 AND state='VERIFIED' AND (last_accepted_step IS NULL OR last_accepted_step<$2);

-- name: CompleteAdminOnboarding :execrows
UPDATE users SET onboarding_state='READY',updated_at=clock_timestamp()
WHERE id=$1 AND role='SUPERADMIN' AND onboarding_state='ENROLLMENT_REQUIRED';

-- name: RevokeUserSessions :exec
UPDATE device_sessions SET status='REVOKED',updated_at=clock_timestamp()
WHERE user_id=$1 AND status='ACTIVE';

-- name: InsertAdminSession :exec
INSERT INTO device_sessions(id,user_id,token_hash,generation,auth_version,binding_epoch,factor_version,mfa_verified_at,expires_at,status)
VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp(),$8,'ACTIVE');

-- name: SessionByHash :one
SELECT * FROM device_sessions WHERE token_hash=$1;

-- name: LockAdminSession :one
SELECT * FROM device_sessions WHERE id=$1 AND user_id=$2 FOR UPDATE;

-- name: AccountByID :one
SELECT * FROM users WHERE id=$1;

-- name: FactorByUser :one
SELECT * FROM mfa_factors WHERE user_id=$1 AND state='VERIFIED';

-- name: AccountWallet :one
SELECT available,frozen FROM credit_accounts WHERE user_id=$1;

-- name: TouchAdminSession :execrows
UPDATE device_sessions SET last_active_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND status='ACTIVE' AND expires_at>clock_timestamp()
 AND last_active_at>clock_timestamp()-interval '30 minutes';

-- name: InsertMFAChallenge :exec
INSERT INTO mfa_challenges(id,user_id,context_id,purpose,factor_version,expires_at)
VALUES($1,$2,$3,$4,$5,$6);

-- name: LockMFAChallenge :one
SELECT * FROM mfa_challenges WHERE id=$1 FOR UPDATE;

-- name: ConsumeMFAChallenge :execrows
UPDATE mfa_challenges SET used_at=clock_timestamp()
WHERE id=$1 AND used_at IS NULL AND expires_at>clock_timestamp();

-- name: InsertRecoveryCode :exec
INSERT INTO mfa_recovery_codes(id,factor_id,code_hash) VALUES($1,$2,$3);

-- name: LockRecoveryCode :one
SELECT * FROM mfa_recovery_codes WHERE factor_id=$1 AND code_hash=$2 FOR UPDATE;

-- name: ConsumeRecoveryCode :execrows
UPDATE mfa_recovery_codes SET used_at=clock_timestamp() WHERE id=$1 AND used_at IS NULL;

-- name: StepUpAdminSession :one
UPDATE device_sessions SET mfa_verified_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND status='ACTIVE' RETURNING mfa_verified_at;

-- name: CreateUserIdempotency :one
SELECT * FROM idempotency_records WHERE actor_id=$1 AND operation='CREATE_USER' AND key_hash=$2 FOR UPDATE;

-- name: InsertCreateUserIdempotency :exec
INSERT INTO idempotency_records(id,actor_id,operation,key_hash,request_hash,resource_id,result_json,status)
VALUES($1,$2,'CREATE_USER',$3,$4,$5,$6,'COMPLETED');

-- name: AccountAudit :exec
INSERT INTO audit_logs(id,actor_id,action,target_type,target_id,request_id,redacted_meta)
VALUES($1,$2,$3,'USER',$4,$5,sqlc.arg(redacted_meta)::jsonb);

-- name: ListManagedUsers :many
SELECT u.id,u.username,u.role,u.is_active,u.onboarding_state,u.binding_epoch,u.device_state,c.available,c.frozen
FROM users u JOIN credit_accounts c ON c.user_id=u.id
WHERE u.role='USER'
 AND (sqlc.arg(username_filter)::text='' OR strpos(u.username,sqlc.arg(username_filter)::text)>0)
 AND (sqlc.arg(status_filter)::text='' OR
      (sqlc.arg(status_filter)::text='ACTIVE' AND u.is_active) OR
      (sqlc.arg(status_filter)::text='INACTIVE' AND NOT u.is_active) OR
      u.onboarding_state=sqlc.arg(status_filter)::text)
 AND (sqlc.narg(cursor_id)::uuid IS NULL OR
      (u.created_at,u.id)<(SELECT created_at,id FROM users WHERE id=sqlc.narg(cursor_id)::uuid))
ORDER BY u.created_at DESC,u.id DESC LIMIT sqlc.arg(page_limit);

-- name: AuthRateAttempt :one
INSERT INTO auth_rate_limits(scope,key_hash,bucket,hits,expires_at)
VALUES(sqlc.arg(scope),sqlc.arg(key_hash),floor(extract(epoch FROM clock_timestamp())/sqlc.arg(window_seconds)::integer)::bigint,1,clock_timestamp()+interval '1 day')
ON CONFLICT(scope,key_hash,bucket) DO UPDATE SET hits=auth_rate_limits.hits+1
WHERE auth_rate_limits.hits<sqlc.arg(maximum)::integer
RETURNING hits;

-- name: CollectAuthRates :exec
DELETE FROM auth_rate_limits WHERE expires_at<clock_timestamp();

-- name: RevokeAdminSession :execrows
UPDATE device_sessions SET status='REVOKED',updated_at=clock_timestamp() WHERE id=$1 AND status='ACTIVE';

-- name: AdminAuditPage :many
SELECT a.id, COALESCE(u.username,'') AS actor, a.action, a.target_type, a.target_id, a.request_id, a.created_at
FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_id
WHERE (sqlc.arg(action_filter)::text = '' OR a.action = sqlc.arg(action_filter)::text)
  AND (sqlc.arg(cursor_created)::timestamptz IS NULL OR (a.created_at, a.id) < (sqlc.arg(cursor_created)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg(page_limit)::int;
