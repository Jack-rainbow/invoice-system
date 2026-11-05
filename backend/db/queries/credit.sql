-- name: LockCreditUser :one
SELECT id, role, is_active, onboarding_state, auth_version, binding_epoch, recovery_state
FROM users WHERE id = $1 FOR UPDATE;

-- name: LockGrantSession :one
SELECT id, user_id, generation, auth_version, binding_epoch, factor_version,
       mfa_verified_at, last_active_at, expires_at, status
FROM device_sessions WHERE id = $1 AND user_id = $2 FOR UPDATE;

-- name: LockGrantFactor :one
SELECT id, state, factor_version
FROM mfa_factors WHERE user_id = $1 AND state = 'VERIFIED' FOR UPDATE;

-- name: LockCreditWallet :one
SELECT available, frozen FROM credit_accounts WHERE user_id = $1 FOR UPDATE;

-- name: CreditClock :one
SELECT clock_timestamp()::timestamptz;

-- name: FindGrantIdempotency :one
SELECT id, request_hash, resource_id, result_json, status
FROM idempotency_records
WHERE actor_id = $1 AND operation = 'GRANT' AND key_hash = $2
FOR UPDATE;

-- name: InsertGrantIdempotency :execrows
INSERT INTO idempotency_records (id, actor_id, operation, key_hash, request_hash, status)
VALUES ($1, $2, 'GRANT', $3, $4, 'PENDING');

-- name: AddGrantedPoints :execrows
UPDATE credit_accounts SET available = available + $2, updated_at = clock_timestamp()
WHERE user_id = $1 AND available = $3 AND frozen = $4;

-- name: InsertGrantEntry :execrows
INSERT INTO credit_entries (id, account_id, kind, delta_available, delta_frozen,
  before_available, after_available, before_frozen, after_frozen,
  operation_id, actor_id, reason)
VALUES ($1, $2, 'GRANT', $3, 0, $4, $5, $6, $6, $7, $8, $9);

-- name: InsertGrantAudit :execrows
INSERT INTO audit_logs (id, actor_id, action, target_type, target_id, request_id, redacted_meta)
VALUES ($1, $2, 'CREDIT_GRANT', 'USER', $3, $4, $5);

-- name: CompleteGrantIdempotency :execrows
UPDATE idempotency_records
SET resource_id = $2, result_json = $3, status = 'COMPLETED', updated_at = clock_timestamp()
WHERE id = $1 AND status = 'PENDING';

-- name: CreditEntriesPage :many
SELECT e.id, e.kind, e.delta_available, e.delta_frozen, e.after_available, e.after_frozen, e.operation_id, e.reason, e.created_at
FROM credit_entries e
WHERE e.account_id = $1
  AND (sqlc.arg(cursor_created)::timestamptz IS NULL OR (e.created_at, e.id) < (sqlc.arg(cursor_created)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: FindOperationByIdempotency :one
SELECT id, operation, request_hash, resource_id, result_json, status
FROM idempotency_records
WHERE actor_id = $1 AND operation = $2 AND key_hash = $3;

-- name: AdminCreditEntriesPage :many
SELECT e.id, u.username, e.kind, e.delta_available, e.delta_frozen, e.after_available, e.after_frozen, e.reason, e.created_at
FROM credit_entries e JOIN users u ON u.id = e.account_id
WHERE (sqlc.arg(user_filter)::uuid IS NULL OR e.account_id = sqlc.arg(user_filter)::uuid)
  AND (sqlc.arg(cursor_created)::timestamptz IS NULL OR (e.created_at, e.id) < (sqlc.arg(cursor_created)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(page_limit)::int;
