package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"invoice.local/system/backend/internal/credit"
	"invoice.local/system/backend/internal/devicekey"
	"invoice.local/system/backend/internal/httpdevice"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

var ErrDeviceProof = errors.New("invalid device signature")
var ErrDeviceProofRequired = errors.New("device signature required")

// Internal routing sentinel: it is never sufficient authorization by itself.
var ErrDeviceSession = errors.New("USER session requires device proof")

type DeviceChallengeCommand struct {
	Purpose    string          `json:"purpose"`
	PublicJWK  json.RawMessage `json:"public_jwk,omitempty"`
	SetupToken string          `json:"setup_token,omitempty"`
	TransferID string          `json:"transfer_id,omitempty"`
}
type DeviceChallengeResult struct {
	ID            string    `json:"challenge_id"`
	Nonce         string    `json:"nonce"`
	Context       string    `json:"context_id"`
	ProofKey      string    `json:"proof_key_id"`
	ServerTime    time.Time `json:"server_time"`
	ExpiresAt     time.Time `json:"expires_at"`
	Epoch         int32     `json:"binding_epoch"`
	Purpose       string    `json:"purpose"`
	KeyThumbprint string    `json:"key_thumbprint"`
}
type InitialDevicePasswordCommand struct {
	Token       string          `json:"token"`
	Password    string          `json:"new_password"`
	PublicJWK   json.RawMessage `json:"public_jwk"`
	ChallengeID string          `json:"challenge_id"`
}
type InitialDevicePasswordResult struct {
	State      string    `json:"state"`
	TransferID string    `json:"transfer_id"`
	ContextID  string    `json:"context_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}
type DeviceContextResult struct {
	Generation    string    `json:"generation"`
	BindingID     *string   `json:"binding_id"`
	ServerTime    time.Time `json:"server_time"`
	Epoch         int32     `json:"binding_epoch"`
	KeyThumbprint *string   `json:"key_thumbprint"`
}

func liveDeviceUser(u db.User) bool {
	return u.Role == "USER" && u.IsActive && u.RecoveryState == "NORMAL"
}
func liveDevicePre(p db.AuthPreSession, u db.User, now time.Time) bool {
	return liveDeviceUser(u) && p.UserID == u.ID && p.AuthVersion == u.AuthVersion && p.BindingEpoch == u.BindingEpoch && !p.UsedAt.Valid && now.Before(p.ExpiresAt.Time)
}
func liveDeviceSession(u db.User, b db.DeviceBinding, s db.DeviceSession, now time.Time) bool {
	return liveDeviceUser(u) && u.OnboardingState == "READY" && u.DeviceState == "ACTIVE" && b.UserID == u.ID && b.Status == "ACTIVE" && b.Epoch == u.BindingEpoch && s.UserID == u.ID && s.BindingID == b.ID && s.Status == "ACTIVE" && s.AuthVersion == u.AuthVersion && s.BindingEpoch == u.BindingEpoch && now.Before(s.ExpiresAt.Time) && now.Before(s.LastActiveAt.Time.Add(30*time.Minute))
}
func deviceToken(raw string) ([]byte, error) {
	h, e := ParseToken(raw)
	if e != nil {
		return nil, ErrAuth
	}
	return h[:], nil
}

// DeviceChallenge authorizes a narrowly scoped proof. It never updates USER
// idle time and never creates or revokes a formal session.
func (s *Service) DeviceChallenge(ctx context.Context, preRaw, sessionRaw string, c DeviceChallengeCommand) (DeviceChallengeResult, error) {
	q0 := db.New(s.pool)
	var uid, subject, generation, transferID pgtype.UUID
	var token db.PasswordToken
	var pre db.AuthPreSession
	var session db.DeviceSession
	var candidate devicekey.PublicKey
	var e error
	switch c.Purpose {
	case "INITIAL_SETUP":
		if len(c.PublicJWK) == 0 || c.SetupToken == "" || c.TransferID != "" {
			return DeviceChallengeResult{}, ErrInvalid
		}
		candidate, e = devicekey.ParsePublicJWK(c.PublicJWK)
		if e != nil {
			return DeviceChallengeResult{}, ErrInvalid
		}
		h, e := deviceToken(c.SetupToken)
		if e != nil {
			return DeviceChallengeResult{}, e
		}
		token, e = q0.DeviceSetupTokenByHash(ctx, h)
		if e != nil {
			return DeviceChallengeResult{}, ErrAuth
		}
		uid, subject, generation = token.UserID, token.ID, token.ID
	case "BIND", "LOGIN", "PAIR":
		if c.SetupToken != "" || (c.Purpose != "PAIR" && len(c.PublicJWK) != 0) || (c.Purpose == "LOGIN" && c.TransferID != "") {
			return DeviceChallengeResult{}, ErrInvalid
		}
		if c.Purpose == "PAIR" && len(c.PublicJWK) == 0 {
			return DeviceChallengeResult{}, ErrInvalid
		}
		purpose := "LOGIN"
		if c.Purpose == "BIND" {
			purpose = "BIND"
			transferID, e = ParseID(c.TransferID)
			if e != nil {
				return DeviceChallengeResult{}, ErrInvalid
			}
		}
		if c.Purpose == "PAIR" {
			candidate, e = devicekey.ParsePublicJWK(c.PublicJWK)
			if e != nil {
				return DeviceChallengeResult{}, ErrInvalid
			}
		}
		pre, e = s.pre(ctx, preRaw, purpose)
		if e != nil {
			return DeviceChallengeResult{}, ErrAuth
		}
		uid, subject, generation = pre.UserID, pre.ID, pre.ID
	case "REQUEST":
		if len(c.PublicJWK) != 0 || c.SetupToken != "" || c.TransferID != "" {
			return DeviceChallengeResult{}, ErrInvalid
		}
		h, e := deviceToken(sessionRaw)
		if e != nil {
			return DeviceChallengeResult{}, e
		}
		session, e = q0.SessionByHash(ctx, h)
		if e != nil {
			return DeviceChallengeResult{}, ErrAuth
		}
		uid, subject, generation = session.UserID, session.ID, session.Generation
	default:
		return DeviceChallengeResult{}, ErrInvalid
	}
	if e = s.Allow(ctx, "device-challenge", IDText(uid)+"/"+c.Purpose, 60, 60); e != nil {
		return DeviceChallengeResult{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return DeviceChallengeResult{}, e
	}
	defer rollbackAccount(tx)
	u, e := q.LockDeviceUser(ctx, uid)
	if e != nil || !liveDeviceUser(u) {
		return DeviceChallengeResult{}, ErrAuth
	}
	keyHash, kind := candidate.Thumbprint, "PASSWORD_TOKEN"
	proofID := NewID()
	challengeID := proofID
	var binding db.DeviceBinding
	var transfer db.BindingTransfer
	var pairing db.DevicePairing
	switch c.Purpose {
	case "INITIAL_SETUP":
		token, e = q.LockPasswordToken(ctx, token.ID)
	case "BIND":
		kind = "PRE_SESSION"
		pre, e = q.LockPreSession(ctx, pre.ID)
		if e != nil {
			break
		}
		transfer, e = q.LockBindingTransfer(ctx, db.LockBindingTransferParams{ID: transferID, UserID: uid})
		if e != nil {
			break
		}
		pairing, e = q.LockDevicePairing(ctx, db.LockDevicePairingParams{ID: transfer.PairingID, UserID: uid})
		keyHash = transfer.TargetKeyHash
	case "LOGIN", "PAIR":
		kind = "PRE_SESSION"
		if c.Purpose == "PAIR" {
			// A pairing candidate must present its own key: the challenge binds
			// the offered thumbprint and the fresh proof key id.
			keyHash = candidate.Thumbprint
			pre, e = q.LockPreSession(ctx, pre.ID)
			break
		}
		binding, e = q.LockActiveDeviceBinding(ctx, uid)
		if e != nil {
			break
		}
		pre, e = q.LockPreSession(ctx, pre.ID)
		keyHash, proofID = binding.KeyThumbprint, binding.ID
	case "REQUEST":
		kind = "SESSION"
		binding, e = q.LockActiveDeviceBinding(ctx, uid)
		if e != nil {
			break
		}
		session, e = q.LockProofSession(ctx, db.LockProofSessionParams{ID: session.ID, UserID: uid})
		keyHash, proofID = binding.KeyThumbprint, binding.ID
	}
	if e != nil {
		return DeviceChallengeResult{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return DeviceChallengeResult{}, e
	}
	switch c.Purpose {
	case "INITIAL_SETUP":
		if token.Purpose != "INITIAL_SETUP" || token.UserID != u.ID || token.AuthVersion != u.AuthVersion || token.UsedAt.Valid || !now.Time.Before(token.ExpiresAt.Time) || u.OnboardingState != "NEEDS_SETUP" || u.DeviceState != "INITIAL_PENDING" {
			return DeviceChallengeResult{}, ErrAuth
		}
	case "BIND":
		if pre.Purpose != "BIND" || !liveDevicePre(pre, u, now.Time) || !bindTransferLive(u, transfer, pairing, now.Time) {
			return DeviceChallengeResult{}, ErrAuth
		}
	case "LOGIN":
		if pre.Purpose != "LOGIN" || !liveDevicePre(pre, u, now.Time) || u.OnboardingState != "READY" || u.DeviceState != "ACTIVE" || binding.Epoch != u.BindingEpoch {
			return DeviceChallengeResult{}, ErrAuth
		}
	case "PAIR":
		// Pairing candidates authenticate with password only: the account is
		// READY and the browser may have lost or never held the bound key.
		if pre.Purpose != "LOGIN" || !liveDevicePre(pre, u, now.Time) || u.OnboardingState != "READY" || u.Role != "USER" {
			return DeviceChallengeResult{}, ErrAuth
		}
	case "REQUEST":
		if !liveDeviceSession(u, binding, session, now.Time) {
			return DeviceChallengeResult{}, ErrAuth
		}
	}
	nonce, e := NewToken()
	if e != nil {
		return DeviceChallengeResult{}, e
	}
	hash := nonce.Hash()
	expires := now.Time.Add(60 * time.Second)
	if e = q.InsertDeviceChallenge(ctx, db.InsertDeviceChallengeParams{ID: challengeID, NonceHash: hash[:], Purpose: c.Purpose, SubjectID: subject, SubjectKind: kind, BindingTransferID: transferID, KeyHash: keyHash, AuthVersion: u.AuthVersion, Epoch: u.BindingEpoch, Generation: generation, ExpiresAt: stamp(expires)}); e != nil {
		return DeviceChallengeResult{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return DeviceChallengeResult{}, e
	}
	return DeviceChallengeResult{ID: IDText(challengeID), Nonce: nonce.Reveal(), Context: IDText(generation), ProofKey: IDText(proofID), ServerTime: now.Time, ExpiresAt: expires, Epoch: u.BindingEpoch, Purpose: c.Purpose, KeyThumbprint: keyHash}, nil
}
func initialTransferLive(u db.User, t db.BindingTransfer, p db.DevicePairing, now time.Time) bool {
	return liveDeviceUser(u) && u.OnboardingState == "NEEDS_SETUP" && u.DeviceState == "INITIAL_PENDING" && t.UserID == u.ID && t.Purpose == "INITIAL" && t.Status == "PENDING" && t.TargetEpoch == u.BindingEpoch && now.Before(t.ExpiresAt.Time) && p.ID == t.PairingID && p.UserID == u.ID && p.ProofVerified && p.KeyThumbprint == t.TargetKeyHash && now.Before(p.ExpiresAt.Time)
}
func (s *Service) checkDeviceProof(r *http.Request, body []byte, ch db.DeviceChallenge, u db.User, purpose, kind string, subject, generation, proofID pgtype.UUID, key devicekey.PublicKey, now time.Time) error {
	if len(r.Header.Values("Signature-Input")) == 0 {
		return ErrDeviceProofRequired
	}
	if ch.Purpose != purpose || ch.SubjectKind != kind || ch.SubjectID != subject || ch.Generation != generation || ch.AuthVersion != u.AuthVersion || ch.Epoch != u.BindingEpoch || ch.KeyHash != key.Thumbprint || ch.UsedAt.Valid || !now.Before(ch.ExpiresAt.Time) || len(ch.NonceHash) != 32 {
		return ErrDeviceProof
	}
	var hash [32]byte
	copy(hash[:], ch.NonceHash)
	if httpdevice.Verify(r, body, s.origin, key, httpdevice.Expectation{KeyID: IDText(proofID), ContextID: IDText(generation), NonceHash: hash, IssuedAt: ch.CreatedAt.Time, ExpiresAt: ch.ExpiresAt.Time}, now) != nil {
		return ErrDeviceProof
	}
	return nil
}
func (s *Service) SetInitialDevicePassword(ctx context.Context, c InitialDevicePasswordCommand, r *http.Request, body []byte) (InitialDevicePasswordResult, Token, error) {
	h, e := deviceToken(c.Token)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	key, e := devicekey.ParsePublicJWK(c.PublicJWK)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrInvalid
	}
	challengeID, e := ParseID(c.ChallengeID)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrInvalid
	}
	token, e := db.New(s.pool).DeviceSetupTokenByHash(ctx, h)
	if e != nil || token.UsedAt.Valid {
		return InitialDevicePasswordResult{}, Token{}, ErrAuth
	}
	if e = s.Allow(ctx, "device-password", IDText(token.UserID), 5, 300); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	phc, e := s.passwords.Hash(ctx, c.Password)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrInvalid
	}
	cookie, e := NewToken()
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	defer rollbackAccount(tx)
	u, e := q.LockDeviceUser(ctx, token.UserID)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrAuth
	}
	token, e = q.LockPasswordToken(ctx, token.ID)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrAuth
	}
	ch, e := q.LockDeviceChallenge(ctx, db.LockDeviceChallengeParams{ID: challengeID, SubjectID: token.ID, SubjectKind: "PASSWORD_TOKEN"})
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, ErrDeviceProof
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	if !liveDeviceUser(u) || u.OnboardingState != "NEEDS_SETUP" || u.DeviceState != "INITIAL_PENDING" || token.UserID != u.ID || token.Purpose != "INITIAL_SETUP" || token.AuthVersion != u.AuthVersion || token.UsedAt.Valid || !now.Time.Before(token.ExpiresAt.Time) {
		return InitialDevicePasswordResult{}, Token{}, ErrAuth
	}
	if e = s.checkDeviceProof(r, body, ch, u, "INITIAL_SETUP", "PASSWORD_TOKEN", token.ID, token.ID, ch.ID, key, now.Time); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	if n, e := q.ConsumeDeviceChallenge(ctx, ch.ID); e != nil || n != 1 {
		return InitialDevicePasswordResult{}, Token{}, ErrDeviceProof
	}
	if n, e := q.ConsumePasswordToken(ctx, token.ID); e != nil || n != 1 {
		return InitialDevicePasswordResult{}, Token{}, ErrAuth
	}
	if e = q.UpsertPassword(ctx, db.UpsertPasswordParams{UserID: u.ID, PasswordPhc: phc.StoredPHC()}); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	pairID, transferID, preID := NewID(), NewID(), NewID()
	if e = q.InsertInitialPairing(ctx, db.InsertInitialPairingParams{ID: pairID, UserID: u.ID, PublicJwk: c.PublicJWK, KeyThumbprint: key.Thumbprint, ExpiresAt: stamp(now.Time.Add(15 * time.Minute))}); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	if e = q.InsertInitialTransfer(ctx, db.InsertInitialTransferParams{ID: transferID, UserID: u.ID, PairingID: pairID, TargetKeyHash: key.Thumbprint, TargetEpoch: u.BindingEpoch, ExpiresAt: stamp(now.Time.Add(10 * time.Minute))}); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	expires := now.Time.Add(5 * time.Minute)
	cookieHash := cookie.Hash()
	if e = q.InsertPreSession(ctx, db.InsertPreSessionParams{ID: preID, TokenHash: cookieHash[:], UserID: u.ID, Purpose: "BIND", AuthVersion: u.AuthVersion, BindingEpoch: u.BindingEpoch, ExpiresAt: stamp(expires)}); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	if e = deviceAudit(ctx, q, u.ID, "INITIAL_DEVICE_AUTHORIZED", transferID); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return InitialDevicePasswordResult{}, Token{}, e
	}
	return InitialDevicePasswordResult{State: "BIND_REQUIRED", TransferID: IDText(transferID), ContextID: IDText(preID), ExpiresAt: expires}, cookie, nil
}
func deviceAudit(ctx context.Context, q *db.Queries, uid pgtype.UUID, action string, requestID pgtype.UUID) error {
	return q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: uid, Action: action, TargetID: uid, RequestID: requestID, RedactedMeta: []byte("{}")})
}

func (s *Service) CompleteDeviceProof(ctx context.Context, preRaw, transferRaw, challengeRaw string, r *http.Request, body []byte, bind bool) (SessionResult, Token, error) {
	purpose := "LOGIN"
	if bind {
		purpose = "BIND"
	}
	pre, e := s.pre(ctx, preRaw, purpose)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	challengeID, e := ParseID(challengeRaw)
	if e != nil {
		return SessionResult{}, Token{}, ErrInvalid
	}
	var transferID pgtype.UUID
	if bind {
		transferID, e = ParseID(transferRaw)
		if e != nil {
			return SessionResult{}, Token{}, ErrInvalid
		}
	}
	if e = s.Allow(ctx, "device-proof", IDText(pre.UserID), 20, 60); e != nil {
		return SessionResult{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	defer rollbackAccount(tx)
	u, e := q.LockDeviceUser(ctx, pre.UserID)
	if e != nil || !liveDeviceUser(u) {
		return SessionResult{}, Token{}, ErrAuth
	}
	pre, e = q.LockPreSession(ctx, pre.ID)
	if e != nil || pre.Purpose != purpose {
		return SessionResult{}, Token{}, ErrAuth
	}
	var binding db.DeviceBinding
	var transfer db.BindingTransfer
	var pairing db.DevicePairing
	var key devicekey.PublicKey
	if bind {
		transfer, e = q.LockBindingTransfer(ctx, db.LockBindingTransferParams{ID: transferID, UserID: u.ID})
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		pairing, e = q.LockDevicePairing(ctx, db.LockDevicePairingParams{ID: transfer.PairingID, UserID: u.ID})
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		key, e = devicekey.ParsePublicJWK(pairing.PublicJwk)
		if _, err := q.ActiveDeviceBinding(ctx, u.ID); err == nil {
			return SessionResult{}, Token{}, ErrConflict
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return SessionResult{}, Token{}, err
		}
	} else {
		binding, e = q.LockActiveDeviceBinding(ctx, u.ID)
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		key, e = devicekey.ParsePublicJWK(binding.PublicJwk)
	}
	if e != nil {
		return SessionResult{}, Token{}, ErrDeviceProof
	}
	ch, e := q.LockDeviceChallenge(ctx, db.LockDeviceChallengeParams{ID: challengeID, SubjectID: pre.ID, SubjectKind: "PRE_SESSION"})
	if e != nil {
		return SessionResult{}, Token{}, ErrDeviceProof
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	if !liveDevicePre(pre, u, now.Time) {
		return SessionResult{}, Token{}, ErrAuth
	}
	proofID := binding.ID
	if bind {
		proofID = ch.ID
		if ch.BindingTransferID != transfer.ID || !initialTransferLive(u, transfer, pairing, now.Time) {
			return SessionResult{}, Token{}, ErrAuth
		}
	} else if u.OnboardingState != "READY" || u.DeviceState != "ACTIVE" || binding.Epoch != u.BindingEpoch {
		return SessionResult{}, Token{}, ErrAuth
	}
	if e = s.checkDeviceProof(r, body, ch, u, purpose, "PRE_SESSION", pre.ID, pre.ID, proofID, key, now.Time); e != nil {
		return SessionResult{}, Token{}, e
	}
	if n, e := q.ConsumeDeviceChallenge(ctx, ch.ID); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrDeviceProof
	}
	if n, e := q.ConsumePreSession(ctx, pre.ID); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	if bind {
		if n, e := q.ConsumeInitialTransfer(ctx, transfer.ID); e != nil || n != 1 {
			return SessionResult{}, Token{}, ErrAuth
		}
		binding = db.DeviceBinding{ID: NewID(), UserID: u.ID, PublicJwk: pairing.PublicJwk, KeyThumbprint: pairing.KeyThumbprint, Epoch: u.BindingEpoch, Status: "ACTIVE"}
		if e = q.InsertActiveDeviceBinding(ctx, db.InsertActiveDeviceBindingParams{ID: binding.ID, UserID: u.ID, PublicJwk: binding.PublicJwk, KeyThumbprint: binding.KeyThumbprint, Epoch: u.BindingEpoch}); e != nil {
			return SessionResult{}, Token{}, e
		}
		if transfer.Purpose == "RESET" {
			// The approved reset already advanced the epoch; completing the
			// bind activates the account on the designated key.
			if n, e := q.CompleteResetBinding(ctx, db.CompleteResetBindingParams{ID: u.ID, BindingEpoch: transfer.TargetEpoch}); e != nil || n != 1 {
				return SessionResult{}, Token{}, ErrAuth
			}
		} else if n, e := q.CompleteUserDeviceOnboarding(ctx, u.ID); e != nil || n != 1 {
			return SessionResult{}, Token{}, ErrAuth
		}
	}
	result, cookie, e := issueDeviceSession(ctx, q, u, binding.ID, now.Time)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = deviceAudit(ctx, q, u.ID, "USER_DEVICE_SESSION_ESTABLISHED", ch.ID); e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return SessionResult{}, Token{}, e
	}
	return result, cookie, nil
}
func issueDeviceSession(ctx context.Context, q *db.Queries, u db.User, bindingID pgtype.UUID, now time.Time) (SessionResult, Token, error) {
	cookie, e := NewToken()
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	hash := cookie.Hash()
	generation := NewID()
	expires := now.Add(8 * time.Hour)
	if e = q.RevokeUserSessions(ctx, u.ID); e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = q.InsertUserDeviceSession(ctx, db.InsertUserDeviceSessionParams{ID: NewID(), UserID: u.ID, TokenHash: hash[:], BindingID: bindingID, Generation: generation, AuthVersion: u.AuthVersion, BindingEpoch: u.BindingEpoch, ExpiresAt: stamp(expires)}); e != nil {
		return SessionResult{}, Token{}, e
	}
	return SessionResult{State: "AUTHENTICATED", Generation: IDText(generation), ExpiresAt: expires}, cookie, nil
}
func (s *Service) DeviceContext(ctx context.Context, raw string) (DeviceContextResult, error) {
	hash, e := deviceToken(raw)
	if e != nil {
		return DeviceContextResult{}, e
	}
	q := db.New(s.pool)
	session, e := q.SessionByHash(ctx, hash)
	if e != nil {
		return DeviceContextResult{}, ErrAuth
	}
	u, e := q.AccountByID(ctx, session.UserID)
	if e != nil {
		return DeviceContextResult{}, ErrAuth
	}
	if u.Role == "SUPERADMIN" {
		i, e := s.Resolve(ctx, raw)
		if e != nil {
			return DeviceContextResult{}, e
		}
		now, e := q.AccountClock(ctx)
		if e != nil {
			return DeviceContextResult{}, e
		}
		return DeviceContextResult{Generation: IDText(i.Principal.Generation), ServerTime: now.Time, Epoch: u.BindingEpoch}, nil
	}
	b, e := q.ActiveDeviceBinding(ctx, u.ID)
	if e != nil {
		return DeviceContextResult{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil || !liveDeviceSession(u, b, session, now.Time) {
		return DeviceContextResult{}, ErrAuth
	}
	id := IDText(b.ID)
	return DeviceContextResult{Generation: IDText(session.Generation), BindingID: &id, ServerTime: now.Time, Epoch: u.BindingEpoch, KeyThumbprint: &b.KeyThumbprint}, nil
}

// ResolveDevice consumes a valid REQUEST nonce atomically with current account,
// binding and session checks. Invalid signatures never burn a nonce or touch idle.
func (s *Service) ResolveDevice(ctx context.Context, raw string, r *http.Request, body []byte, logout bool) (Identity, error) {
	hash, e := deviceToken(raw)
	if e != nil {
		return Identity{}, e
	}
	q0 := db.New(s.pool)
	session, e := q0.SessionByHash(ctx, hash)
	if e != nil {
		return Identity{}, ErrAuth
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Identity{}, e
	}
	defer rollbackAccount(tx)
	u, e := q.LockDeviceUser(ctx, session.UserID)
	if e != nil || !liveDeviceUser(u) {
		return Identity{}, ErrAuth
	}
	b, e := q.LockActiveDeviceBinding(ctx, u.ID)
	if e != nil {
		return Identity{}, ErrAuth
	}
	session, e = q.LockProofSession(ctx, db.LockProofSessionParams{ID: session.ID, UserID: u.ID})
	if e != nil {
		return Identity{}, ErrAuth
	}
	precheck, e := q.AccountClock(ctx)
	if e != nil {
		return Identity{}, e
	}
	if !liveDeviceSession(u, b, session, precheck.Time) {
		return Identity{}, ErrAuth
	}
	if len(r.Header.Values("Signature-Input")) == 0 {
		return Identity{}, ErrDeviceProofRequired
	}
	proof, e := httpdevice.ReadProof(r)
	if e != nil {
		return Identity{}, ErrDeviceProof
	}
	nonceHash, e := httpdevice.NonceHash(proof.Nonce)
	if e != nil {
		return Identity{}, ErrDeviceProof
	}
	found, e := q.DeviceChallengeByNonce(ctx, nonceHash[:])
	if e != nil {
		return Identity{}, ErrDeviceProof
	}
	ch, e := q.LockDeviceChallenge(ctx, db.LockDeviceChallengeParams{ID: found.ID, SubjectID: session.ID, SubjectKind: "SESSION"})
	if e != nil {
		return Identity{}, ErrDeviceProof
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return Identity{}, e
	}
	if !liveDeviceSession(u, b, session, now.Time) {
		return Identity{}, ErrAuth
	}
	key, e := devicekey.ParsePublicJWK(b.PublicJwk)
	if e != nil {
		return Identity{}, ErrDeviceProof
	}
	if e = s.checkDeviceProof(r, body, ch, u, "REQUEST", "SESSION", session.ID, session.Generation, b.ID, key, now.Time); e != nil {
		return Identity{}, e
	}
	if n, e := q.ConsumeDeviceChallenge(ctx, ch.ID); e != nil || n != 1 {
		return Identity{}, ErrDeviceProof
	}
	wallet, e := q.AccountWallet(ctx, u.ID)
	if e != nil {
		return Identity{}, e
	}
	if logout {
		if n, e := q.RevokeAdminSession(ctx, session.ID); e != nil || n != 1 {
			return Identity{}, ErrAuth
		}
		if e = deviceAudit(ctx, q, u.ID, "USER_LOGOUT", ch.ID); e != nil {
			return Identity{}, e
		}
	} else if n, e := q.TouchProofSession(ctx, session.ID); e != nil || n != 1 {
		return Identity{}, ErrAuth
	}
	if e = commitAccount(ctx, tx); e != nil {
		return Identity{}, e
	}
	return Identity{User: userRow(u), Wallet: credit.Wallet{Available: fmt.Sprint(wallet.Available), Frozen: fmt.Sprint(wallet.Frozen)}, Principal: credit.Principal{ActorUserID: u.ID, SessionID: session.ID, Generation: session.Generation}}, nil
}
