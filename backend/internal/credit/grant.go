package credit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "invoice.local/system/backend/internal/platform/postgres/db"
)

// CursorCodec seals and opens server-issued pagination cursors. The keyring
// implements it; the interface keeps this package free of an account import
// cycle.
type CursorCodec interface {
	SealCursor(context, plain string) (string, error)
	OpenCursor(context, sealed string) (string, error)
}

var (
	ErrInvalid        = errors.New("invalid credit grant")
	ErrUnauthorized   = errors.New("credit grant unauthorized")
	ErrConflict       = errors.New("idempotency key conflicts with another request")
	ErrOverflow       = errors.New("credit wallet overflow")
	ErrIntegrity      = errors.New("credit data integrity failure")
	ErrOutcomeUnknown = errors.New("credit grant outcome unknown; retry with the same idempotency key")
	ErrNotFound       = errors.New("credit grant result not found; an earlier commit may still complete")
)

const (
	maxReasonRunes = 1000
	idleTTL        = 30 * time.Minute
	mfaTTL         = 5 * time.Minute
	maxAttempts    = 3
)

// Principal must be built from a verified server-side session, never from a request body.
type Principal struct {
	ActorUserID pgtype.UUID
	SessionID   pgtype.UUID
	Generation  pgtype.UUID
}

type Command struct {
	TargetUserID   pgtype.UUID
	Points         int64
	Reason         string
	IdempotencyKey pgtype.UUID
}

type Wallet struct {
	Available string `json:"available"`
	Frozen    string `json:"frozen"`
}

type GrantResult struct {
	OperationID string `json:"operation_id"`
	Wallet      Wallet `json:"wallet"`
}

type Service struct {
	pool *pgxpool.Pool
	keys CursorCodec
}

// New wires the ledger service. The keyring seals list cursors; grant paths
// never depend on it and may pass a nil codec.
func New(pool *pgxpool.Pool, keys CursorCodec) *Service { return &Service{pool: pool, keys: keys} }

func (s *Service) Grant(ctx context.Context, principal Principal, command Command) (GrantResult, error) {
	command, keyHash, requestHash, err := prepare(principal, command)
	if err != nil {
		return GrantResult{}, err
	}
	operationID, err := newUUID()
	if err != nil {
		return GrantResult{}, err
	}
	idemID, err := newUUID()
	if err != nil {
		return GrantResult{}, err
	}
	entryID, err := newUUID()
	if err != nil {
		return GrantResult{}, err
	}
	auditID, err := newUUID()
	if err != nil {
		return GrantResult{}, err
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := s.grantOnce(ctx, principal, command, keyHash, requestHash, operationID, idemID, entryID, auditID)
		if err == nil || errors.Is(err, ErrOutcomeUnknown) {
			return result, err
		}
		if !retryable(err) || ctx.Err() != nil {
			return GrantResult{}, err
		}
		lastErr = err
	}
	return GrantResult{}, lastErr
}

func (s *Service) grantOnce(ctx context.Context, principal Principal, command Command, keyHash, requestHash []byte, operationID, idemID, entryID, auditID pgtype.UUID) (GrantResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return GrantResult{}, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
		return GrantResult{}, err
	}
	q := db.New(s.pool).WithTx(tx)
	_, target, wallet, err := authorize(ctx, q, principal, command.TargetUserID, true)
	if err != nil {
		return GrantResult{}, err
	}
	if target.RecoveryState == "RECOVERY_HOLD" {
		return GrantResult{}, ErrUnauthorized
	}
	previous, err := q.FindGrantIdempotency(ctx, db.FindGrantIdempotencyParams{ActorID: principal.ActorUserID, KeyHash: keyHash})
	if err == nil {
		if !bytes.Equal(previous.RequestHash, requestHash) {
			return GrantResult{}, ErrConflict
		}
		if previous.Status != "COMPLETED" {
			return GrantResult{}, ErrOutcomeUnknown
		}
		result, err := decodeResult(previous.ResultJson, previous.ResourceID)
		if err != nil {
			return GrantResult{}, err
		}
		if err = commit(tx, ctx); err != nil {
			return GrantResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GrantResult{}, err
	}
	if command.Points > math.MaxInt64-wallet.Available {
		return GrantResult{}, ErrOverflow
	}
	if err = oneRow(q.InsertGrantIdempotency(ctx, db.InsertGrantIdempotencyParams{ID: idemID, ActorID: principal.ActorUserID, KeyHash: keyHash, RequestHash: requestHash})); err != nil {
		return GrantResult{}, err
	}
	after := wallet.Available + command.Points
	if err = oneRow(q.AddGrantedPoints(ctx, db.AddGrantedPointsParams{UserID: command.TargetUserID, Available: command.Points, Available_2: wallet.Available, Frozen: wallet.Frozen})); err != nil {
		return GrantResult{}, err
	}
	if err = oneRow(q.InsertGrantEntry(ctx, db.InsertGrantEntryParams{ID: entryID, AccountID: command.TargetUserID, DeltaAvailable: command.Points, BeforeAvailable: wallet.Available, AfterAvailable: after, BeforeFrozen: wallet.Frozen, OperationID: operationID, ActorID: principal.ActorUserID, Reason: command.Reason})); err != nil {
		return GrantResult{}, err
	}
	meta, _ := json.Marshal(struct {
		Points string `json:"points"`
	}{Points: fmt.Sprint(command.Points)})
	if err = oneRow(q.InsertGrantAudit(ctx, db.InsertGrantAuditParams{ID: auditID, ActorID: principal.ActorUserID, TargetID: command.TargetUserID, RequestID: command.IdempotencyKey, RedactedMeta: meta})); err != nil {
		return GrantResult{}, err
	}
	result := GrantResult{OperationID: uuidText(operationID), Wallet: Wallet{Available: fmt.Sprint(after), Frozen: fmt.Sprint(wallet.Frozen)}}
	resultJSON, _ := json.Marshal(result)
	if err = oneRow(q.CompleteGrantIdempotency(ctx, db.CompleteGrantIdempotencyParams{ID: idemID, ResourceID: operationID, ResultJson: resultJSON})); err != nil {
		return GrantResult{}, err
	}
	if err = commit(tx, ctx); err != nil {
		return GrantResult{}, err
	}
	return result, nil
}

// Read checks current authorization and only reads the result of the same canonical request.
// Absence is not proof that an earlier ambiguous Commit cannot still complete.
func (s *Service) Read(ctx context.Context, principal Principal, command Command) (GrantResult, error) {
	command, keyHash, requestHash, err := prepare(principal, command)
	if err != nil {
		return GrantResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return GrantResult{}, err
	}
	defer rollback(tx)
	q := db.New(s.pool).WithTx(tx)
	_, target, _, err := authorize(ctx, q, principal, command.TargetUserID, true)
	if err != nil {
		return GrantResult{}, err
	}
	if target.RecoveryState == "RECOVERY_HOLD" {
		return GrantResult{}, ErrUnauthorized
	}
	previous, err := q.FindGrantIdempotency(ctx, db.FindGrantIdempotencyParams{ActorID: principal.ActorUserID, KeyHash: keyHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return GrantResult{}, ErrNotFound
	}
	if err != nil {
		return GrantResult{}, err
	}
	if !bytes.Equal(previous.RequestHash, requestHash) {
		return GrantResult{}, ErrConflict
	}
	if previous.Status != "COMPLETED" {
		return GrantResult{}, ErrOutcomeUnknown
	}
	result, err := decodeResult(previous.ResultJson, previous.ResourceID)
	if err != nil {
		return GrantResult{}, err
	}
	if err = commit(tx, ctx); err != nil {
		return GrantResult{}, err
	}
	return result, nil
}

func authorize(ctx context.Context, q *db.Queries, principal Principal, targetID pgtype.UUID, recent bool) (db.LockCreditUserRow, db.LockCreditUserRow, db.LockCreditWalletRow, error) {
	ids := []pgtype.UUID{principal.ActorUserID}
	if targetID.Bytes != principal.ActorUserID.Bytes {
		ids = append(ids, targetID)
	}
	if len(ids) == 2 && bytes.Compare(ids[0].Bytes[:], ids[1].Bytes[:]) > 0 {
		ids[0], ids[1] = ids[1], ids[0]
	}
	var actor, target db.LockCreditUserRow
	for _, id := range ids {
		row, err := q.LockCreditUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return actor, target, db.LockCreditWalletRow{}, ErrUnauthorized
		}
		if err != nil {
			return actor, target, db.LockCreditWalletRow{}, err
		}
		if id.Bytes == principal.ActorUserID.Bytes {
			actor = row
		}
		if id.Bytes == targetID.Bytes {
			target = row
		}
	}
	if actor.Role != "SUPERADMIN" || !actor.IsActive || actor.OnboardingState != "READY" || actor.RecoveryState != "NORMAL" || !target.IsActive || target.RecoveryState != "NORMAL" {
		return actor, target, db.LockCreditWalletRow{}, ErrUnauthorized
	}
	session, err := q.LockGrantSession(ctx, db.LockGrantSessionParams{ID: principal.SessionID, UserID: principal.ActorUserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, target, db.LockCreditWalletRow{}, ErrUnauthorized
	}
	if err != nil {
		return actor, target, db.LockCreditWalletRow{}, err
	}
	factor, err := q.LockGrantFactor(ctx, principal.ActorUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, target, db.LockCreditWalletRow{}, ErrUnauthorized
	}
	if err != nil {
		return actor, target, db.LockCreditWalletRow{}, err
	}
	wallet, err := q.LockCreditWallet(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return actor, target, wallet, ErrIntegrity
	}
	if err != nil {
		return actor, target, wallet, err
	}
	now, err := q.CreditClock(ctx)
	if err != nil {
		return actor, target, wallet, err
	}
	if !now.Valid || !session.Generation.Valid || session.Generation.Bytes != principal.Generation.Bytes ||
		session.Status != "ACTIVE" || session.AuthVersion != actor.AuthVersion || session.BindingEpoch != actor.BindingEpoch ||
		!session.FactorVersion.Valid || session.FactorVersion.Int32 != factor.FactorVersion || factor.State != "VERIFIED" ||
		!session.MfaVerifiedAt.Valid || !session.LastActiveAt.Valid || !session.ExpiresAt.Valid ||
		!now.Time.Before(session.ExpiresAt.Time) || !now.Time.Before(session.LastActiveAt.Time.Add(idleTTL)) ||
		now.Time.Before(session.MfaVerifiedAt.Time) || (recent && now.Time.Sub(session.MfaVerifiedAt.Time) > mfaTTL) {
		return actor, target, wallet, ErrUnauthorized
	}
	if wallet.Available < 0 || wallet.Frozen < 0 {
		return actor, target, wallet, ErrIntegrity
	}
	return actor, target, wallet, nil
}

func prepare(principal Principal, command Command) (Command, []byte, []byte, error) {
	if !validUUID(principal.ActorUserID) || !validUUID(principal.SessionID) || !validUUID(principal.Generation) || !validUUID(command.TargetUserID) || !validUUID(command.IdempotencyKey) || command.Points <= 0 {
		return command, nil, nil, ErrInvalid
	}
	command.Reason = strings.TrimSpace(command.Reason)
	if !utf8.ValidString(command.Reason) || utf8.RuneCountInString(command.Reason) == 0 || utf8.RuneCountInString(command.Reason) > maxReasonRunes {
		return command, nil, nil, ErrInvalid
	}
	for _, r := range command.Reason {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return command, nil, nil, ErrInvalid
		}
	}
	key := sha256.Sum256([]byte(uuidText(command.IdempotencyKey)))
	var input bytes.Buffer
	input.Write(command.TargetUserID.Bytes[:])
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(command.Points))
	input.Write(number[:])
	binary.BigEndian.PutUint64(number[:], uint64(len(command.Reason)))
	input.Write(number[:])
	input.WriteString(command.Reason)
	hash := sha256.Sum256(input.Bytes())
	return command, key[:], hash[:], nil
}

func decodeResult(raw []byte, operationID pgtype.UUID) (GrantResult, error) {
	var result GrantResult
	if !validUUID(operationID) || json.Unmarshal(raw, &result) != nil || result.OperationID != uuidText(operationID) || result.Wallet.Available == "" || result.Wallet.Frozen == "" {
		return GrantResult{}, ErrIntegrity
	}
	return result, nil
}

func validUUID(id pgtype.UUID) bool {
	if !id.Valid {
		return false
	}
	for _, b := range id.Bytes {
		if b != 0 {
			return true
		}
	}
	return false
}

func uuidText(id pgtype.UUID) string {
	hexBytes := make([]byte, 32)
	hex.Encode(hexBytes, id.Bytes[:])
	return string(hexBytes[:8]) + "-" + string(hexBytes[8:12]) + "-" + string(hexBytes[12:16]) + "-" + string(hexBytes[16:20]) + "-" + string(hexBytes[20:])
}

func newUUID() (pgtype.UUID, error) {
	var id pgtype.UUID
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		return pgtype.UUID{}, err
	}
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func commit(tx pgx.Tx, ctx context.Context) error {
	err := tx.Commit(ctx)
	if err == nil {
		return nil
	}
	// These server outcomes definitively abort the transaction. In particular,
	// 40P01/40001 may be retried by Grant with its original operation IDs.
	if retryable(err) || errors.Is(err, pgx.ErrTxCommitRollback) {
		return err
	}
	// A disconnected client or cancelled context cannot establish the outcome.
	return ErrOutcomeUnknown
}

func oneRow(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrIntegrity
	}
	return nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// RequireRecentAdministrator reuses the transaction's current authorization
// checks. The caller must use its existing transaction/WithTx instance.
func RequireRecentAdministrator(ctx context.Context, q *db.Queries, principal Principal) error {
	_, _, _, err := authorize(ctx, q, principal, principal.ActorUserID, true)
	return err
}

// RequireAdministrator validates the complete MFA session for read-only management.
func RequireAdministrator(ctx context.Context, q *db.Queries, p Principal) error {
	_, _, _, e := authorize(ctx, q, p, p.ActorUserID, false)
	return e
}
