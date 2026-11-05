-- name: LivePreviewByContent :one
SELECT * FROM preview_requests
WHERE owner_id=$1 AND document_id=$2 AND revision=$3 AND profile_id=$4 AND content_hash=$5
  AND state IN ('QUEUED','RUNNING','READY')
ORDER BY created_at DESC
LIMIT 1;

-- name: SupersedeQueuedPreviews :exec
UPDATE preview_requests SET state='EXPIRED', error_code='PREVIEW_STALE', updated_at=clock_timestamp()
WHERE owner_id=$1 AND state='QUEUED' AND id<>$2;

-- name: InsertPreviewRequest :exec
INSERT INTO preview_requests(id,owner_id,document_id,revision,profile_id,content_hash,state,expires_at)
VALUES($1,$2,$3,$4,$5,$6,'QUEUED',$7);

-- name: PreviewRequestForOwner :one
SELECT * FROM preview_requests WHERE id=$1 AND owner_id=$2;

-- name: LockPreviewRequestForOwner :one
SELECT * FROM preview_requests WHERE id=$1 AND owner_id=$2 FOR UPDATE;

-- name: ClaimQueuedPreview :one
UPDATE preview_requests SET state='RUNNING', updated_at=clock_timestamp()
WHERE id=(SELECT q.id FROM preview_requests q
          WHERE q.state='QUEUED' AND q.expires_at>now()
          ORDER BY q.created_at, q.id
          LIMIT 1
          FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: InsertPreviewManifest :exec
INSERT INTO preview_manifests(id,owner_id,document_id,revision,profile_id,canonical_bytes,content_hash,amounts,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: InsertPreviewObject :exec
INSERT INTO preview_objects(id,owner_id,request_id,jpeg,sha256) VALUES($1,$2,$3,$4,$5);

-- name: CompletePreview :execrows
UPDATE preview_requests SET state='READY', manifest_id=$3, error_code=NULL, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state='RUNNING';

-- name: FailPreview :execrows
UPDATE preview_requests SET state='FAILED', error_code=$2, updated_at=clock_timestamp()
WHERE id=$1 AND state='RUNNING';

-- name: ExpirePreview :execrows
UPDATE preview_requests SET state='EXPIRED', error_code='PREVIEW_STALE', updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state IN ('QUEUED','RUNNING','READY') AND expires_at<=now();

-- name: RetirePreviewObjects :exec
DELETE FROM preview_objects WHERE request_id=$1 AND owner_id=$2;

-- name: PreviewObjectForOwner :one
SELECT o.jpeg, o.sha256, r.state, r.expires_at
FROM preview_objects o
JOIN preview_requests r ON r.id=o.request_id AND r.owner_id=o.owner_id
WHERE o.request_id=$1 AND o.owner_id=$2;

-- name: ExpireStalePreviews :many
UPDATE preview_requests SET state='EXPIRED', error_code='PREVIEW_STALE', updated_at=clock_timestamp()
WHERE state IN ('QUEUED','RUNNING','READY') AND expires_at<=now()
RETURNING id, owner_id;

-- name: DeletePreviewObjectsByRequest :many
DELETE FROM preview_objects WHERE request_id = ANY(sqlc.arg(request_ids)::uuid[])
RETURNING id;

-- name: PreviewObjectCountForOwner :one
SELECT count(*) FROM preview_objects WHERE owner_id=$1;

-- name: OldestReadyPreviewForOwner :one
SELECT id FROM preview_requests
WHERE owner_id=$1 AND state='READY'
ORDER BY created_at, id
LIMIT 1;

-- name: ExpireReadyPreview :execrows
UPDATE preview_requests SET state='EXPIRED', error_code='PREVIEW_STALE', updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state='READY';

-- Draft deletion retires its free previews first: objects, then requests, then
-- confirmation manifests. No paid artifact can exist for a draft without a
-- version, so nothing user-visible is lost.
-- name: RetireDocumentPreviewObjects :exec
DELETE FROM preview_objects o USING preview_requests r
WHERE o.request_id=r.id AND o.owner_id=r.owner_id AND r.document_id=$1 AND r.owner_id=$2;

-- name: DeleteDocumentPreviewRequests :exec
DELETE FROM preview_requests WHERE document_id=$1 AND owner_id=$2;

-- name: DeleteDocumentPreviewManifests :exec
DELETE FROM preview_manifests WHERE document_id=$1 AND owner_id=$2;
