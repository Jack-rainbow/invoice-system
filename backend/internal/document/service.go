// Package document persists owner-scoped invoice drafts and recomputes their
// amounts from normalized fields. It never renders, bills, or returns a file.
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/money"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// CalculationVersion identifies the fixed server-side amount algorithm.
const CalculationVersion = "mxn-iva16-halfup-v1"

var (
	// ErrNotFound hides whether the document exists for another owner.
	ErrNotFound = errors.New("document not found")
	// ErrInvalid rejects malformed identifiers, cursors and filters.
	ErrInvalid = errors.New("invalid document request")
	// ErrUnavailable reports that the deployment has no usable render profile.
	ErrUnavailable = errors.New("no active render profile")
)

// ValidationError carries field-level codes without echoing submitted values.
type ValidationError struct{ Errors []FieldErrorCode }

type FieldErrorCode struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

func (e *ValidationError) Error() string { return "invalid draft fields" }

// ConflictError reports an optimistic-lock or entitlement conflict. Current is
// set only for revision conflicts.
type ConflictError struct {
	Code    string
	Current int32
}

func (e *ConflictError) Error() string { return e.Code }

// Amounts is the server-computed display breakdown for one draft revision.
type Amounts struct {
	Subtotal string `json:"subtotal"`
	TaxRate  string `json:"tax_rate"`
	Tax      string `json:"tax"`
	Total    string `json:"total"`
}

// Draft is the response DTO for a single owner-scoped draft revision.
type Draft struct {
	ID                 string  `json:"id"`
	Revision           int32   `json:"revision"`
	RenderProfileID    string  `json:"render_profile_id"`
	Fields             Fields  `json:"fields"`
	Amounts            Amounts `json:"amounts"`
	CalculationVersion string  `json:"calculation_version"`
}

// Summary is one item of the owner's document list.
type Summary struct {
	ID        string    `json:"id"`
	Revision  int32     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`
}

// Page returns items with an authenticated cursor bound to owner and filter.
type Page struct {
	Items      []Summary `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// Filter is the validated list request.
type Filter struct {
	Cursor string
	Limit  int32
	Status string
}

// Limiter is the shared database-backed throttle used by the account service.
type Limiter interface {
	Allow(ctx context.Context, scope, key string, max, window int32) error
}

// Service owns draft persistence. Callers must pass an authenticated owner.
type Service struct {
	pool    *pgxpool.Pool
	keys    *account.Keyring
	limiter Limiter
}

func NewService(pool *pgxpool.Pool, keys *account.Keyring, limiter Limiter) (*Service, error) {
	if pool == nil || keys == nil || limiter == nil {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, keys: keys, limiter: limiter}, nil
}

func rollback(tx pgx.Tx) { _ = tx.Rollback(context.WithoutCancel(context.Background())) }

func (s *Service) transaction(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, nil, e
	}
	return tx, db.New(tx), nil
}

func fieldCodes(input error) []FieldErrorCode {
	var f *FieldError
	if errors.As(input, &f) {
		code := "INVALID_FORMAT"
		switch f.Code {
		case CodeRequired:
			code = "REQUIRED"
		case CodeTooLong, CodeTooManyLines:
			code = "TEXT_DOES_NOT_FIT"
		case CodeAmountLimit:
			code = "OUT_OF_RANGE"
		}
		return []FieldErrorCode{{Field: f.Field, Code: code}}
	}
	return []FieldErrorCode{{Field: "fields", Code: "INVALID_FORMAT"}}
}

func canonical(f Fields) ([]byte, error) { return json.Marshal(f) }

func parseDraft(raw []byte) (Fields, money.Totals, error) {
	var stored Fields
	if e := json.Unmarshal(raw, &stored); e != nil {
		return Fields{}, money.Totals{}, ErrInvalid
	}
	totals, e := money.Calculate(stored.UnitPrice, stored.Quantity)
	if e != nil {
		return Fields{}, money.Totals{}, ErrInvalid
	}
	return stored, totals, nil
}

func draftFrom(rowID pgtype.UUID, revision int32, profileID pgtype.UUID, raw []byte) (Draft, error) {
	fields, totals, e := parseDraft(raw)
	if e != nil {
		return Draft{}, e
	}
	return Draft{
		ID: account.IDText(rowID), Revision: revision, RenderProfileID: account.IDText(profileID), Fields: fields,
		Amounts:            Amounts{Subtotal: totals.Subtotal, TaxRate: "0.16", Tax: totals.Tax, Total: totals.Total},
		CalculationVersion: CalculationVersion,
	}, nil
}

// Create stores revision 1 under the newest ACTIVE profile. Owner, profile and
// revision are always server-assigned.
func (s *Service) Create(ctx context.Context, owner pgtype.UUID, fields Fields) (Draft, error) {
	if !owner.Valid {
		return Draft{}, ErrInvalid
	}
	normalized, _, e := NormalizeFields(fields)
	if e != nil {
		return Draft{}, &ValidationError{Errors: fieldCodes(e)}
	}
	if e = s.limiter.Allow(ctx, "documents-write", account.IDText(owner), 60, 60); e != nil {
		return Draft{}, e
	}
	payload, e := canonical(normalized)
	if e != nil {
		return Draft{}, ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Draft{}, e
	}
	defer rollback(tx)
	profile, e := q.ActiveRenderProfile(ctx)
	if errors.Is(e, pgx.ErrNoRows) {
		return Draft{}, ErrUnavailable
	}
	if e != nil {
		return Draft{}, e
	}
	id := account.NewID()
	if e = q.InsertDocument(ctx, db.InsertDocumentParams{ID: id, OwnerID: owner, DraftJson: payload, RenderProfileID: profile.ID, Title: normalized.RecipientName}); e != nil {
		return Draft{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Draft{}, e
	}
	return draftFrom(id, 1, profile.ID, payload)
}

// Get returns one owned draft with amounts recomputed from stored fields.
func (s *Service) Get(ctx context.Context, owner, id pgtype.UUID) (Draft, error) {
	if !owner.Valid || !id.Valid {
		return Draft{}, ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "documents-read", account.IDText(owner), 240, 60); e != nil {
		return Draft{}, e
	}
	row, e := db.New(s.pool).DocumentForOwner(ctx, db.DocumentForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if e != nil {
		return Draft{}, e
	}
	return draftFrom(row.ID, row.Revision, row.RenderProfileID, row.DraftJson)
}

// Save replaces the complete field set under an optimistic revision check.
// Re-saving canonically identical fields returns the stored revision unchanged.
func (s *Service) Save(ctx context.Context, owner, id pgtype.UUID, expected int32, fields Fields) (Draft, error) {
	if !owner.Valid || !id.Valid || expected < 1 {
		return Draft{}, ErrInvalid
	}
	normalized, _, e := NormalizeFields(fields)
	if e != nil {
		return Draft{}, &ValidationError{Errors: fieldCodes(e)}
	}
	if e = s.limiter.Allow(ctx, "documents-write", account.IDText(owner), 60, 60); e != nil {
		return Draft{}, e
	}
	payload, e := canonical(normalized)
	if e != nil {
		return Draft{}, ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Draft{}, e
	}
	defer rollback(tx)
	row, e := q.LockDocumentForOwner(ctx, db.LockDocumentForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if e != nil {
		return Draft{}, e
	}
	if row.Revision != expected {
		return Draft{}, &ConflictError{Code: "REVISION_CONFLICT", Current: row.Revision}
	}
	stored, _, e := parseDraft(row.DraftJson)
	if e != nil {
		return Draft{}, e
	}
	existing, e := canonical(stored)
	if e != nil {
		return Draft{}, ErrInvalid
	}
	if string(existing) == string(payload) {
		if e = tx.Commit(ctx); e != nil {
			return Draft{}, e
		}
		return draftFrom(row.ID, row.Revision, row.RenderProfileID, row.DraftJson)
	}
	next := row.Revision + 1
	n, e := q.UpdateDocumentDraft(ctx, db.UpdateDocumentDraftParams{ID: id, OwnerID: owner, DraftJson: payload, Revision: next})
	if e != nil {
		return Draft{}, e
	}
	if n != 1 {
		return Draft{}, &ConflictError{Code: "REVISION_CONFLICT", Current: row.Revision}
	}
	if e = tx.Commit(ctx); e != nil {
		return Draft{}, e
	}
	return draftFrom(row.ID, next, row.RenderProfileID, payload)
}

// Delete removes a draft that has no immutable version. Entitlements are never
// cascaded away: a document with any version returns DOCUMENT_HAS_VERSIONS.
func (s *Service) Delete(ctx context.Context, owner, id pgtype.UUID, expected int32) error {
	if !owner.Valid || !id.Valid || expected < 1 {
		return ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "documents-delete", account.IDText(owner), 30, 60); e != nil {
		return e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	row, e := q.LockDocumentForOwner(ctx, db.LockDocumentForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if e != nil {
		return e
	}
	if row.Revision != expected {
		return &ConflictError{Code: "REVISION_CONFLICT", Current: row.Revision}
	}
	versions, e := q.DocumentVersionCount(ctx, db.DocumentVersionCountParams{DocumentID: id, OwnerID: owner})
	if e != nil {
		return e
	}
	if versions > 0 {
		return &ConflictError{Code: "DOCUMENT_HAS_VERSIONS"}
	}
	// Free previews are not history: retiring their objects, requests and
	// confirmation manifests keeps the draft deletable without orphaning rows
	// that a formal version could still reference.
	if e = q.RetireDocumentPreviewObjects(ctx, db.RetireDocumentPreviewObjectsParams{DocumentID: id, OwnerID: owner}); e != nil {
		return e
	}
	if e = q.DeleteDocumentPreviewRequests(ctx, db.DeleteDocumentPreviewRequestsParams{DocumentID: id, OwnerID: owner}); e != nil {
		return e
	}
	if e = q.DeleteDocumentPreviewManifests(ctx, db.DeleteDocumentPreviewManifestsParams{DocumentID: id, OwnerID: owner}); e != nil {
		return e
	}
	n, e := q.DeleteDocumentDraft(ctx, db.DeleteDocumentDraftParams{ID: id, OwnerID: owner, Revision: expected})
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

const cursorVersion = "DOCUMENTS_CURSOR/v1"

func (s *Service) cursorContext(owner pgtype.UUID, status string) string {
	digest := sha256.Sum256([]byte("v1\x00" + status))
	return cursorVersion + "/" + account.IDText(owner) + "/" + hex.EncodeToString(digest[:])
}

// List returns the owner's documents ordered by created_at,id descending with a
// sealed keyset cursor. Status is derived from immutable versions and live
// orders; nothing is denormalized on the draft row.
func (s *Service) List(ctx context.Context, owner pgtype.UUID, filter Filter) (Page, error) {
	if !owner.Valid {
		return Page{}, ErrInvalid
	}
	if filter.Limit == 0 {
		filter.Limit = 20
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return Page{}, ErrInvalid
	}
	switch filter.Status {
	case "", "DRAFT", "GENERATING", "HAS_VERSIONS":
	default:
		return Page{}, ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "documents-read", account.IDText(owner), 240, 60); e != nil {
		return Page{}, e
	}
	context := s.cursorContext(owner, filter.Status)
	q := db.New(s.pool)
	params := db.DocumentPageParams{OwnerID: owner, PageLimit: filter.Limit + 1}
	if filter.Status != "" {
		params.Status = pgtype.Text{String: filter.Status, Valid: true}
	}
	if filter.Cursor != "" {
		plain, e := s.keys.OpenCursor(context, filter.Cursor)
		if e != nil {
			return Page{}, ErrInvalid
		}
		created, id, ok := splitCursor(plain)
		if !ok {
			return Page{}, ErrInvalid
		}
		if _, e := q.DocumentForOwner(ctx, db.DocumentForOwnerParams{ID: id, OwnerID: owner}); e != nil {
			return Page{}, ErrInvalid
		}
		params.CursorCreated, params.CursorID = stampTime(created), id
	}
	rows, e := q.DocumentPage(ctx, params)
	if e != nil {
		return Page{}, e
	}
	result := Page{Items: make([]Summary, 0, len(rows))}
	if len(rows) > int(filter.Limit) {
		rows = rows[:filter.Limit]
		last := rows[len(rows)-1]
		sealed, e := s.keys.SealCursor(context, last.CreatedAt.Time.UTC().Format(time.RFC3339Nano)+"|"+account.IDText(last.ID))
		if e != nil {
			return Page{}, e
		}
		result.NextCursor = &sealed
	}
	for _, row := range rows {
		result.Items = append(result.Items, Summary{ID: account.IDText(row.ID), Revision: row.Revision, CreatedAt: row.CreatedAt.Time.UTC(), Status: row.Status})
	}
	return result, nil
}

func stampTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func splitCursor(plain string) (time.Time, pgtype.UUID, bool) {
	index := -1
	for i := 0; i < len(plain); i++ {
		if plain[i] == '|' {
			index = i
			break
		}
	}
	if index <= 0 {
		return time.Time{}, pgtype.UUID{}, false
	}
	created, e := time.Parse(time.RFC3339Nano, plain[:index])
	if e != nil {
		return time.Time{}, pgtype.UUID{}, false
	}
	id, e := account.ParseID(plain[index+1:])
	if e != nil || !id.Valid {
		return time.Time{}, pgtype.UUID{}, false
	}
	return created.UTC(), id, true
}

// ParseLimit parses the exact decimal limit query value.
func ParseLimit(raw string) (int32, error) {
	if raw == "" {
		return 0, nil
	}
	n, e := strconv.ParseInt(raw, 10, 32)
	if e != nil || n < 1 || n > 100 || strconv.FormatInt(n, 10) != raw {
		return 0, ErrInvalid
	}
	return int32(n), nil
}

// ProfileSummary is the selectable render profile metadata. Template bytes and
// storage keys are deliberately absent.
type ProfileSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Profiles lists the render profiles this deployment can currently render.
func (s *Service) Profiles(ctx context.Context, owner pgtype.UUID) ([]ProfileSummary, error) {
	if !owner.Valid {
		return nil, ErrInvalid
	}
	if e := s.limiter.Allow(ctx, "documents-read", account.IDText(owner), 240, 60); e != nil {
		return nil, e
	}
	rows, e := db.New(s.pool).ListActiveRenderProfiles(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]ProfileSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProfileSummary{ID: account.IDText(row.ID), Name: row.Name, Status: row.Status})
	}
	return out, nil
}

// ChangeProfile adopts another ACTIVE render profile as a new revision, which is
// the only way an existing draft may move off a revoked profile. Adopting the
// profile the draft already uses is a no-op and does not consume a revision.
func (s *Service) ChangeProfile(ctx context.Context, owner, id pgtype.UUID, expected int32, target string) (Draft, error) {
	if !owner.Valid || !id.Valid || expected < 1 {
		return Draft{}, ErrInvalid
	}
	profileID, e := account.ParseID(target)
	if e != nil || !profileID.Valid {
		return Draft{}, ErrInvalid
	}
	if e = s.limiter.Allow(ctx, "documents-write", account.IDText(owner), 60, 60); e != nil {
		return Draft{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Draft{}, e
	}
	defer rollback(tx)
	row, e := q.LockDocumentForOwner(ctx, db.LockDocumentForOwnerParams{ID: id, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if e != nil {
		return Draft{}, e
	}
	if row.Revision != expected {
		return Draft{}, &ConflictError{Code: "REVISION_CONFLICT", Current: row.Revision}
	}
	if row.RenderProfileID == profileID {
		if e = tx.Commit(ctx); e != nil {
			return Draft{}, e
		}
		return draftFrom(row.ID, row.Revision, row.RenderProfileID, row.DraftJson)
	}
	target_, e := q.RenderProfileByID(ctx, profileID)
	if errors.Is(e, pgx.ErrNoRows) {
		return Draft{}, ErrInvalid
	}
	if e != nil {
		return Draft{}, e
	}
	if target_.Status != "ACTIVE" {
		return Draft{}, &ConflictError{Code: "PROFILE_REVOKED"}
	}
	n, e := q.AdoptDocumentProfile(ctx, db.AdoptDocumentProfileParams{ID: id, OwnerID: owner, RenderProfileID: profileID, Revision: expected})
	if e != nil {
		return Draft{}, e
	}
	if n != 1 {
		return Draft{}, &ConflictError{Code: "REVISION_CONFLICT", Current: expected}
	}
	if e = tx.Commit(ctx); e != nil {
		return Draft{}, e
	}
	return draftFrom(row.ID, expected+1, profileID, row.DraftJson)
}
