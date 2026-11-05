package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/render"

	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// Attempt backoff seconds after the first and second failed attempts.
var attemptBackoff = []int32{5, 15}

// objectProtection bounds how long a published formal object is protected from
// GC even without read leases; purchased files are never GC-eligible while
// referenced, this only covers orphaned uploads.
const objectProtection = 30 * 24 * time.Hour

// RunOnce claims at most one formal job whose planned slot has arrived and
// renders, settles or releases it. ok reports whether work was found.
func (s *Service) RunOnce(ctx context.Context) (bool, error) {
	lease := account.NewID()
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return false, e
	}
	defer rollback(tx)
	job, e := q.ClaimFormalJob(ctx, lease)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if e = oneRow(q.MarkTicketRunning(ctx, job.ID)); e != nil {
		return true, e
	}
	if e = tx.Commit(ctx); e != nil {
		return true, e
	}
	// The snapshot, manifest and profile rows are immutable; reading them
	// outside the claim transaction is safe.
	c, e := db.New(s.pool).GenerationJobContext(ctx, job.ID)
	if e != nil {
		return true, e
	}
	invoice, renderErr := s.renderInput(c)
	if renderErr == nil {
		var png, pdf []byte
		png, renderErr = s.assets.CleanPNG(invoice)
		if renderErr == nil {
			pdf, renderErr = s.assets.CleanPDF(invoice)
		}
		if renderErr == nil {
			return true, s.settle(ctx, job, c, png, pdf)
		}
	}
	// Content that cannot fit the template will never fit: release now.
	if errors.Is(renderErr, render.ErrTextDoesNotFit) {
		return true, s.releaseOrder(ctx, c.OrderID, c.OwnerID, s.orderCost(ctx, c.OrderID, c.OwnerID), job.ID, "TEXT_DOES_NOT_FIT")
	}
	return true, s.failAttempt(ctx, job, c, renderErr)
}

var (
	errProfile  = errors.New("render profile unavailable")
	errSnapshot = errors.New("version snapshot unusable")
)

// renderInput validates the job's dependency set and maps the snapshot.
func (s *Service) renderInput(c db.GenerationJobContextRow) (render.Invoice, error) {
	if c.ProfileStatus != "ACTIVE" || !s.assetsMatch(c.ProfileManifest) {
		return render.Invoice{}, errProfile
	}
	invoice, totals, e := invoiceFor(c.SnapshotJson)
	if e != nil {
		return render.Invoice{}, errSnapshot
	}
	// The stored cents are the billing facts; a snapshot that disagrees with
	// its own amounts is an integrity failure, not a render input.
	if totals.UnitCents != c.UnitCents || totals.QuantityMicros != c.QtyMicros ||
		totals.SubtotalCents != c.SubtotalCents || totals.TaxCents != c.TaxCents || totals.TotalCents != c.TotalCents {
		return render.Invoice{}, errSnapshot
	}
	return invoice, nil
}

// settle publishes both formats and consumes the reservation in one
// transaction: exactly one terminal transition per order.
func (s *Service) settle(ctx context.Context, job db.RenderJob, c db.GenerationJobContextRow, png, pdf []byte) error {
	cost := int64(0)
	if row, e := db.New(s.pool).GenerationForOwner(ctx, db.GenerationForOwnerParams{ID: c.OrderID, OwnerID: c.OwnerID}); e == nil {
		cost = int64(row.CostPoints)
	} else {
		return e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	// A deactivated account no longer receives publications; the reservation
	// is compensated instead (design: 停用先提交则不发布并补偿).
	user, e := q.LockCreditUser(ctx, c.OwnerID)
	if e != nil {
		return e
	}
	if !user.IsActive {
		return s.releaseOrderTx(ctx, tx, q, c.OrderID, c.OwnerID, cost, job.ID, "ACCOUNT_DEACTIVATED")
	}
	if cost == 1 {
		wallet, e := q.LockCreditWallet(ctx, c.OwnerID)
		if e != nil {
			return e
		}
		consumed, e := q.ConsumeWalletPoints(ctx, db.ConsumeWalletPointsParams{UserID: c.OwnerID, Frozen: 1, Frozen_2: wallet.Frozen})
		if e != nil {
			return e
		}
		if e = oneRow(q.InsertLedgerEntry(ctx, db.InsertLedgerEntryParams{
			ID: account.NewID(), AccountID: c.OwnerID, Kind: "CONSUME",
			DeltaAvailable: 0, DeltaFrozen: -1, BeforeAvailable: wallet.Available, AfterAvailable: consumed.Available,
			BeforeFrozen: wallet.Frozen, AfterFrozen: consumed.Frozen, OperationID: c.OrderID,
			ActorID: c.OwnerID, Reason: "formal generation completed"})); e != nil {
			return e
		}
	}
	formats := []struct {
		kind string
		data []byte
	}{{"CLEAN_PNG", png}, {"CLEAN_PDF", pdf}}
	for _, f := range formats {
		storageKey := formalKey(c.VersionID, job.ReserveAttempt, job.AttemptNo)
		objectID := account.NewID()
		if e = oneRow(q.InsertArtifactObject(ctx, db.InsertArtifactObjectParams{
			ID: objectID, OwnerID: c.OwnerID, VersionID: c.VersionID, Type: f.kind,
			ReserveAttempt: job.ReserveAttempt, AttemptNo: job.AttemptNo, StorageKey: storageKey,
			ProtectedUntil: stamp(time.Now().Add(objectProtection))})); e != nil {
			return e
		}
		if e = oneRow(q.InsertArtifactBlob(ctx, db.InsertArtifactBlobParams{StorageKey: storageKey, Data: f.data})); e != nil {
			return e
		}
		sum := sha256.Sum256(f.data)
		if e = oneRow(q.ReadyArtifactObject(ctx, db.ReadyArtifactObjectParams{
			ID: objectID, ObjectVersion: pgtype.Text{String: account.IDText(account.NewID()), Valid: true},
			Sha256: pgtype.Text{String: hex.EncodeToString(sum[:]), Valid: true},
			Size:   pgtype.Int8{Int64: int64(len(f.data)), Valid: true}})); e != nil {
			return e
		}
		if e = oneRow(q.PublishArtifactObject(ctx, objectID)); e != nil {
			return e
		}
		if e = oneRow(q.InsertArtifactReference(ctx, db.InsertArtifactReferenceParams{
			ID: account.NewID(), VersionID: c.VersionID, Type: f.kind, CurrentObjectID: objectID})); e != nil {
			return e
		}
	}
	if e = oneRow(q.CompleteGenerationOrder(ctx, c.OrderID)); e != nil {
		return e
	}
	if e = oneRow(q.SucceedRenderJob(ctx, db.SucceedRenderJobParams{ID: job.ID, LeaseToken: job.LeaseToken, AttemptNo: job.AttemptNo})); e != nil {
		return e
	}
	if e = oneRow(q.CompleteTicketByJob(ctx, job.ID)); e != nil {
		return e
	}
	if e = q.InsertFinalizeOutbox(ctx, db.InsertFinalizeOutboxParams{ID: account.NewID(), AggregateID: c.OrderID}); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// failAttempt schedules a retry within the attempt budget or releases the
// reservation terminally when attempts are exhausted.
func (s *Service) failAttempt(ctx context.Context, job db.RenderJob, c db.GenerationJobContextRow, cause error) error {
	slog.Warn("formal render attempt failed", "job", account.IDText(job.ID), "attempt", job.AttemptNo, "error", cause)
	if int(job.AttemptNo) >= maxAttempts {
		return s.releaseOrder(ctx, c.OrderID, c.OwnerID, s.orderCost(ctx, c.OrderID, c.OwnerID), job.ID, "RENDER_FAILED")
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	backoff := attemptBackoff[0]
	if int(job.AttemptNo) < len(attemptBackoff) {
		backoff = attemptBackoff[job.AttemptNo-1]
	}
	if e = oneRow(q.DeferRenderJob(ctx, db.DeferRenderJobParams{
		ID: job.ID, LeaseToken: job.LeaseToken, AttemptNo: job.AttemptNo, DeferSeconds: backoff})); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// releaseOrder terminally releases one reservation: frozen points return,
// exactly one RELEASE entry, order FAILED_RELEASED, job FAILED, ticket freed.
func (s *Service) releaseOrder(ctx context.Context, orderID, ownerID pgtype.UUID, cost int64, jobID pgtype.UUID, reason string) error {
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	if e = s.releaseOrderTx(ctx, tx, q, orderID, ownerID, cost, jobID, reason); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Service) releaseOrderTx(ctx context.Context, tx pgx.Tx, q *db.Queries, orderID, ownerID pgtype.UUID, cost int64, jobID pgtype.UUID, reason string) error {
	if _, e := q.LockCreditUser(ctx, ownerID); e != nil {
		return e
	}
	if cost == 1 {
		wallet, e := q.LockCreditWallet(ctx, ownerID)
		if e != nil {
			return e
		}
		released, e := q.ReleaseWalletPoints(ctx, db.ReleaseWalletPointsParams{
			UserID: ownerID, Available: 1, Frozen: 1, Available_2: wallet.Available, Frozen_2: wallet.Frozen})
		if e != nil {
			return e
		}
		if e = oneRow(q.InsertLedgerEntry(ctx, db.InsertLedgerEntryParams{
			ID: account.NewID(), AccountID: ownerID, Kind: "RELEASE",
			DeltaAvailable: 1, DeltaFrozen: -1, BeforeAvailable: wallet.Available, AfterAvailable: released.Available,
			BeforeFrozen: wallet.Frozen, AfterFrozen: released.Frozen, OperationID: account.NewID(),
			ActorID: ownerID, Reason: "formal generation released: " + reason})); e != nil {
			return e
		}
	}
	// The guarded order transition is the single-winner gate: a concurrent
	// settlement that already COMPLETED this order makes this a no-op.
	n, e := q.ReleaseGenerationOrder(ctx, orderID)
	if e != nil {
		return e
	}
	if n != 1 {
		return nil
	}
	if jobID.Valid {
		if _, e = q.AbandonJob(ctx, db.AbandonJobParams{ID: jobID, OrderID: orderID}); e != nil {
			return e
		}
	}
	if _, e = q.ReleaseTicketByJob(ctx, jobID); e != nil {
		return e
	}
	return q.InsertGenerationEvent(ctx, db.InsertGenerationEventParams{
		ID: account.NewID(), OrderID: orderID, Kind: "RELEASE", Detail: []byte(`{"reason":"` + reason + `"}`)})
}

func (s *Service) orderCost(ctx context.Context, orderID, ownerID pgtype.UUID) int64 {
	row, e := db.New(s.pool).GenerationForOwner(ctx, db.GenerationForOwnerParams{ID: orderID, OwnerID: ownerID})
	if e != nil {
		return 1
	}
	return int64(row.CostPoints)
}

// RunFinalize drains FINALIZE_DELIVERY events: after a real durability probe
// write it records the delivery receipt that alone unlocks downloads.
func (s *Service) RunFinalize(ctx context.Context) error {
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	events, e := q.PendingFinalizeEvents(ctx)
	if e != nil {
		return e
	}
	for _, event := range events {
		c, e := q.FinalizeOrderContext(ctx, event.AggregateID)
		if errors.Is(e, pgx.ErrNoRows) {
			if e = oneRow(q.MarkOutboxDelivered(ctx, event.ID)); e != nil {
				return e
			}
			continue
		}
		if e != nil {
			return e
		}
		switch c.BillingState {
		case "FAILED_RELEASED":
			// Released orders never deliver; the event is done.
			if e = oneRow(q.MarkOutboxDelivered(ctx, event.ID)); e != nil {
				return e
			}
		case "COMPLETED":
			if c.PublishedFormats != 2 {
				if e = oneRow(q.DeferOutboxEvent(ctx, event.ID)); e != nil {
					return e
				}
				continue
			}
			if e = oneRow(q.UpsertDurabilityProbe(ctx, db.UpsertDurabilityProbeParams{
				OrderID: c.ID, ProbeNonce: account.NewID()})); e != nil {
				return e
			}
			if e = oneRow(q.InsertDeliveryReceipt(ctx, db.InsertDeliveryReceiptParams{
				VersionID: c.VersionID, OrderID: c.ID, ContentHash: c.ContentHash})); e != nil {
				return e
			}
			if e = oneRow(q.MarkOutboxDelivered(ctx, event.ID)); e != nil {
				return e
			}
		default:
			// Still reserved or running: recheck later.
			if e = oneRow(q.DeferOutboxEvent(ctx, event.ID)); e != nil {
				return e
			}
		}
	}
	return tx.Commit(ctx)
}

// Sweep recovers crashed workers and expired reservations: stuck RUNNING jobs
// requeue or release, past-deadline reservations always release.
func (s *Service) Sweep(ctx context.Context) error {
	stuck, e := db.New(s.pool).StuckRunningJobs(ctx)
	if e != nil {
		return e
	}
	for _, job := range stuck {
		if job.BillingState != "RESERVED" {
			continue
		}
		tx, q, e := s.transaction(ctx)
		if e != nil {
			return e
		}
		if int(job.AttemptNo) >= maxAttempts {
			if e = s.releaseOrderTx(ctx, tx, q, job.OrderID, job.OwnerID, int64(job.CostPoints), job.ID, "LEASE_EXPIRED"); e != nil {
				rollback(tx)
				return e
			}
		} else if e = oneRow(q.DeferRenderJob(ctx, db.DeferRenderJobParams{
			ID: job.ID, LeaseToken: job.LeaseToken, AttemptNo: job.AttemptNo, DeferSeconds: 1})); e != nil {
			rollback(tx)
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	expired, e := db.New(s.pool).ExpiredReserveOrders(ctx)
	if e != nil {
		return e
	}
	for _, order := range expired {
		if e = s.releaseOrder(ctx, order.ID, order.OwnerID, int64(order.CostPoints), order.JobID, "DEADLINE_EXCEEDED"); e != nil {
			return e
		}
	}
	return nil
}

// formalKey builds the server-owned object key: one unique key per attempt, so
// a late upload can never overwrite the purchased file of an earlier settle.
func formalKey(versionID pgtype.UUID, reserveAttempt, attemptNo int32) string {
	return "clean/" + account.IDText(versionID) + "/" + itoa64(int64(reserveAttempt)) + "/" + itoa64(int64(attemptNo)) + "/" + account.IDText(account.NewID())
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
