// Package generation owns the paid formal chain: it turns a confirmed preview
// manifest into an immutable version plus a reserved order, settles successful
// renders exactly once, and hands out downloads only for delivered versions.
// Credits move only through the append-only ledger inside the same
// transaction as the order state change.
package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/document"
	"invoice.local/system/backend/internal/money"
	"invoice.local/system/backend/internal/preview"
	"invoice.local/system/backend/internal/render"

	db "invoice.local/system/backend/internal/platform/postgres/db"
)

var (
	// ErrNotFound hides whether a resource exists for another owner.
	ErrNotFound = errors.New("generation not found")
	// ErrInvalid rejects malformed identifiers and parameters.
	ErrInvalid = errors.New("invalid generation request")
	// ErrUnauthorized reports a caller whose session no longer authorizes writes.
	ErrUnauthorized = errors.New("generation unauthorized")
	// ErrConflict reports an idempotency key bound to different parameters.
	ErrConflict = errors.New("idempotency key conflicts with another request")
	// ErrIntegrity reports a guarded write that affected an unexpected row count.
	ErrIntegrity = errors.New("generation data integrity failure")
	// ErrPending reports a version that is settled but not yet delivery-confirmed.
	ErrPending = errors.New("generation result pending delivery confirmation")
)

// ConflictError reports a client-resolvable state conflict; the identifiers let
// the client resume the original task instead of creating a second one.
type ConflictError struct {
	Code         string
	Current      int32
	GenerationID string
	VersionID    string
}

func (e *ConflictError) Error() string { return e.Code }

// Capacity planning constants of the single formal lane, per the design:
// 320 s per row budget, 300 s max first wait, 1800 s max reservation window.
const (
	slotBudget    = 320 * time.Second
	settleBudget  = 10 * time.Second
	maxQueueWait  = 300 * time.Second
	maxWindow     = 1800 * time.Second
	maxAttempts   = 3
	idleTTL       = 30 * time.Minute
	sessionTTLDur = 8 * time.Hour
)

// Limiter is the shared database-backed throttle used by the account service.
type Limiter interface {
	Allow(ctx context.Context, scope, key string, max, window int32) error
}

// Response is the wire DTO for one generation order, matching the closed
// contract states QUEUED/RUNNING/RETRY_WAIT, FINALIZING, COMPLETED, FAILED.
type Response struct {
	GenerationID    string     `json:"generation_id"`
	VersionID       string     `json:"version_id"`
	State           string     `json:"state"`
	BillingState    string     `json:"billing_state"`
	CostPoints      string     `json:"cost_points"`
	AvailablePoints string     `json:"available_points"`
	FrozenPoints    string     `json:"frozen_points"`
	DeliveryState   string     `json:"delivery_state"`
	DeadlineAt      *time.Time `json:"deadline_at,omitempty"`
	Downloads       *Downloads `json:"downloads,omitempty"`
}

// Downloads carries the signed-channel paths of a delivered version.
type Downloads struct {
	PDF string `json:"pdf"`
	PNG string `json:"png"`
}

// VersionSummary is one immutable version of the owner's document.
type VersionSummary struct {
	ID             string    `json:"id"`
	GenerationID   string    `json:"generation_id"`
	DocumentID     string    `json:"document_id"`
	VersionNo      int32     `json:"version_no"`
	SourceRevision int32     `json:"source_revision"`
	CreatedAt      time.Time `json:"created_at"`
	DeliveryState  string    `json:"delivery_state"`
	Amounts        Amounts   `json:"amounts"`
}

// Amounts mirrors the closed Amounts contract of drafts and versions.
type Amounts struct {
	Subtotal string `json:"subtotal"`
	TaxRate  string `json:"tax_rate"`
	Tax      string `json:"tax"`
	Total    string `json:"total"`
}

// Command is the validated create request built by the HTTP layer.
type Command struct {
	DocumentID       pgtype.UUID
	ExpectedRevision int32
	ManifestID       pgtype.UUID
	AcceptedCost     int64
	IdempotencyKey   pgtype.UUID
}

// Download is an authorized formal file with its delivery headers.
type Download struct {
	Data        []byte
	ContentType string
	Filename    string
}

// Service owns the formal queue, settlement and the download read path.
type Service struct {
	pool    *pgxpool.Pool
	limiter Limiter
	assets  *render.Assets
}

func NewService(pool *pgxpool.Pool, limiter Limiter, assets *render.Assets) (*Service, error) {
	if pool == nil || limiter == nil || assets == nil {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, limiter: limiter, assets: assets}, nil
}

func rollback(tx pgx.Tx) { _ = tx.Rollback(context.WithoutCancel(context.Background())) }

func (s *Service) transaction(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if e != nil {
		return nil, nil, e
	}
	if _, e = tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); e != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, e
	}
	return tx, db.New(tx), nil
}

func contentHash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func stamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func oneRow(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrIntegrity
	}
	return nil
}

// ownerState is the in-transaction recheck of the acting account.
type ownerState struct {
	role        string
	cost        int64
	wallet      db.LockCreditWalletRow
	sessionOK   bool
	adminFactor bool
}

// authorizeOwner re-verifies the session inside the write transaction: role,
// activation, onboarding, session identity and TTLs. Administrators must keep
// a VERIFIED factor; device users must keep their bound session generation.
func authorizeOwner(ctx context.Context, q *db.Queries, principal credit.Principal) (ownerState, error) {
	var out ownerState
	user, e := q.LockCreditUser(ctx, principal.ActorUserID)
	if e != nil {
		return out, ErrUnauthorized
	}
	if !user.IsActive || user.OnboardingState != "READY" || user.RecoveryState != "NORMAL" {
		return out, ErrUnauthorized
	}
	session, e := q.LockGrantSession(ctx, db.LockGrantSessionParams{ID: principal.SessionID, UserID: principal.ActorUserID})
	if e != nil {
		return out, ErrUnauthorized
	}
	now, e := q.CreditClock(ctx)
	if e != nil {
		return out, e
	}
	if session.Status != "ACTIVE" || session.AuthVersion != user.AuthVersion || session.BindingEpoch != user.BindingEpoch ||
		!session.Generation.Valid || session.Generation.Bytes != principal.Generation.Bytes ||
		!now.Time.Before(session.ExpiresAt.Time) || !now.Time.Before(session.LastActiveAt.Time.Add(idleTTL)) {
		return out, ErrUnauthorized
	}
	switch user.Role {
	case "USER":
		out.cost = 1
	case "SUPERADMIN":
		out.cost = 0
		factor, e := q.LockGrantFactor(ctx, user.ID)
		if e != nil || factor.State != "VERIFIED" || !session.FactorVersion.Valid || session.FactorVersion.Int32 != factor.FactorVersion {
			return out, ErrUnauthorized
		}
		if !session.MfaVerifiedAt.Valid || session.MfaVerifiedAt.Time.After(now.Time) {
			return out, ErrUnauthorized
		}
		out.adminFactor = true
	default:
		return out, ErrUnauthorized
	}
	out.role = user.Role
	out.sessionOK = true
	wallet, e := q.LockCreditWallet(ctx, user.ID)
	if e != nil {
		return out, ErrIntegrity
	}
	if wallet.Available < 0 || wallet.Frozen < 0 {
		return out, ErrIntegrity
	}
	out.wallet = wallet
	return out, nil
}

// prepare validates the command and derives the idempotency hashes.
func prepare(cmd Command) (Command, []byte, []byte, error) {
	if !cmd.DocumentID.Valid || !cmd.ManifestID.Valid || !cmd.IdempotencyKey.Valid ||
		cmd.ExpectedRevision < 1 || cmd.AcceptedCost < 0 || cmd.AcceptedCost > 1 {
		return cmd, nil, nil, ErrInvalid
	}
	key := sha256.Sum256([]byte(account.IDText(cmd.IdempotencyKey)))
	var input bytes.Buffer
	input.Write(cmd.DocumentID.Bytes[:])
	input.Write(cmd.ManifestID.Bytes[:])
	var number [8]byte
	putUint32 := func(v uint32) {
		number[4], number[5], number[6], number[7] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
		input.Write(number[4:8])
	}
	putUint32(uint32(cmd.ExpectedRevision))
	putUint32(uint32(cmd.AcceptedCost))
	hash := sha256.Sum256(input.Bytes())
	return cmd, key[:], hash[:], nil
}

// Create admits one formal generation. Replays with the same key return the
// live state of the original order; the same revision never bills twice.
func (s *Service) Create(ctx context.Context, principal credit.Principal, cmd Command) (Response, error) {
	cmd, keyHash, requestHash, e := prepare(cmd)
	if e != nil {
		return Response{}, e
	}
	if e = s.limiter.Allow(ctx, "generation-create", account.IDText(principal.ActorUserID), 20, 60); e != nil {
		return Response{}, e
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := s.createOnce(ctx, principal, cmd, keyHash, requestHash)
		if err == nil {
			return result, nil
		}
		if !retryable(err) || ctx.Err() != nil {
			return Response{}, err
		}
		lastErr = err
	}
	return Response{}, lastErr
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

func (s *Service) createOnce(ctx context.Context, principal credit.Principal, cmd Command, keyHash, requestHash []byte) (Response, error) {
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Response{}, e
	}
	defer rollback(tx)

	owner, e := authorizeOwner(ctx, q, principal)
	if e != nil {
		return Response{}, e
	}

	// Same-key replay: return the live state of the original order.
	previous, e := q.FindGenerationIdempotency(ctx, db.FindGenerationIdempotencyParams{ActorID: principal.ActorUserID, KeyHash: keyHash})
	switch {
	case e == nil:
		if !bytes.Equal(previous.RequestHash, requestHash) {
			return Response{}, ErrConflict
		}
		if !previous.ResourceID.Valid {
			return Response{}, ErrIntegrity
		}
		if e = tx.Commit(ctx); e != nil {
			return Response{}, e
		}
		return s.Status(ctx, principal.ActorUserID, previous.ResourceID)
	case !errors.Is(e, pgx.ErrNoRows):
		return Response{}, e
	}

	// Lock the draft and validate the confirmed preview against it.
	doc, e := q.LockDocumentForOwner(ctx, db.LockDocumentForOwnerParams{ID: cmd.DocumentID, OwnerID: principal.ActorUserID})
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, ErrNotFound
	}
	if e != nil {
		return Response{}, e
	}
	if doc.Revision != cmd.ExpectedRevision {
		return Response{}, &ConflictError{Code: "REVISION_CONFLICT", Current: doc.Revision}
	}
	profile, e := q.RenderProfileByID(ctx, doc.RenderProfileID)
	if e == nil && profile.Status != "ACTIVE" {
		return Response{}, &ConflictError{Code: "PROFILE_REVOKED"}
	}
	if e != nil {
		return Response{}, &ConflictError{Code: "PROFILE_REVOKED"}
	}
	manifest, e := q.ConfirmPreviewManifest(ctx, db.ConfirmPreviewManifestParams{ID: cmd.ManifestID, OwnerID: principal.ActorUserID, DocumentID: cmd.DocumentID})
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, &ConflictError{Code: "PREVIEW_STALE"}
	}
	if e != nil {
		return Response{}, e
	}
	if manifest.Revision != doc.Revision || manifest.ProfileID != doc.RenderProfileID ||
		manifest.ContentHash != contentHash(doc.DraftJson) {
		return Response{}, &ConflictError{Code: "PREVIEW_STALE"}
	}
	if cmd.AcceptedCost != owner.cost {
		return Response{}, &ConflictError{Code: "COST_CHANGED"}
	}

	// The accepted cost must match the acting role's fixed cost.
	existing, errExisting := q.ExistingVersionByRevision(ctx, db.ExistingVersionByRevisionParams{
		DocumentID: cmd.DocumentID, OwnerID: principal.ActorUserID, SourceRevision: doc.Revision})
	if errExisting != nil && !errors.Is(errExisting, pgx.ErrNoRows) {
		return Response{}, errExisting
	}
	if errExisting == nil {
		switch existing.BillingState.String {
		case "COMPLETED", "RESERVED":
			return s.resumeVersion(ctx, tx, q, principal, cmd, keyHash, requestHash, existing)
		case "FAILED_RELEASED":
			return s.retryReleased(ctx, tx, q, principal, cmd, keyHash, requestHash, existing, owner)
		default:
			return Response{}, ErrIntegrity
		}
	}

	// Capacity first: refuse before any reservation when the lane is full.
	lane, e := q.FormalLane(ctx)
	if e != nil {
		return Response{}, e
	}
	now := time.Now().UTC()
	firstStart, e := nextSlotStart(ctx, q, lane.ID, now)
	if e != nil {
		return Response{}, e
	}
	if firstStart.Sub(now) > maxQueueWait {
		return Response{}, &ConflictError{Code: "GENERATION_CAPACITY_EXCEEDED"}
	}
	slotEnd := firstStart.Add(slotBudget)
	deadline := slotEnd.Add(settleBudget)
	if slotEnd.Sub(now) > maxWindow {
		return Response{}, &ConflictError{Code: "GENERATION_CAPACITY_EXCEEDED"}
	}

	var available, frozen int64 = owner.wallet.Available, owner.wallet.Frozen
	operationID := account.NewID()
	if owner.cost == 1 {
		if owner.wallet.Available < 1 {
			return Response{}, &ConflictError{Code: "CREDITS_INSUFFICIENT"}
		}
		reserved, e := q.ReserveWalletPoints(ctx, db.ReserveWalletPointsParams{
			UserID: principal.ActorUserID, Available: 1, Frozen: 1, Available_2: owner.wallet.Available, Frozen_2: owner.wallet.Frozen})
		if e != nil {
			return Response{}, e
		}
		available, frozen = reserved.Available, reserved.Frozen
		if e = oneRow(q.InsertLedgerEntry(ctx, db.InsertLedgerEntryParams{
			ID: account.NewID(), AccountID: principal.ActorUserID, Kind: "RESERVE",
			DeltaAvailable: -1, DeltaFrozen: 1, BeforeAvailable: owner.wallet.Available, AfterAvailable: reserved.Available,
			BeforeFrozen: owner.wallet.Frozen, AfterFrozen: reserved.Frozen, OperationID: operationID,
			ActorID: principal.ActorUserID, Reason: "formal generation reserve"})); e != nil {
			return Response{}, e
		}
	}

	// Immutable version snapshot bound to the confirmed manifest.
	_, totals, e := invoiceFor(doc.DraftJson)
	if e != nil {
		return Response{}, &ConflictError{Code: "PREVIEW_STALE"}
	}
	if e = amountsAgree(manifest.Amounts, totals); e != nil {
		return Response{}, &ConflictError{Code: "PREVIEW_STALE"}
	}
	rendererID, e := s.rendererManifest(ctx, q, profile)
	if e != nil {
		return Response{}, e
	}
	nextNo, e := q.NextDocumentVersionNo(ctx, db.NextDocumentVersionNoParams{DocumentID: cmd.DocumentID, OwnerID: principal.ActorUserID})
	if e != nil {
		return Response{}, e
	}
	versionID := account.NewID()
	if e = q.InsertDocumentVersion(ctx, db.InsertDocumentVersionParams{
		ID: versionID, DocumentID: cmd.DocumentID, OwnerID: principal.ActorUserID, VersionNo: nextNo,
		SourceRevision: doc.Revision, ManifestID: manifest.ID, RendererManifestID: rendererID,
		SnapshotJson: manifest.CanonicalBytes, UnitCents: totals.UnitCents, QtyMicros: totals.QuantityMicros,
		SubtotalCents: totals.SubtotalCents, TaxCents: totals.TaxCents, TotalCents: totals.TotalCents,
	}); e != nil {
		return Response{}, e
	}
	orderID := account.NewID()
	if e = q.InsertGenerationOrder(ctx, db.InsertGenerationOrderParams{
		ID: orderID, VersionID: versionID, OwnerID: principal.ActorUserID, CostPoints: int16(owner.cost), Deadline: stamp(deadline)}); e != nil {
		return Response{}, e
	}
	jobID := account.NewID()
	if e = q.InsertRenderJob(ctx, db.InsertRenderJobParams{
		ID: jobID, OrderID: orderID, VersionID: versionID, ReserveAttempt: 1}); e != nil {
		return Response{}, e
	}
	if e = q.InsertCapacityTicket(ctx, db.InsertCapacityTicketParams{
		ID: account.NewID(), LaneID: lane.ID, OwnerID: principal.ActorUserID, JobID: jobID,
		SlotStart: stamp(firstStart), SlotEnd: stamp(slotEnd), Deadline: stamp(deadline)}); e != nil {
		return Response{}, e
	}
	if e = q.InsertGenerationOutbox(ctx, db.InsertGenerationOutboxParams{ID: account.NewID(), AggregateID: jobID}); e != nil {
		return Response{}, e
	}
	if e = q.InsertGenerationEvent(ctx, db.InsertGenerationEventParams{
		ID: account.NewID(), OrderID: orderID, Kind: "RESERVE", Detail: []byte(fmt.Sprintf(`{"cost":%d,"reserve_operation":%q}`, owner.cost, account.IDText(operationID)))}); e != nil {
		return Response{}, e
	}
	if owner.role == "SUPERADMIN" {
		if e = q.AccountAudit(ctx, db.AccountAuditParams{
			ID: account.NewID(), ActorID: principal.ActorUserID, Action: "GENERATION_CREATE", TargetID: principal.ActorUserID,
			RequestID: cmd.IdempotencyKey, RedactedMeta: []byte(`{"cost_points":0}`)}); e != nil {
			return Response{}, e
		}
	}

	result := Response{
		GenerationID: account.IDText(orderID), VersionID: account.IDText(versionID), State: "QUEUED",
		BillingState: "RESERVED", CostPoints: strconv.FormatInt(owner.cost, 10),
		AvailablePoints: strconv.FormatInt(available, 10), FrozenPoints: strconv.FormatInt(frozen, 10),
		DeliveryState: "PENDING", DeadlineAt: &deadline,
	}
	encoded, _ := json.Marshal(result)
	idemID := account.NewID()
	if e = oneRow(q.InsertGenerationIdempotency(ctx, db.InsertGenerationIdempotencyParams{
		ID: idemID, ActorID: principal.ActorUserID, KeyHash: keyHash, RequestHash: requestHash})); e != nil {
		return Response{}, e
	}
	if e = oneRow(q.CompleteGenerationIdempotency(ctx, db.CompleteGenerationIdempotencyParams{
		ID: idemID, ResourceID: orderID, ResultJson: encoded})); e != nil {
		return Response{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Response{}, e
	}
	return result, nil
}

// resumeVersion returns the live state of an existing version/order without
// writing a new one, recording the idempotency result for a fresh key.
func (s *Service) resumeVersion(ctx context.Context, tx pgx.Tx, q *db.Queries, principal credit.Principal,
	cmd Command, keyHash, requestHash []byte, existing db.ExistingVersionByRevisionRow) (Response, error) {
	if !existing.OrderID.Valid {
		return Response{}, ErrIntegrity
	}
	idemID := account.NewID()
	if e := oneRow(q.InsertGenerationIdempotency(ctx, db.InsertGenerationIdempotencyParams{
		ID: idemID, ActorID: principal.ActorUserID, KeyHash: keyHash, RequestHash: requestHash})); e != nil {
		return Response{}, e
	}
	if e := oneRow(q.CompleteGenerationIdempotency(ctx, db.CompleteGenerationIdempotencyParams{
		ID: idemID, ResourceID: existing.OrderID, ResultJson: []byte(`{}`)})); e != nil {
		return Response{}, e
	}
	if e := tx.Commit(ctx); e != nil {
		return Response{}, e
	}
	return s.Status(ctx, principal.ActorUserID, existing.OrderID)
}

// retryReleased re-reserves an explicitly retried failed order under a new key:
// next reserve attempt, same immutable snapshot, fresh capacity slot.
func (s *Service) retryReleased(ctx context.Context, tx pgx.Tx, q *db.Queries, principal credit.Principal,
	cmd Command, keyHash, requestHash []byte, existing db.ExistingVersionByRevisionRow, owner ownerState) (Response, error) {
	if !existing.OrderID.Valid {
		return Response{}, ErrIntegrity
	}
	lane, e := q.FormalLane(ctx)
	if e != nil {
		return Response{}, e
	}
	now := time.Now().UTC()
	firstStart, e := nextSlotStart(ctx, q, lane.ID, now)
	if e != nil {
		return Response{}, e
	}
	if firstStart.Sub(now) > maxQueueWait {
		return Response{}, &ConflictError{Code: "GENERATION_CAPACITY_EXCEEDED"}
	}
	slotEnd := firstStart.Add(slotBudget)
	deadline := slotEnd.Add(settleBudget)
	if slotEnd.Sub(now) > maxWindow {
		return Response{}, &ConflictError{Code: "GENERATION_CAPACITY_EXCEEDED"}
	}
	orderRow, e := q.GenerationForOwner(ctx, db.GenerationForOwnerParams{ID: existing.OrderID, OwnerID: principal.ActorUserID})
	if e != nil {
		return Response{}, e
	}
	attempt := orderRow.ReserveAttempt + 1

	var available, frozen int64 = owner.wallet.Available, owner.wallet.Frozen
	if owner.cost == 1 {
		if owner.wallet.Available < 1 {
			return Response{}, &ConflictError{Code: "CREDITS_INSUFFICIENT"}
		}
		reserved, e := q.ReserveWalletPoints(ctx, db.ReserveWalletPointsParams{
			UserID: principal.ActorUserID, Available: 1, Frozen: 1, Available_2: owner.wallet.Available, Frozen_2: owner.wallet.Frozen})
		if e != nil {
			return Response{}, e
		}
		available, frozen = reserved.Available, reserved.Frozen
		operationID := account.NewID()
		if e = oneRow(q.InsertLedgerEntry(ctx, db.InsertLedgerEntryParams{
			ID: account.NewID(), AccountID: principal.ActorUserID, Kind: "RESERVE",
			DeltaAvailable: -1, DeltaFrozen: 1, BeforeAvailable: owner.wallet.Available, AfterAvailable: reserved.Available,
			BeforeFrozen: owner.wallet.Frozen, AfterFrozen: reserved.Frozen, OperationID: operationID,
			ActorID: principal.ActorUserID, Reason: "formal generation retry reserve"})); e != nil {
			return Response{}, e
		}
		if e = q.InsertGenerationEvent(ctx, db.InsertGenerationEventParams{
			ID: account.NewID(), OrderID: existing.OrderID, Kind: "RETRY_RESERVE", Detail: []byte(fmt.Sprintf(`{"attempt":%d}`, attempt))}); e != nil {
			return Response{}, e
		}
	}
	if e = oneRow(q.RetryGenerationOrder(ctx, db.RetryGenerationOrderParams{
		ID: existing.OrderID, ReserveAttempt: attempt, Deadline: stamp(deadline)})); e != nil {
		return Response{}, e
	}
	if e = oneRow(q.RequeueRenderJob(ctx, db.RequeueRenderJobParams{
		ID: orderRow.JobID, ReserveAttempt: attempt, OrderID: existing.OrderID})); e != nil {
		return Response{}, e
	}
	if e = oneRow(q.RebookTicketForJob(ctx, db.RebookTicketForJobParams{
		JobID: orderRow.JobID, SlotStart: stamp(firstStart), SlotEnd: stamp(slotEnd), Deadline: stamp(deadline)})); e != nil {
		return Response{}, e
	}
	result := Response{
		GenerationID: account.IDText(existing.OrderID), VersionID: account.IDText(existing.VersionID), State: "QUEUED",
		BillingState: "RESERVED", CostPoints: strconv.FormatInt(owner.cost, 10),
		AvailablePoints: strconv.FormatInt(available, 10), FrozenPoints: strconv.FormatInt(frozen, 10),
		DeliveryState: "PENDING", DeadlineAt: &deadline,
	}
	encoded, _ := json.Marshal(result)
	idemID := account.NewID()
	if e = oneRow(q.InsertGenerationIdempotency(ctx, db.InsertGenerationIdempotencyParams{
		ID: idemID, ActorID: principal.ActorUserID, KeyHash: keyHash, RequestHash: requestHash})); e != nil {
		return Response{}, e
	}
	if e = oneRow(q.CompleteGenerationIdempotency(ctx, db.CompleteGenerationIdempotencyParams{
		ID: idemID, ResourceID: existing.OrderID, ResultJson: encoded})); e != nil {
		return Response{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Response{}, e
	}
	return result, nil
}

// nextSlotStart returns the earliest slot a new ticket may occupy: now, or the
// end of the latest active booking on the lane.
func nextSlotStart(ctx context.Context, q *db.Queries, laneID pgtype.UUID, now time.Time) (time.Time, error) {
	booked, e := q.LaneBookedUntil(ctx, laneID)
	if errors.Is(e, pgx.ErrNoRows) {
		return now, nil
	}
	if e != nil {
		return time.Time{}, e
	}
	if booked.Time.After(now) {
		return booked.Time, nil
	}
	return now, nil
}

// rendererManifest finds or records the exact renderer dependency set of one
// profile so history repair can later reproduce the pixels.
func (s *Service) rendererManifest(ctx context.Context, q *db.Queries, profile db.RenderProfile) (pgtype.UUID, error) {
	assetHashes, _ := json.Marshal(s.assets.AssetHashes())
	algorithmVersions, _ := json.Marshal(struct {
		Calculation string `json:"calculation"`
		Words       string `json:"amount_words"`
		Renderer    string `json:"renderer"`
	}{document.CalculationVersion, "es-MX-centavos-v1", s.assets.Template.RendererProtocol})
	pageSettings := []byte(`{"width_pt":"569.52","height_pt":"737.28","logical_width":1582,"logical_height":2048,"pt_per_pixel":"0.36"}`)
	digest := "local:" + s.assets.Template.RendererProtocol + ":" + profile.ManifestHash
	existing, e := q.FindRendererManifest(ctx, db.FindRendererManifestParams{
		ImageDigest: digest, AssetHashes: assetHashes, AlgorithmVersions: algorithmVersions, Locale: "es-MX", PageSettings: pageSettings})
	if e == nil {
		return existing.ID, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return pgtype.UUID{}, e
	}
	id := account.NewID()
	if e = oneRow(q.InsertRendererManifest(ctx, db.InsertRendererManifestParams{
		ID: id, ImageDigest: digest, AssetHashes: assetHashes, AlgorithmVersions: algorithmVersions,
		Locale: "es-MX", PageSettings: pageSettings, ArchiveRef: "embedded:invoice-mxn-v1"})); e != nil {
		return pgtype.UUID{}, e
	}
	return id, nil
}

// Status reports the live order, job and delivery state for the owner.
func (s *Service) Status(ctx context.Context, owner, generationID pgtype.UUID) (Response, error) {
	if !owner.Valid || !generationID.Valid {
		return Response{}, ErrInvalid
	}
	row, e := db.New(s.pool).GenerationForOwner(ctx, db.GenerationForOwnerParams{ID: generationID, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Response{}, ErrNotFound
	}
	if e != nil {
		return Response{}, e
	}
	return responseFor(row), nil
}

func responseFor(row db.GenerationForOwnerRow) Response {
	available, frozen := int64(0), int64(0)
	if row.Available.Valid {
		available = row.Available.Int64
	}
	if row.Frozen.Valid {
		frozen = row.Frozen.Int64
	}
	state, delivery := "QUEUED", "PENDING"
	var downloads *Downloads
	switch row.BillingState {
	case "FAILED_RELEASED":
		state = "FAILED"
	case "COMPLETED":
		if row.ReceiptID.Valid {
			state, delivery = "COMPLETED", "READY"
			base := "/api/v1/versions/" + account.IDText(row.VersionID) + "/download?format="
			downloads = &Downloads{PDF: base + "pdf", PNG: base + "png"}
		} else {
			state = "FINALIZING"
		}
	default:
		if row.JobState.Valid {
			switch row.JobState.String {
			case "RUNNING":
				state = "RUNNING"
			case "RETRY_WAIT":
				state = "RETRY_WAIT"
			}
		}
	}
	return Response{
		GenerationID: account.IDText(row.ID), VersionID: account.IDText(row.VersionID), State: state,
		BillingState: row.BillingState, CostPoints: strconv.FormatInt(int64(row.CostPoints), 10),
		AvailablePoints: strconv.FormatInt(available, 10), FrozenPoints: strconv.FormatInt(frozen, 10),
		DeliveryState: delivery, DeadlineAt: ptr(row.Deadline.Time.UTC()), Downloads: downloads,
	}
}

func ptr[T any](v T) *T { return &v }

// Versions lists the immutable versions of one owned document, newest first.
func (s *Service) Versions(ctx context.Context, owner, documentID pgtype.UUID) ([]VersionSummary, error) {
	if !owner.Valid || !documentID.Valid {
		return nil, ErrInvalid
	}
	rows, e := db.New(s.pool).DocumentVersionsForOwner(ctx, db.DocumentVersionsForOwnerParams{DocumentID: documentID, OwnerID: owner})
	if e != nil {
		return nil, e
	}
	items := make([]VersionSummary, 0, len(rows))
	for _, row := range rows {
		delivery := "PENDING"
		if row.ReceiptID.Valid {
			delivery = "READY"
		}
		items = append(items, VersionSummary{
			ID: account.IDText(row.ID), GenerationID: account.IDText(row.OrderID), DocumentID: account.IDText(row.DocumentID),
			VersionNo: row.VersionNo, SourceRevision: row.SourceRevision, CreatedAt: row.CreatedAt.Time.UTC(),
			DeliveryState: delivery, Amounts: centsAmounts(row.SubtotalCents, row.TaxCents, row.TotalCents),
		})
	}
	return items, nil
}

// centsAmounts renders stored integer cents in the contract's decimal form.
func centsAmounts(subtotal, tax, total int64) Amounts {
	return Amounts{Subtotal: cents(subtotal), TaxRate: "0.16", Tax: cents(tax), Total: cents(total)}
}

func cents(v int64) string {
	return strconv.FormatInt(v/100, 10) + "." + fmt.Sprintf("%02d", v%100)
}

// Download streams one formal format of a delivered version. Same-version
// repeats are free; balances are never consulted.
func (s *Service) Download(ctx context.Context, owner, versionID pgtype.UUID, format string) (Download, error) {
	if !owner.Valid || !versionID.Valid {
		return Download{}, ErrInvalid
	}
	var kind, contentType string
	switch format {
	case "pdf":
		kind, contentType = "CLEAN_PDF", "application/pdf"
	case "png":
		kind, contentType = "CLEAN_PNG", "image/png"
	default:
		return Download{}, ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Download{}, e
	}
	defer rollback(tx)
	row, e := q.VersionDeliveryForOwner(ctx, db.VersionDeliveryForOwnerParams{ID: versionID, OwnerID: owner})
	if errors.Is(e, pgx.ErrNoRows) {
		return Download{}, ErrNotFound
	}
	if e != nil {
		return Download{}, e
	}
	if row.BillingState != "COMPLETED" || !row.ReceiptID.Valid {
		return Download{}, ErrPending
	}
	object, e := q.LockCurrentArtifact(ctx, db.LockCurrentArtifactParams{VersionID: versionID, Type: kind})
	if errors.Is(e, pgx.ErrNoRows) {
		return Download{}, ErrNotFound
	}
	if e != nil {
		return Download{}, e
	}
	if object.State != "PUBLISHED" {
		return Download{}, ErrPending
	}
	if e = oneRow(q.InsertReadLease(ctx, db.InsertReadLeaseParams{ID: account.NewID(), ObjectID: object.ObjectID})); e != nil {
		return Download{}, e
	}
	blob, e := q.ArtifactBlobByKey(ctx, object.StorageKey)
	if e != nil {
		return Download{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Download{}, e
	}
	return Download{
		Data:        blob,
		ContentType: contentType,
		Filename:    "documento-v" + strconv.FormatInt(int64(row.VersionNo), 10) + "." + format,
	}, nil
}

// amountSnapshotIn mirrors the manifest amounts recorded at preview time.
type amountSnapshotIn struct {
	SubtotalCents  int64 `json:"subtotal_cents"`
	TaxCents       int64 `json:"tax_cents"`
	TotalCents     int64 `json:"total_cents"`
	UnitCents      int64 `json:"unit_cents"`
	QuantityMicros int64 `json:"quantity_micros"`
}

// amountsAgree refuses a manifest whose recorded amounts disagree with the
// recomputation from the snapshot: confirmation and billing must match.
func amountsAgree(raw []byte, totals money.Totals) error {
	var snap amountSnapshotIn
	if e := json.Unmarshal(raw, &snap); e != nil {
		return e
	}
	if snap.UnitCents != totals.UnitCents || snap.QuantityMicros != totals.QuantityMicros ||
		snap.SubtotalCents != totals.SubtotalCents || snap.TaxCents != totals.TaxCents || snap.TotalCents != totals.TotalCents {
		return ErrIntegrity
	}
	if totals.TotalCents < 0 || totals.TotalCents > 99999999999 || math.IsNaN(float64(totals.TotalCents)) {
		return ErrIntegrity
	}
	return nil
}

// invoiceFor maps the stored canonical snapshot onto the render input and
// recomputes amounts server-side; client numbers are never trusted.
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

// assetsMatch reuses the preview profile gate: the formal renderer must be the
// same compiled asset set the profile manifest describes.
func (s *Service) assetsMatch(manifest []byte) bool { return preview.AssetsMatch(s.assets, manifest) }
