package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/devicekey"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// PairingResult is what a candidate browser learns after proving its new key.
type PairingResult struct {
	PairingID  string    `json:"pairing_id"`
	Thumbprint string    `json:"key_thumbprint"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// ConfirmPairing verifies the PAIR proof of a candidate key and records the
// pairing as proven. Pairing never binds: only an administrator approval can
// turn it into a transfer.
func (s *Service) ConfirmPairing(ctx context.Context, preRaw, challengeID, publicJWK string, r *http.Request, body []byte) (PairingResult, error) {
	key, e := devicekey.ParsePublicJWK([]byte(publicJWK))
	if e != nil {
		return PairingResult{}, ErrInvalid
	}
	chID, e := ParseID(challengeID)
	if e != nil {
		return PairingResult{}, ErrInvalid
	}
	pre, e := s.pre(ctx, preRaw, "LOGIN")
	if e != nil {
		return PairingResult{}, ErrAuth
	}
	if e = s.Allow(ctx, "device-pairing", IDText(pre.UserID), 5, 60); e != nil {
		return PairingResult{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return PairingResult{}, e
	}
	defer rollbackAccount(tx)
	u, e := q.LockDeviceUser(ctx, pre.UserID)
	if e != nil || !liveDeviceUser(u) || u.Role != "USER" || u.OnboardingState != "READY" {
		return PairingResult{}, ErrAuth
	}
	pre, e = q.LockPreSession(ctx, pre.ID)
	if e != nil {
		return PairingResult{}, ErrAuth
	}
	ch, e := q.LockDeviceChallenge(ctx, db.LockDeviceChallengeParams{ID: chID, SubjectID: pre.ID, SubjectKind: "PRE_SESSION"})
	if e != nil {
		return PairingResult{}, ErrDeviceProof
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return PairingResult{}, e
	}
	if !liveDevicePre(pre, u, now.Time) || ch.Purpose != "PAIR" || ch.KeyHash != key.Thumbprint {
		return PairingResult{}, ErrAuth
	}
	if e = s.checkDeviceProof(r, body, ch, u, "PAIR", "PRE_SESSION", pre.ID, pre.ID, ParseIDLoose(ch.ID), key, now.Time); e != nil {
		return PairingResult{}, e
	}
	if n, e := q.ConsumeDeviceChallenge(ctx, ch.ID); e != nil || n != 1 {
		return PairingResult{}, ErrDeviceProof
	}
	// Bound the number of live candidates: the oldest proven pairings beyond
	// three are not deleted here; the 15-minute expiry retires them.
	pairID := NewID()
	expires := now.Time.Add(15 * time.Minute)
	if e = q.InsertVerifiedPairing(ctx, db.InsertVerifiedPairingParams{
		ID: pairID, UserID: u.ID, PublicJwk: []byte(publicJWK), KeyThumbprint: key.Thumbprint, ExpiresAt: stamp(expires)}); e != nil {
		return PairingResult{}, e
	}
	if e = deviceAudit(ctx, q, u.ID, "DEVICE_PAIRING_PROVEN", pairID); e != nil {
		return PairingResult{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return PairingResult{}, e
	}
	return PairingResult{PairingID: IDText(pairID), Thumbprint: key.Thumbprint, ExpiresAt: expires}, nil
}

// ParseIDLoose is the internal string form of an already valid row id.
func ParseIDLoose(id pgtype.UUID) pgtype.UUID { return id }

// bindTransferLive accepts the INITIAL activation transfer or an approved RESET
// transfer of the next epoch.
func bindTransferLive(u db.User, t db.BindingTransfer, p db.DevicePairing, now time.Time) bool {
	if !liveDeviceUser(u) || t.UserID != u.ID || t.Status != "PENDING" || !now.Before(t.ExpiresAt.Time) ||
		p.ID != t.PairingID || p.UserID != u.ID || !p.ProofVerified || p.KeyThumbprint != t.TargetKeyHash || !now.Before(p.ExpiresAt.Time) {
		return false
	}
	switch t.Purpose {
	case "INITIAL":
		return u.OnboardingState == "NEEDS_SETUP" && u.DeviceState == "INITIAL_PENDING" && t.TargetEpoch == u.BindingEpoch
	case "RESET":
		return u.OnboardingState == "READY" && u.DeviceState == "RESET_PENDING" && t.TargetEpoch == u.BindingEpoch
	}
	return false
}

// TransferApprovalCommand is the admin's designated-device approval input.
type TransferApprovalCommand struct {
	ExpectedEpoch   int32
	PairingID       pgtype.UUID
	TargetKeyHash   string
	VerificationRef string
	Reason          string
}

// ApproveDeviceReset atomically revokes the old device credentials and issues
// a 10-minute transfer bound to the specified proven pairing. Only that exact
// key can finish the bind; the old epoch dies with the approval.
func (s *Service) ApproveDeviceReset(ctx context.Context, principal credit.Principal, target pgtype.UUID, c TransferApprovalCommand, idemKey pgtype.UUID) (ResetApprovalResult, error) {
	if !target.Valid || !c.PairingID.Valid || !idemKey.Valid || c.ExpectedEpoch < 1 || c.TargetKeyHash == "" || c.VerificationRef == "" || c.Reason == "" {
		return ResetApprovalResult{}, ErrInvalid
	}
	key := sha256.Sum256([]byte(IDText(idemKey)))
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return ResetApprovalResult{}, e
	}
	defer rollbackAccount(tx)
	// Lock actor and target in UUID order, then re-verify the administrator's
	// recent MFA inside this transaction.
	if e = credit.RequireRecentAdministrator(ctx, q, principal); e != nil {
		return ResetApprovalResult{}, ErrAuth
	}
	previous, e := q.FindOperationByIdempotency(ctx, db.FindOperationByIdempotencyParams{ActorID: principal.ActorUserID, Operation: "DEVICE_RESET", KeyHash: key[:]})
	switch {
	case e == nil:
		if previous.Status != "COMPLETED" || !previous.ResourceID.Valid {
			return ResetApprovalResult{}, ErrConflict
		}
		if e = commitAccount(ctx, tx); e != nil {
			return ResetApprovalResult{}, e
		}
		return s.resetApprovalState(ctx, target, previous.ResourceID)
	case !errors.Is(e, pgx.ErrNoRows):
		return ResetApprovalResult{}, e
	}
	u, e := q.LockDeviceUser(ctx, target)
	if e != nil || u.Role != "USER" || !u.IsActive || u.OnboardingState != "READY" {
		return ResetApprovalResult{}, ErrAuth
	}
	if u.BindingEpoch != c.ExpectedEpoch || u.DeviceState != "ACTIVE" {
		return ResetApprovalResult{}, ErrConflict
	}
	if _, e = q.PendingTransferForUser(ctx, u.ID); !errors.Is(e, pgx.ErrNoRows) {
		return ResetApprovalResult{}, ErrConflict
	}
	pairing, e := q.LockDevicePairing(ctx, db.LockDevicePairingParams{ID: c.PairingID, UserID: u.ID})
	if e != nil || !pairing.ProofVerified || pairing.KeyThumbprint != c.TargetKeyHash {
		return ResetApprovalResult{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return ResetApprovalResult{}, e
	}
	if !now.Time.Before(pairing.ExpiresAt.Time) {
		return ResetApprovalResult{}, ErrAuth
	}
	if _, e = q.RevokeUserBindings(ctx, u.ID); e != nil {
		return ResetApprovalResult{}, e
	}
	if e = q.RevokeUserSessions(ctx, u.ID); e != nil {
		return ResetApprovalResult{}, e
	}
	if _, e = q.RevokeUserPreSessions(ctx, u.ID); e != nil {
		return ResetApprovalResult{}, e
	}
	if n, e := q.AdvanceBindingEpoch(ctx, db.AdvanceBindingEpochParams{ID: u.ID, BindingEpoch: u.BindingEpoch}); e != nil || n != 1 {
		return ResetApprovalResult{}, ErrConflict
	}
	transferID := NewID()
	expires := now.Time.Add(10 * time.Minute)
	if e = q.InsertResetTransfer(ctx, db.InsertResetTransferParams{
		ID: transferID, UserID: u.ID, PairingID: pairing.ID, TargetKeyHash: pairing.KeyThumbprint,
		TargetEpoch: u.BindingEpoch + 1, ApprovedBy: principal.ActorUserID, VerificationRef: pgtype.Text{String: c.VerificationRef, Valid: true}, ExpiresAt: stamp(expires)}); e != nil {
		return ResetApprovalResult{}, e
	}
	meta := []byte(`{"reason":"` + c.Reason + `","verification_ref":"` + c.VerificationRef + `"}`)
	if e = q.AdminAuditAction(ctx, db.AdminAuditActionParams{
		ID: NewID(), ActorID: principal.ActorUserID, Action: "DEVICE_RESET_APPROVED", TargetID: u.ID, RequestID: idemKey, RedactedMeta: meta}); e != nil {
		return ResetApprovalResult{}, e
	}
	if e = completeOperation(ctx, q, principal.ActorUserID, "DEVICE_RESET", key[:], transferID); e != nil {
		return ResetApprovalResult{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return ResetApprovalResult{}, e
	}
	return ResetApprovalResult{TransferID: IDText(transferID), TargetEpoch: u.BindingEpoch + 1, ExpiresAt: expires}, nil
}

func (s *Service) resetApprovalState(ctx context.Context, target, transfer pgtype.UUID) (ResetApprovalResult, error) {
	row, e := db.New(s.pool).PendingTransferForOwner(ctx, db.PendingTransferForOwnerParams{ID: transfer, UserID: target})
	if errors.Is(e, pgx.ErrNoRows) {
		return ResetApprovalResult{}, ErrConflict
	}
	if e != nil {
		return ResetApprovalResult{}, e
	}
	return ResetApprovalResult{TransferID: IDText(row.ID), TargetEpoch: row.TargetEpoch, ExpiresAt: row.ExpiresAt.Time.UTC()}, nil
}

// CancelTransfer revokes a pending transfer. The account stays RESET_PENDING
// until another explicit approval; a cancelled reset never reopens password
// activation.
func (s *Service) CancelTransfer(ctx context.Context, principal credit.Principal, target, transfer pgtype.UUID, expectedEpoch int32, reason string) error {
	if !target.Valid || !transfer.Valid || expectedEpoch < 1 || reason == "" {
		return ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireRecentAdministrator(ctx, q, principal); e != nil {
		return ErrAuth
	}
	u, e := q.LockDeviceUser(ctx, target)
	if e != nil || u.Role != "USER" {
		return ErrAuth
	}
	if u.BindingEpoch != expectedEpoch {
		return ErrConflict
	}
	if n, e := q.CancelBindingTransfer(ctx, db.CancelBindingTransferParams{ID: transfer, UserID: target}); e != nil || n != 1 {
		return ErrConflict
	}
	if e = q.AdminAuditAction(ctx, db.AdminAuditActionParams{
		ID: NewID(), ActorID: principal.ActorUserID, Action: "DEVICE_TRANSFER_CANCELLED", TargetID: target, RequestID: NewID(),
		RedactedMeta: []byte(`{"reason":"` + reason + `"}`)}); e != nil {
		return e
	}
	return commitAccount(ctx, tx)
}

// DeviceBindingView is the administrator's核验 view of one customer's device
// state: current binding, proven candidates and any pending transfer.
type DeviceBindingView struct {
	Epoch           int32            `json:"binding_epoch"`
	DeviceState     string           `json:"device_state"`
	Binding         *BindingSummary  `json:"binding"`
	PendingTransfer *TransferBrief   `json:"pending_transfer"`
	Candidates      []CandidateBrief `json:"candidates"`
}

type BindingSummary struct {
	ID         string `json:"id"`
	Thumbprint string `json:"key_thumbprint"`
	Epoch      int32  `json:"epoch"`
	Status     string `json:"status"`
}

type TransferBrief struct {
	ID            string    `json:"id"`
	TargetKeyHash string    `json:"target_key_hash"`
	TargetEpoch   int32     `json:"target_epoch"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type CandidateBrief struct {
	ID         string    `json:"pairing_id"`
	Thumbprint string    `json:"key_thumbprint"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s *Service) DeviceBindingView(ctx context.Context, principal credit.Principal, target pgtype.UUID) (DeviceBindingView, error) {
	if !target.Valid {
		return DeviceBindingView{}, ErrInvalid
	}
	q := db.New(s.pool)
	if e := credit.RequireAdministrator(ctx, q, principal); e != nil {
		return DeviceBindingView{}, ErrAuth
	}
	u, e := q.DeviceUserByID(ctx, target)
	if e != nil || u.Role != "USER" {
		return DeviceBindingView{}, ErrConflict
	}
	view := DeviceBindingView{Epoch: u.BindingEpoch, DeviceState: u.DeviceState, Candidates: []CandidateBrief{}}
	if binding, e := q.ActiveDeviceBinding(ctx, target); e == nil {
		view.Binding = &BindingSummary{ID: IDText(binding.ID), Thumbprint: binding.KeyThumbprint, Epoch: binding.Epoch, Status: binding.Status}
	}
	if transfer, e := q.PendingTransferForUserView(ctx, target); e == nil {
		view.PendingTransfer = &TransferBrief{ID: IDText(transfer.ID), TargetKeyHash: transfer.TargetKeyHash, TargetEpoch: transfer.TargetEpoch, ExpiresAt: transfer.ExpiresAt.Time.UTC()}
	}
	pairings, e := q.LivePairingsForUser(ctx, target)
	if e != nil {
		return DeviceBindingView{}, e
	}
	for _, pairing := range pairings {
		if pairing.ProofVerified {
			view.Candidates = append(view.Candidates, CandidateBrief{ID: IDText(pairing.ID), Thumbprint: pairing.KeyThumbprint, ExpiresAt: pairing.ExpiresAt.Time.UTC()})
		}
	}
	return view, nil
}

// SetUserStatus gates future access; disabling also revokes live sessions so
// old cookies stop working immediately. History is never removed.
func (s *Service) SetUserStatus(ctx context.Context, principal credit.Principal, target pgtype.UUID, active bool, reason string) error {
	if !target.Valid || reason == "" {
		return ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireRecentAdministrator(ctx, q, principal); e != nil {
		return ErrAuth
	}
	u, e := q.LockDeviceUser(ctx, target)
	if e != nil || u.Role != "USER" {
		return ErrAuth
	}
	status := "DISABLED"
	if active {
		status = "ENABLED"
	}
	if _, e = q.SetUserActive(ctx, db.SetUserActiveParams{ID: target, IsActive: active}); e != nil {
		return e
	}
	if !active {
		if e = q.RevokeUserSessions(ctx, target); e != nil {
			return e
		}
	}
	if e = q.AdminAuditAction(ctx, db.AdminAuditActionParams{
		ID: NewID(), ActorID: principal.ActorUserID, Action: "USER_STATUS_" + status, TargetID: target, RequestID: NewID(),
		RedactedMeta: []byte(`{"reason":"` + reason + `"}`)}); e != nil {
		return e
	}
	return commitAccount(ctx, tx)
}

// IssuePasswordReset creates a one-time PASSWORD_RESET link. It never touches
// the device binding: reset and transfer stay separate authorizations.
func (s *Service) IssuePasswordReset(ctx context.Context, principal credit.Principal, target pgtype.UUID, reason string) (ResetLinkResult, error) {
	if !target.Valid || reason == "" {
		return ResetLinkResult{}, ErrInvalid
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return ResetLinkResult{}, e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireRecentAdministrator(ctx, q, principal); e != nil {
		return ResetLinkResult{}, ErrAuth
	}
	u, e := q.LockDeviceUser(ctx, target)
	if e != nil || u.Role != "USER" || !u.IsActive {
		return ResetLinkResult{}, ErrAuth
	}
	token, e := NewToken()
	if e != nil {
		return ResetLinkResult{}, e
	}
	hash := token.Hash()
	link := NewID()
	now, e := q.AccountClock(ctx)
	if e != nil {
		return ResetLinkResult{}, e
	}
	expires := now.Time.Add(15 * time.Minute)
	if e = q.InsertPasswordToken(ctx, db.InsertPasswordTokenParams{
		ID: link, TokenHash: hash[:], UserID: target, Purpose: "PASSWORD_RESET", AuthVersion: u.AuthVersion, ExpiresAt: stamp(expires)}); e != nil {
		return ResetLinkResult{}, e
	}
	if e = q.AdminAuditAction(ctx, db.AdminAuditActionParams{
		ID: NewID(), ActorID: principal.ActorUserID, Action: "PASSWORD_RESET_ISSUED", TargetID: target, RequestID: link,
		RedactedMeta: []byte(`{"reason":"` + reason + `"}`)}); e != nil {
		return ResetLinkResult{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return ResetLinkResult{}, e
	}
	return ResetLinkResult{Link: "/reset?token=" + token.Reveal(), ExpiresAt: expires}, nil
}

type ResetApprovalResult struct {
	TransferID  string    `json:"transfer_id"`
	TargetEpoch int32     `json:"target_epoch"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type ResetLinkResult struct {
	Link      string    `json:"reset_link"`
	ExpiresAt time.Time `json:"expires_at"`
}

// completeOperation records an idempotent management operation result.
func completeOperation(ctx context.Context, q *db.Queries, actor pgtype.UUID, operation string, keyHash []byte, resource pgtype.UUID) error {
	id := NewID()
	if _, e := q.InsertOperationIdempotency(ctx, db.InsertOperationIdempotencyParams{
		ID: id, ActorID: actor, Operation: operation, KeyHash: keyHash}); e != nil {
		return e
	}
	_, e := q.CompleteOperationIdempotency(ctx, db.CompleteOperationIdempotencyParams{ID: id, ResourceID: resource})
	return e
}
