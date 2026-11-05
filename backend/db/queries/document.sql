-- name: ActiveRenderProfile :one
SELECT * FROM render_profiles WHERE status='ACTIVE' ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: RenderProfileByID :one
SELECT * FROM render_profiles WHERE id=$1;

-- name: InsertDocument :exec
INSERT INTO documents(id,owner_id,draft_json,revision,render_profile_id,title)
VALUES($1,$2,$3,1,$4,$5);

-- name: DocumentForOwner :one
SELECT * FROM documents WHERE id=$1 AND owner_id=$2;

-- name: LockDocumentForOwner :one
SELECT * FROM documents WHERE id=$1 AND owner_id=$2 FOR UPDATE;

-- name: UpdateDocumentDraft :execrows
UPDATE documents SET draft_json=$3, revision=$4, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND revision=$4-1;

-- name: DocumentVersionCount :one
SELECT count(*) FROM document_versions WHERE document_id=$1 AND owner_id=$2;

-- name: DeleteDocumentDraft :execrows
DELETE FROM documents d WHERE d.id=$1 AND d.owner_id=$2 AND d.revision=$3
 AND NOT EXISTS (SELECT 1 FROM document_versions v WHERE v.document_id=d.id AND v.owner_id=d.owner_id);

-- name: DocumentPage :many
SELECT s.id, s.revision, s.created_at, s.status FROM (
 SELECT d.id, d.revision, d.created_at,
  CASE
   WHEN EXISTS(SELECT 1 FROM generation_orders o JOIN document_versions v ON v.id=o.version_id
               WHERE v.document_id=d.id AND v.owner_id=d.owner_id AND o.billing_state='RESERVED') THEN 'GENERATING'
   WHEN EXISTS(SELECT 1 FROM document_versions v2 WHERE v2.document_id=d.id AND v2.owner_id=d.owner_id) THEN 'HAS_VERSIONS'
   ELSE 'DRAFT' END AS status
 FROM documents d
 WHERE d.owner_id=sqlc.arg(owner_id)
   AND (sqlc.narg(cursor_created)::timestamptz IS NULL
        OR (d.created_at, d.id) < (sqlc.narg(cursor_created)::timestamptz, sqlc.narg(cursor_id)::uuid))
) s
WHERE (sqlc.narg(status)::text IS NULL OR s.status=sqlc.narg(status)::text)
ORDER BY s.created_at DESC, s.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListActiveRenderProfiles :many
SELECT id, name, status FROM render_profiles WHERE status='ACTIVE' ORDER BY created_at DESC, id DESC;

-- name: AdoptDocumentProfile :execrows
UPDATE documents SET render_profile_id=sqlc.arg(render_profile_id), revision=(sqlc.arg(revision)::int)+1, updated_at=clock_timestamp()
WHERE id=sqlc.arg(id) AND owner_id=sqlc.arg(owner_id) AND revision=sqlc.arg(revision)::int;

-- name: InsertPreset :exec
INSERT INTO product_presets (id, owner_id, name, fields_json) VALUES ($1, $2, $3, $4);

-- name: PresetForOwner :one
SELECT * FROM product_presets WHERE id = $1 AND owner_id = $2;

-- name: LockPresetForOwner :one
SELECT * FROM product_presets WHERE id = $1 AND owner_id = $2 FOR UPDATE;

-- name: PresetPage :many
SELECT id, name, fields_json, created_at FROM product_presets
WHERE owner_id = $1
  AND (sqlc.arg(cursor_created)::timestamptz IS NULL OR (created_at, id) < (sqlc.arg(cursor_created)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: UpdatePresetFields :execrows
UPDATE product_presets SET name = $3, fields_json = $4, updated_at = clock_timestamp()
WHERE id = $1 AND owner_id = $2;

-- name: DeletePreset :execrows
DELETE FROM product_presets WHERE id = $1 AND owner_id = $2;

-- name: PresetCountForOwner :one
SELECT count(*) AS total FROM product_presets WHERE owner_id = $1;
