-- name: DeviceSetupTokenByHash :one
SELECT * FROM password_tokens WHERE token_hash=$1 AND purpose='INITIAL_SETUP';

-- name: LockDeviceUser :one
SELECT * FROM users WHERE id=$1 FOR UPDATE;

-- name: ActiveDeviceBinding :one
SELECT * FROM device_bindings WHERE user_id=$1 AND status='ACTIVE';

-- name: LockActiveDeviceBinding :one
SELECT * FROM device_bindings WHERE user_id=$1 AND status='ACTIVE' FOR UPDATE;

-- name: LockProofSession :one
SELECT * FROM device_sessions WHERE id=$1 AND user_id=$2 FOR UPDATE;

-- name: InsertDeviceChallenge :exec
INSERT INTO device_challenges(id,nonce_hash,purpose,subject_id,subject_kind,binding_transfer_id,key_hash,auth_version,epoch,generation,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11);

-- name: LockDeviceChallenge :one
SELECT * FROM device_challenges WHERE id=$1 AND subject_id=$2 AND subject_kind=$3 FOR UPDATE;

-- name: DeviceChallengeByNonce :one
SELECT * FROM device_challenges WHERE nonce_hash=$1;

-- name: ConsumeDeviceChallenge :execrows
UPDATE device_challenges SET used_at=clock_timestamp()
WHERE id=$1 AND used_at IS NULL AND expires_at>clock_timestamp();

-- name: InsertInitialPairing :exec
INSERT INTO device_pairings(id,user_id,public_jwk,key_thumbprint,proof_verified,expires_at)
VALUES($1,$2,$3,$4,true,$5);

-- name: InsertInitialTransfer :exec
INSERT INTO binding_transfers(id,user_id,pairing_id,target_key_hash,target_epoch,purpose,expires_at)
VALUES($1,$2,$3,$4,$5,'INITIAL',$6);

-- name: LockBindingTransfer :one
SELECT * FROM binding_transfers WHERE id=$1 AND user_id=$2 FOR UPDATE;

-- name: LockDevicePairing :one
SELECT * FROM device_pairings WHERE id=$1 AND user_id=$2 FOR UPDATE;

-- name: ConsumeInitialTransfer :execrows
UPDATE binding_transfers SET status='CONSUMED',updated_at=clock_timestamp()
WHERE id=$1 AND purpose='INITIAL' AND status='PENDING' AND expires_at>clock_timestamp();

-- name: InsertActiveDeviceBinding :exec
INSERT INTO device_bindings(id,user_id,public_jwk,key_thumbprint,epoch,status)
VALUES($1,$2,$3,$4,$5,'ACTIVE');

-- name: CompleteUserDeviceOnboarding :execrows
UPDATE users SET onboarding_state='READY',device_state='ACTIVE',updated_at=clock_timestamp()
WHERE id=$1 AND role='USER' AND onboarding_state='NEEDS_SETUP' AND device_state='INITIAL_PENDING';

-- name: InsertUserDeviceSession :exec
INSERT INTO device_sessions(id,user_id,token_hash,binding_id,generation,auth_version,binding_epoch,expires_at,status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE');

-- name: TouchProofSession :execrows
UPDATE device_sessions SET last_active_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND status='ACTIVE' AND expires_at>clock_timestamp() AND last_active_at>clock_timestamp()-interval '30 minutes';

-- name: InsertVerifiedPairing :exec
INSERT INTO device_pairings(id,user_id,public_jwk,key_thumbprint,proof_verified,expires_at)
VALUES($1,$2,$3,$4,true,$5);

-- name: LivePairingsForUser :many
SELECT id, public_jwk, key_thumbprint, proof_verified, expires_at, created_at FROM device_pairings
WHERE user_id=$1 AND expires_at>now() ORDER BY created_at DESC;

-- name: InsertResetTransfer :exec
INSERT INTO binding_transfers(id,user_id,pairing_id,target_key_hash,target_epoch,purpose,status,approved_by,verification_ref,expires_at)
VALUES($1,$2,$3,$4,$5,'RESET','PENDING',$6,$7,$8);

-- name: CancelBindingTransfer :execrows
UPDATE binding_transfers SET status='CANCELLED', updated_at=clock_timestamp()
WHERE id=$1 AND user_id=$2 AND status='PENDING';

-- name: PendingTransferForUser :one
SELECT * FROM binding_transfers WHERE user_id=$1 AND status='PENDING' FOR UPDATE;

-- name: RevokeUserBindings :execrows
UPDATE device_bindings SET status='REVOKED', updated_at=clock_timestamp() WHERE user_id=$1 AND status='ACTIVE';

-- name: RevokeUserPreSessions :execrows
UPDATE auth_pre_sessions SET used_at=clock_timestamp() WHERE user_id=$1 AND used_at IS NULL AND expires_at>now();

-- name: AdvanceBindingEpoch :execrows
UPDATE users SET binding_epoch=binding_epoch+1, auth_version=auth_version+1, device_state='RESET_PENDING', updated_at=clock_timestamp()
WHERE id=$1 AND binding_epoch=$2 AND device_state='ACTIVE' AND onboarding_state='READY';

-- name: CompleteResetBinding :execrows
UPDATE users SET binding_epoch=$2, device_state='ACTIVE', updated_at=clock_timestamp()
WHERE id=$1 AND device_state='RESET_PENDING' AND binding_epoch+1=$2;

-- name: SetUserActive :execrows
UPDATE users SET is_active=$2, updated_at=clock_timestamp() WHERE id=$1 AND is_active<>$2;

-- name: AdminAuditAction :exec
INSERT INTO audit_logs(id,actor_id,action,target_type,target_id,request_id,redacted_meta)
VALUES($1,$2,$3,'USER',$4,$5,$6);

-- name: PendingTransferForOwner :one
SELECT * FROM binding_transfers WHERE id=$1 AND user_id=$2;

-- name: PendingTransferForUserView :one
SELECT * FROM binding_transfers WHERE user_id=$1 AND status='PENDING';

-- name: DeviceUserByID :one
SELECT * FROM users WHERE id=$1;

-- name: InsertOperationIdempotency :execrows
INSERT INTO idempotency_records(id,actor_id,operation,key_hash,request_hash,status)
VALUES($1,$2,$3,$4,$5,'PENDING');

-- name: CompleteOperationIdempotency :execrows
UPDATE idempotency_records
SET resource_id=$2, result_json='{}'::jsonb, status='COMPLETED', updated_at=clock_timestamp()
WHERE id=$1 AND status='PENDING';
