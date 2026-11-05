-- Formal generation: order creation, capacity planning, settlement, delivery
-- receipts and downloads. Every financial mutation keeps the ledger append-only
-- and guarded by expected-before wallet values.

-- name: GenerationClock :one
SELECT clock_timestamp()::timestamptz;

-- name: ConfirmPreviewManifest :one
SELECT * FROM preview_manifests
WHERE id = $1 AND owner_id = $2 AND document_id = $3 AND expires_at > now()
FOR UPDATE;

-- name: NextDocumentVersionNo :one
SELECT COALESCE(MAX(version_no), 0) + 1 AS next_no FROM document_versions
WHERE document_id = $1 AND owner_id = $2;

-- name: FindRendererManifest :one
SELECT * FROM renderer_manifests
WHERE image_digest = $1 AND asset_hashes = $2 AND algorithm_versions = $3
  AND locale = $4 AND page_settings = $5
LIMIT 1;

-- name: InsertRendererManifest :execrows
INSERT INTO renderer_manifests (id, image_digest, asset_hashes, algorithm_versions, locale, page_settings, archive_ref)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ExistingVersionByRevision :one
SELECT v.id AS version_id, v.version_no, v.manifest_id, v.total_cents,
       o.id AS order_id, o.billing_state, o.cost_points, o.deadline
FROM document_versions v
LEFT JOIN generation_orders o ON o.version_id = v.id
WHERE v.document_id = $1 AND v.owner_id = $2 AND v.source_revision = $3
FOR UPDATE OF v;

-- name: FormalLane :one
SELECT * FROM scheduler_lanes WHERE queue_name = 'formal' FOR UPDATE;

-- name: LaneBookedUntil :one
SELECT slot_end AS booked_until FROM capacity_tickets
WHERE lane_id = $1 AND status IN ('BOOKED', 'RUNNING')
ORDER BY slot_end DESC
LIMIT 1;

-- name: InsertDocumentVersion :exec
INSERT INTO document_versions (id, document_id, owner_id, version_no, source_revision,
  manifest_id, renderer_manifest_id, snapshot_json, unit_cents, qty_micros,
  subtotal_cents, tax_cents, total_cents)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: InsertGenerationOrder :exec
INSERT INTO generation_orders (id, version_id, owner_id, cost_points, billing_state, reserve_attempt, deadline)
VALUES ($1, $2, $3, $4, 'RESERVED', 1, $5);

-- name: InsertRenderJob :exec
INSERT INTO render_jobs (id, kind, order_id, version_id, state, reserve_attempt, attempt_no, next_attempt_at)
VALUES ($1, 'GENERATE', $2, $3, 'QUEUED', $4, 0, now());

-- name: InsertCapacityTicket :exec
INSERT INTO capacity_tickets (id, lane_id, owner_id, job_id, slot_start, slot_end, deadline, status, plan_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'BOOKED', 1);

-- name: InsertGenerationOutbox :exec
INSERT INTO outbox_events (id, event_type, aggregate_id, payload)
VALUES ($1, 'RENDER_GENERATE', $2, '{}'::jsonb);

-- name: InsertGenerationEvent :exec
INSERT INTO generation_events (id, order_id, kind, detail) VALUES ($1, $2, $3, $4);

-- name: FindGenerationIdempotency :one
SELECT id, request_hash, resource_id, result_json, status
FROM idempotency_records
WHERE actor_id = $1 AND operation = 'GENERATION' AND key_hash = $2
FOR UPDATE;

-- name: InsertGenerationIdempotency :execrows
INSERT INTO idempotency_records (id, actor_id, operation, key_hash, request_hash, status)
VALUES ($1, $2, 'GENERATION', $3, $4, 'PENDING');

-- name: CompleteGenerationIdempotency :execrows
UPDATE idempotency_records
SET resource_id = $2, result_json = $3, status = 'COMPLETED', updated_at = clock_timestamp()
WHERE id = $1 AND status = 'PENDING';

-- Wallet mutations return the post-state so responses never read stale values.
-- name: ReserveWalletPoints :one
UPDATE credit_accounts
SET available = available - $2, frozen = frozen + $3, updated_at = clock_timestamp()
WHERE user_id = $1 AND available = $4 AND frozen = $5
RETURNING available, frozen;

-- name: ReleaseWalletPoints :one
UPDATE credit_accounts
SET available = available + $2, frozen = frozen - $3, updated_at = clock_timestamp()
WHERE user_id = $1 AND available = $4 AND frozen = $5
RETURNING available, frozen;

-- name: ConsumeWalletPoints :one
UPDATE credit_accounts
SET frozen = frozen - $2, updated_at = clock_timestamp()
WHERE user_id = $1 AND frozen = $3
RETURNING available, frozen;

-- name: InsertLedgerEntry :execrows
INSERT INTO credit_entries (id, account_id, kind, delta_available, delta_frozen,
  before_available, after_available, before_frozen, after_frozen, operation_id, actor_id, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: GenerationForOwner :one
SELECT o.id, o.version_id, o.cost_points, o.billing_state, o.reserve_attempt, o.deadline, o.created_at,
       j.id AS job_id, j.state AS job_state, j.attempt_no,
       r.version_id AS receipt_id,
       w.available, w.frozen
FROM generation_orders o
LEFT JOIN render_jobs j ON j.order_id = o.id
LEFT JOIN delivery_receipts r ON r.order_id = o.id
LEFT JOIN credit_accounts w ON w.user_id = o.owner_id
WHERE o.id = $1 AND o.owner_id = $2;

-- name: DocumentVersionsForOwner :many
SELECT v.id, v.document_id, v.version_no, v.source_revision, v.created_at,
       v.subtotal_cents, v.tax_cents, v.total_cents,
       o.id AS order_id, o.billing_state, o.cost_points,
       r.version_id AS receipt_id,
       (SELECT count(*) FROM artifacts a JOIN artifact_objects ao ON ao.id = a.current_object_id
         WHERE a.version_id = v.id AND a.type IN ('CLEAN_PDF','CLEAN_PNG') AND ao.state = 'PUBLISHED') AS published_formats
FROM document_versions v
JOIN generation_orders o ON o.version_id = v.id
LEFT JOIN delivery_receipts r ON r.version_id = v.id
WHERE v.document_id = $1 AND v.owner_id = $2
ORDER BY v.version_no DESC;

-- Worker claim: only jobs whose planned slot has arrived, in plan order.
-- name: ClaimFormalJob :one
UPDATE render_jobs
SET state = 'RUNNING', attempt_no = attempt_no + 1, lease_token = $1,
    lease_until = now() + interval '90 seconds', updated_at = clock_timestamp()
WHERE id = (
  SELECT j.id FROM render_jobs j
  JOIN capacity_tickets t ON t.job_id = j.id AND t.status = 'BOOKED'
  JOIN scheduler_lanes l ON l.id = t.lane_id AND l.queue_name = 'formal' AND l.enabled
  WHERE j.kind = 'GENERATE' AND j.state IN ('QUEUED','RETRY_WAIT')
    AND j.next_attempt_at <= now() AND t.slot_start <= now()
  ORDER BY t.slot_start, j.id
  LIMIT 1 FOR UPDATE OF j SKIP LOCKED)
RETURNING *;

-- name: MarkTicketRunning :execrows
UPDATE capacity_tickets SET status = 'RUNNING', updated_at = clock_timestamp()
WHERE job_id = $1 AND status = 'BOOKED';

-- name: GenerationJobContext :one
SELECT j.id AS job_id, j.order_id, j.reserve_attempt, j.attempt_no, j.lease_token,
       v.id AS version_id, v.owner_id, v.snapshot_json,
       v.unit_cents, v.qty_micros, v.subtotal_cents, v.tax_cents, v.total_cents,
       m.canonical_bytes, m.content_hash, m.amounts AS manifest_amounts, m.profile_id,
       p.manifest AS profile_manifest, p.status AS profile_status
FROM render_jobs j
JOIN document_versions v ON v.id = j.version_id
JOIN preview_manifests m ON m.id = v.manifest_id
JOIN render_profiles p ON p.id = m.profile_id
WHERE j.id = $1;

-- name: SucceedRenderJob :execrows
UPDATE render_jobs SET state = 'SUCCEEDED', lease_token = NULL, lease_until = NULL, updated_at = clock_timestamp()
WHERE id = $1 AND state = 'RUNNING' AND lease_token = $2 AND attempt_no = $3;

-- name: CompleteGenerationOrder :execrows
UPDATE generation_orders SET billing_state = 'COMPLETED', updated_at = clock_timestamp()
WHERE id = $1 AND billing_state = 'RESERVED';

-- name: CompleteTicketByJob :execrows
UPDATE capacity_tickets SET status = 'COMPLETED', updated_at = clock_timestamp()
WHERE job_id = $1 AND status = 'RUNNING';

-- name: InsertFinalizeOutbox :exec
INSERT INTO outbox_events (id, event_type, aggregate_id, payload)
VALUES ($1, 'FINALIZE_DELIVERY', $2, '{}'::jsonb);

-- Failure paths. Defer schedules the next attempt; Fail is terminal.
-- name: DeferRenderJob :execrows
UPDATE render_jobs
SET state = 'RETRY_WAIT', lease_token = NULL, lease_until = NULL,
    next_attempt_at = now() + (sqlc.arg(defer_seconds)::int * interval '1 second'), updated_at = clock_timestamp()
WHERE id = $1 AND state = 'RUNNING' AND lease_token = $2 AND attempt_no = $3;

-- name: FailRenderJob :execrows
UPDATE render_jobs SET state = 'FAILED', lease_token = NULL, lease_until = NULL, updated_at = clock_timestamp()
WHERE id = $1 AND state = 'RUNNING' AND lease_token = $2 AND attempt_no = $3;

-- name: ReleaseGenerationOrder :execrows
UPDATE generation_orders SET billing_state = 'FAILED_RELEASED', updated_at = clock_timestamp()
WHERE id = $1 AND billing_state = 'RESERVED';

-- name: ReleaseTicketByJob :execrows
UPDATE capacity_tickets SET status = 'RELEASED', updated_at = clock_timestamp()
WHERE job_id = $1 AND status IN ('BOOKED','RUNNING');

-- Explicit retry of a released order: next reserve attempt, same immutable version.
-- name: RetryGenerationOrder :execrows
UPDATE generation_orders SET billing_state = 'RESERVED', reserve_attempt = $2, deadline = $3, updated_at = clock_timestamp()
WHERE id = $1 AND billing_state = 'FAILED_RELEASED' AND reserve_attempt = $2 - 1;

-- name: RequeueRenderJob :execrows
UPDATE render_jobs SET state = 'QUEUED', reserve_attempt = $2, attempt_no = 0,
    lease_token = NULL, lease_until = NULL, next_attempt_at = now(), updated_at = clock_timestamp()
WHERE id = $1 AND order_id = $3 AND reserve_attempt = $2 - 1;

-- name: RebookTicketForJob :execrows
UPDATE capacity_tickets SET slot_start = $2, slot_end = $3, deadline = $4,
    status = 'BOOKED', plan_version = plan_version + 1, updated_at = clock_timestamp()
WHERE job_id = $1 AND status = 'RELEASED';

-- Objects: insert UPLOADING, store bytes under a fresh key, mark READY.
-- name: InsertArtifactObject :execrows
INSERT INTO artifact_objects (id, owner_id, version_id, type, reserve_attempt, attempt_no, storage_key, state, protected_until)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'UPLOADING', $8);

-- name: InsertArtifactBlob :execrows
INSERT INTO artifact_blobs (storage_key, data) VALUES ($1, $2);

-- name: ReadyArtifactObject :execrows
UPDATE artifact_objects SET object_version = $2, sha256 = $3, size = $4, state = 'READY', updated_at = clock_timestamp()
WHERE id = $1 AND state = 'UPLOADING';

-- name: PublishArtifactObject :execrows
UPDATE artifact_objects SET state = 'PUBLISHED', updated_at = clock_timestamp()
WHERE id = $1 AND state = 'READY';

-- name: InsertArtifactReference :execrows
INSERT INTO artifacts (id, version_id, type, current_object_id) VALUES ($1, $2, $3, $4);

-- Finalizer: durability probe then receipt, both real WAL writes.
-- name: PendingFinalizeEvents :many
SELECT * FROM outbox_events
WHERE event_type = 'FINALIZE_DELIVERY' AND status = 'PENDING' AND next_attempt_at <= now()
ORDER BY created_at, id LIMIT 10 FOR UPDATE SKIP LOCKED;

-- name: UpsertDurabilityProbe :execrows
INSERT INTO durability_probes (order_id, probe_nonce, checked_at)
VALUES ($1, $2, now())
ON CONFLICT (order_id) DO UPDATE SET probe_nonce = EXCLUDED.probe_nonce, checked_at = EXCLUDED.checked_at;

-- name: InsertDeliveryReceipt :execrows
INSERT INTO delivery_receipts (version_id, order_id, content_hash)
VALUES ($1, $2, $3)
ON CONFLICT (version_id) DO NOTHING;

-- name: MarkOutboxDelivered :execrows
UPDATE outbox_events SET status = 'DELIVERED', attempts = attempts + 1, updated_at = clock_timestamp()
WHERE id = $1 AND status = 'PENDING';

-- name: DeferOutboxEvent :execrows
UPDATE outbox_events SET attempts = attempts + 1, next_attempt_at = now() + interval '5 seconds', updated_at = clock_timestamp()
WHERE id = $1 AND status = 'PENDING';

-- Downloads: entitlement then locked current object and a bounded read lease.
-- name: VersionDeliveryForOwner :one
SELECT v.id, v.document_id, v.version_no, v.total_cents, o.id AS order_id, o.billing_state, r.version_id AS receipt_id
FROM document_versions v
JOIN generation_orders o ON o.version_id = v.id
LEFT JOIN delivery_receipts r ON r.version_id = v.id
WHERE v.id = $1 AND v.owner_id = $2;

-- name: LockCurrentArtifact :one
SELECT o.id AS object_id, o.storage_key, o.sha256, o.size, o.state
FROM artifacts a JOIN artifact_objects o ON o.id = a.current_object_id
WHERE a.version_id = $1 AND a.type = $2
FOR UPDATE OF o;

-- name: InsertReadLease :execrows
INSERT INTO artifact_read_leases (id, object_id, lease_until)
VALUES ($1, $2, now() + interval '150 seconds');

-- name: ArtifactBlobByKey :one
SELECT data FROM artifact_blobs WHERE storage_key = $1;

-- Recovery: expired leases and past-deadline reservations.
-- name: StuckRunningJobs :many
SELECT j.id, j.order_id, j.attempt_no, j.lease_token, o.owner_id, o.cost_points, o.deadline, o.billing_state
FROM render_jobs j JOIN generation_orders o ON o.id = j.order_id
WHERE j.kind = 'GENERATE' AND j.state = 'RUNNING' AND j.lease_until < now();

-- name: ExpiredReserveOrders :many
SELECT o.id, o.owner_id, o.cost_points, j.state AS job_state, j.id AS job_id, j.attempt_no, j.lease_token
FROM generation_orders o LEFT JOIN render_jobs j ON j.order_id = o.id
WHERE o.billing_state = 'RESERVED' AND o.deadline < now();

-- name: GenerationWallet :one
SELECT available, frozen FROM credit_accounts WHERE user_id = $1 FOR UPDATE;

-- Finalizer context: the order's own settlement facts plus receipt inputs.
-- name: FinalizeOrderContext :one
SELECT o.id, o.billing_state, o.version_id, m.content_hash,
       (SELECT count(*) FROM artifacts a JOIN artifact_objects ao ON ao.id = a.current_object_id
        WHERE a.version_id = o.version_id AND a.type IN ('CLEAN_PDF','CLEAN_PNG') AND ao.state = 'PUBLISHED') AS published_formats
FROM generation_orders o
JOIN document_versions v ON v.id = o.version_id
JOIN preview_manifests m ON m.id = v.manifest_id
WHERE o.id = $1
FOR UPDATE OF o;

-- Terminal abandon of a job whose order deadline passed, whatever its live state.
-- name: AbandonJob :execrows
UPDATE render_jobs SET state = 'FAILED', lease_token = NULL, lease_until = NULL, updated_at = clock_timestamp()
WHERE id = $1 AND order_id = $2 AND state IN ('QUEUED','RETRY_WAIT','RUNNING');
