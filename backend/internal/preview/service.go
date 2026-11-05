// Package preview turns a saved draft revision into a watermarked JPEG. It
// owns the free-preview queue: requests merge by owner/revision/profile/
// content, one preview runs and one waits per owner, and the clean base page
// never leaves this package.
package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/money"
	"invoice.local/system/backend/internal/platform/postgres/db"
	"invoice.local/system/backend/internal/render"
)

var (
	// ErrNotFound hides whether a preview exists for another owner.
	ErrNotFound = errors.New("preview not found")
	// ErrInvalid rejects malformed identifiers and revisions.
	ErrInvalid = errors.New("invalid preview request")
	// ErrUnavailable reports that the deployment cannot render at all.
	ErrUnavailable = errors.New("renderer unavailable")
)

// ConflictError reports a state conflict the client must resolve explicitly.
type ConflictError struct {
	Code    string
	Current int32
}

func (e *ConflictError) Error() string { return e.Code }

// Limiter is the shared database-backed throttle used by the account service.
type Limiter interface {
	Allow(ctx context.Context, scope, key string, max, window int32) error
}

// Response is the wire DTO. Exactly one variant is populated, matching the
// closed PreviewPending / PreviewReady / PreviewFailed schemas.
type Response struct {
	ID                    string     `json:"id"`
	State                 string     `json:"state"`
	DraftRevision         int32      `json:"draft_revision"`
	PreviewManifestID     *string    `json:"preview_manifest_id,omitempty"`
	ConfirmationExpiresAt *time.Time `json:"confirmation_expires_at,omitempty"`
	ImagePath             *string    `json:"image_path,omitempty"`
	ErrorCode             *string    `json:"error_code,omitempty"`
}

// TTL bounds how long a READY preview can be confirmed and downloaded.
const TTL = 24 * time.Hour

// ObjectQuota is the per-owner preview object budget.
const ObjectQuota = 100

// Service owns the preview queue and the watermarked object read path.
type Service struct {
	pool    *pgxpool.Pool
	limiter Limiter
	assets  *render.Assets
	now     func() time.Time
}

func NewService(pool *pgxpool.Pool, limiter Limiter, assets *render.Assets) (*Service, error) {
	if pool == nil || limiter == nil || assets == nil {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, limiter: limiter, assets: assets, now: time.Now}, nil
}

func rollback(tx pgx.Tx) { _ = tx.Rollback(context.WithoutCancel(context.Background())) }

func (s *Service) transaction(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, nil, e
	}
	return tx, db.New(tx), nil
}

func contentHash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func stamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

// response renders the closed DTO for one request row. Manifest fields appear
// only for READY, and error codes only for FAILED/EXPIRED.
func response(row db.PreviewRequest) Response {
	out := Response{ID: account.IDText(row.ID), State: row.State, DraftRevision: row.Revision}
	switch row.State {
	case "READY":
		id := account.IDText(row.ManifestID)
		image := "/api/v1/previews/" + out.ID + "/image"
		expires := row.ExpiresAt.Time.UTC()
		out.PreviewManifestID, out.ImagePath, out.ConfirmationExpiresAt = &id, &image, &expires
	case "FAILED", "EXPIRED":
		code := "DEPENDENCY_UNAVAILABLE"
		if row.ErrorCode.Valid {
			code = row.ErrorCode.String
		} else if row.State == "EXPIRED" {
			code = "PREVIEW_STALE"
		}
		out.ErrorCode = &code
	}
	return out
}

// Create queues a preview for the current revision of an owned draft. Repeating
// the same content merges into the live request; a newer content replaces the
// waiting slot so only the latest pending preview survives.
func (s *Service) Create(ctx context.Context, owner, id pgtype.UUID, expectedRevision int32) (Response, error) {
	if !owner.Valid || !id.Valid || expectedRevision < 1 {
		return Response{}, ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "preview-create", account.IDText(owner), 20, 60); e != nil {
		return Response{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Response{}, e
	}
	defer rollback(tx)
	doc, e := q.LockDocumentForOwner(ctx, db.LockDocumentForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, ErrNotFound
	}
	if e != nil {
		return Response{}, e
	}
	if doc.Revision != expectedRevision {
		return Response{}, &ConflictError{Code: "REVISION_CONFLICT", Current: doc.Revision}
	}
	profile, e := q.RenderProfileByID(ctx, doc.RenderProfileID)
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, &ConflictError{Code: "PROFILE_REVOKED"}
	}
	if e != nil {
		return Response{}, e
	}
	if profile.Status != "ACTIVE" {
		return Response{}, &ConflictError{Code: "PROFILE_REVOKED"}
	}
	hash := contentHash(doc.DraftJson)
	live, e := q.LivePreviewByContent(ctx, db.LivePreviewByContentParams{
		OwnerID: owner, DocumentID: id, Revision: doc.Revision, ProfileID: doc.RenderProfileID, ContentHash: hash,
	})
	switch {
	case e == nil && live.ExpiresAt.Time.After(s.now()):
		// Same owner, revision, profile and content: merge into the live request.
		if e = tx.Commit(ctx); e != nil {
			return Response{}, e
		}
		return response(live), nil
	case e == nil:
		// Its TTL passed while it was still queued, running or ready: retire it
		// inside this transaction and queue a fresh request.
		if _, e = q.ExpirePreview(ctx, db.ExpirePreviewParams{ID: live.ID, OwnerID: owner}); e != nil {
			return Response{}, e
		}
		if e = q.RetirePreviewObjects(ctx, db.RetirePreviewObjectsParams{RequestID: live.ID, OwnerID: owner}); e != nil {
			return Response{}, e
		}
	case !errors.Is(e, pgx.ErrNoRows):
		return Response{}, e
	}
	if e = s.enforceQuota(ctx, q, owner); e != nil {
		return Response{}, e
	}
	previewID := account.NewID()
	if e = q.SupersedeQueuedPreviews(ctx, db.SupersedeQueuedPreviewsParams{OwnerID: owner, ID: previewID}); e != nil {
		return Response{}, e
	}
	expires := s.now().Add(TTL)
	if e = q.InsertPreviewRequest(ctx, db.InsertPreviewRequestParams{
		ID: previewID, OwnerID: owner, DocumentID: id, Revision: doc.Revision,
		ProfileID: doc.RenderProfileID, ContentHash: hash, ExpiresAt: stamp(expires),
	}); e != nil {
		return Response{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Response{}, e
	}
	return Response{ID: account.IDText(previewID), State: "QUEUED", DraftRevision: doc.Revision}, nil
}

// enforceQuota retires the oldest READY previews until one more object fits.
func (s *Service) enforceQuota(ctx context.Context, q *db.Queries, owner pgtype.UUID) error {
	for {
		count, e := q.PreviewObjectCountForOwner(ctx, owner)
		if e != nil {
			return e
		}
		if int(count) < ObjectQuota {
			return nil
		}
		oldest, e := q.OldestReadyPreviewForOwner(ctx, owner)
		if errors.Is(e, pgx.ErrNoRows) {
			// Only running objects remain; the new request cannot be admitted
			// without breaking the quota, which is a capacity refusal.
			return ErrUnavailable
		}
		if e != nil {
			return e
		}
		if _, e = q.ExpireReadyPreview(ctx, db.ExpireReadyPreviewParams{ID: oldest, OwnerID: owner}); e != nil {
			return e
		}
		if e = q.RetirePreviewObjects(ctx, db.RetirePreviewObjectsParams{RequestID: oldest, OwnerID: owner}); e != nil {
			return e
		}
	}
}

// Status returns the current state, expiring an owned preview whose TTL passed.
func (s *Service) Status(ctx context.Context, owner, id pgtype.UUID) (Response, error) {
	if !owner.Valid || !id.Valid {
		return Response{}, ErrInvalid
	}
	row, e := db.New(s.pool).PreviewRequestForOwner(ctx, db.PreviewRequestForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, ErrNotFound
	}
	if e != nil {
		return Response{}, e
	}
	if (row.State == "QUEUED" || row.State == "RUNNING" || row.State == "READY") && !row.ExpiresAt.Time.After(s.now()) {
		tx, q, e := s.transaction(ctx)
		if e != nil {
			return Response{}, e
		}
		defer rollback(tx)
		if _, e = q.ExpirePreview(ctx, db.ExpirePreviewParams{ID: id, OwnerID: owner}); e != nil {
			return Response{}, e
		}
		if e = q.RetirePreviewObjects(ctx, db.RetirePreviewObjectsParams{RequestID: id, OwnerID: owner}); e != nil {
			return Response{}, e
		}
		if e = tx.Commit(ctx); e != nil {
			return Response{}, e
		}
		row.State, row.ErrorCode = "EXPIRED", pgtype.Text{String: "PREVIEW_STALE", Valid: true}
	}
	return response(row), nil
}

// Image returns the watermarked JPEG for a READY, unexpired owned preview.
// Anything else is reported as not found so other owners learn nothing.
func (s *Service) Image(ctx context.Context, owner, id pgtype.UUID) ([]byte, error) {
	if !owner.Valid || !id.Valid {
		return nil, ErrInvalid
	}
	object, e := db.New(s.pool).PreviewObjectForOwner(ctx, db.PreviewObjectForOwnerParams{RequestID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	if object.State != "READY" || !object.ExpiresAt.Time.After(s.now()) {
		return nil, ErrNotFound
	}
	if len(object.Jpeg) == 0 || len(object.Jpeg) > 2<<20 {
		return nil, ErrNotFound
	}
	return object.Jpeg, nil
}

// RunOnce claims at most one queued preview and renders it. ok reports whether
// work was found; callers loop until ok is false, then sleep.
func (s *Service) RunOnce(ctx context.Context) (bool, error) {
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return false, e
	}
	defer rollback(tx)
	request, e := q.ClaimQueuedPreview(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	fail := func(code string) error {
		if _, e := q.FailPreview(ctx, db.FailPreviewParams{ID: request.ID, ErrorCode: pgtype.Text{String: code, Valid: true}}); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	profile, e := q.RenderProfileByID(ctx, request.ProfileID)
	if errors.Is(e, pgx.ErrNoRows) {
		return true, fail("PROFILE_REVOKED")
	}
	if e != nil {
		return true, e
	}
	if profile.Status != "ACTIVE" {
		return true, fail("PROFILE_REVOKED")
	}
	if !s.assetsMatch(profile.Manifest) {
		return true, fail("DEPENDENCY_UNAVAILABLE")
	}
	doc, e := q.DocumentForOwner(ctx, db.DocumentForOwnerParams{ID: request.DocumentID, OwnerID: request.OwnerID})
	if errors.Is(e, pgx.ErrNoRows) {
		return true, fail("PREVIEW_STALE")
	}
	if e != nil {
		return true, e
	}
	// The draft moved on after this request was queued: the snapshot is gone
	// and only the newest request may render.
	if doc.Revision != request.Revision || contentHash(doc.DraftJson) != request.ContentHash {
		return true, fail("PREVIEW_STALE")
	}
	invoice, totals, e := invoiceFor(doc.DraftJson)
	if e != nil {
		return true, fail("TEXT_DOES_NOT_FIT")
	}
	identity := account.IDText(request.OwnerID)
	if len(identity) > 8 {
		identity = identity[:8]
	}
	jpegBytes, e := s.assets.PreviewJPEG(invoice, identity)
	if errors.Is(e, render.ErrTextDoesNotFit) {
		return true, fail("TEXT_DOES_NOT_FIT")
	}
	if e != nil {
		return true, fail("DEPENDENCY_UNAVAILABLE")
	}
	manifestID := account.NewID()
	amounts, e := json.Marshal(amountSnapshot(totals))
	if e != nil {
		return true, fail("DEPENDENCY_UNAVAILABLE")
	}
	if e = q.InsertPreviewManifest(ctx, db.InsertPreviewManifestParams{
		ID: manifestID, OwnerID: request.OwnerID, DocumentID: request.DocumentID, Revision: request.Revision,
		ProfileID: request.ProfileID, CanonicalBytes: doc.DraftJson, ContentHash: request.ContentHash,
		Amounts: amounts, ExpiresAt: request.ExpiresAt,
	}); e != nil {
		return true, e
	}
	sum := sha256.Sum256(jpegBytes)
	if e = q.InsertPreviewObject(ctx, db.InsertPreviewObjectParams{
		ID: account.NewID(), OwnerID: request.OwnerID, RequestID: request.ID, Jpeg: jpegBytes, Sha256: hex.EncodeToString(sum[:]),
	}); e != nil {
		return true, e
	}
	n, e := q.CompletePreview(ctx, db.CompletePreviewParams{ID: request.ID, OwnerID: request.OwnerID, ManifestID: manifestID})
	if e != nil {
		return true, e
	}
	if n != 1 {
		return true, fail("PREVIEW_STALE")
	}
	return true, tx.Commit(ctx)
}

// Sweep retires every preview whose TTL passed. It never touches formal files.
func (s *Service) Sweep(ctx context.Context) error {
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	rows, e := q.ExpireStalePreviews(ctx)
	if e != nil {
		return e
	}
	if len(rows) > 0 {
		ids := make([]pgtype.UUID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if _, e = q.DeletePreviewObjectsByRequest(ctx, ids); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

type amountSnapshotOut struct {
	Subtotal           string `json:"subtotal"`
	Tax                string `json:"tax"`
	Total              string `json:"total"`
	TaxRate            string `json:"tax_rate"`
	UnitCents          int64  `json:"unit_cents"`
	QuantityMicros     int64  `json:"quantity_micros"`
	SubtotalCents      int64  `json:"subtotal_cents"`
	TaxCents           int64  `json:"tax_cents"`
	TotalCents         int64  `json:"total_cents"`
	CalculationVersion string `json:"calculation_version"`
}

func amountSnapshot(t money.Totals) amountSnapshotOut {
	return amountSnapshotOut{
		Subtotal: t.Subtotal, Tax: t.Tax, Total: t.Total, TaxRate: "0.16",
		UnitCents: t.UnitCents, QuantityMicros: t.QuantityMicros, SubtotalCents: t.SubtotalCents,
		TaxCents: t.TaxCents, TotalCents: t.TotalCents, CalculationVersion: document.CalculationVersion,
	}
}

// invoiceFor maps one canonical draft snapshot onto the render input. Amounts
// are recomputed from stored decimal strings, never taken from the client.
func invoiceFor(canonical []byte) (render.Invoice, money.Totals, error) {
	var fields document.Fields
	if e := json.Unmarshal(canonical, &fields); e != nil {
		return render.Invoice{}, money.Totals{}, e
	}
	totals, e := money.Calculate(fields.UnitPrice, fields.Quantity)
	if e != nil {
		return render.Invoice{}, money.Totals{}, e
	}
	invoice := render.Invoice{
		RecipientName: fields.RecipientName, ProductCode: fields.ProductCode,
		Quantity: fields.Quantity, UnitCode: fields.UnitCode, UnitDescription: fields.UnitDescription,
		Description: fields.Description, IssuedAt: fields.IssuedAt, UnitPrice: fields.UnitPrice,
		Subtotal: totals.Subtotal, Tax: totals.Tax, Total: totals.Total, TotalCents: totals.TotalCents,
	}
	if fields.CertificationDisplayAt != nil {
		invoice.CertificationDisplayAt = *fields.CertificationDisplayAt
	}
	return invoice, totals, nil
}

// assetsMatch checks the profile manifest against the compiled asset bytes and
// the compiled watermark style, so a stale row can never claim a renderer or a
// watermark it does not have.
func (s *Service) assetsMatch(manifest []byte) bool { return AssetsMatch(s.assets, manifest) }

// AssetsMatch is the shared profile/render compatibility gate: the manifest
// must describe exactly the compiled renderer protocol, asset bytes and
// watermark style before any job may render under it.
func AssetsMatch(assets *render.Assets, manifest []byte) bool {
	var parsed struct {
		RendererProtocol string `json:"renderer_protocol"`
		Assets           struct {
			Bundle string            `json:"bundle"`
			SHA256 map[string]string `json:"sha256"`
		} `json:"assets"`
		Watermark *render.WatermarkSpec `json:"watermark"`
	}
	if e := json.Unmarshal(manifest, &parsed); e != nil {
		return false
	}
	if parsed.RendererProtocol != assets.Template.RendererProtocol {
		return false
	}
	// A profile that describes a different watermark would produce an image the
	// manifest does not account for; refuse it rather than render blind.
	if parsed.Watermark == nil || *parsed.Watermark != render.CompiledWatermark() {
		return false
	}
	have := assets.AssetHashes()
	if len(parsed.Assets.SHA256) != len(have) {
		return false
	}
	for name, hash := range have {
		if parsed.Assets.SHA256[name] != hash {
			return false
		}
	}
	return true
}

// FieldBoxes exposes the ACTIVE template's field rectangles for preview-side
// overlays; callers must not use them to infer template content.
func (s *Service) FieldBoxes() map[string]render.Box { return s.assets.FieldBoxes() }
