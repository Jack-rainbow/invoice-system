package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"invoice.local/system/backend/internal/credit"
	db "invoice.local/system/backend/internal/platform/postgres/db"
)

var (
	ErrAuth           = errors.New("authentication required")
	ErrInvalid        = errors.New("invalid account request")
	ErrRateLimited    = errors.New("authentication rate limited")
	ErrConflict       = errors.New("account operation conflicts")
	ErrLinkIssued     = errors.New("setup link already issued; explicit reissue required")
	ErrOutcomeUnknown = errors.New("account result unknown; reconcile original operation")
)
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)
var usernameFilterPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

func normalizeUsername(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, strings.TrimSpace(name))
}

type Service struct {
	pool      *pgxpool.Pool
	keys      *Keyring
	passwords *PasswordHasher
	dummy     string
	origin    string
}
type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	IsActive   bool   `json:"is_active"`
	Onboarding string `json:"onboarding_state"`
}
type BootstrapLink struct {
	Link      string    `json:"bootstrap_link"`
	ExpiresAt time.Time `json:"expires_at"`
}
type BootstrapStart struct {
	Context   string    `json:"bootstrap_context"`
	Seed      string    `json:"seed"`
	URI       string    `json:"otpauth_uri"`
	ExpiresAt time.Time `json:"expires_at"`
}
type PreLogin struct {
	Stage     string    `json:"stage"`
	Context   string    `json:"context_id"`
	ExpiresAt time.Time `json:"expires_at"`
}
type SessionResult struct {
	State         string    `json:"state"`
	Generation    string    `json:"generation"`
	ExpiresAt     time.Time `json:"expires_at"`
	RecoveryCodes []string  `json:"recovery_codes,omitempty"`
	VerifiedAt    time.Time `json:"-"`
}
type Challenge struct {
	ID        string    `json:"challenge_id"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Identity struct {
	User      User
	Wallet    credit.Wallet
	Principal credit.Principal
	Recent    bool
}
type CreateUserCommand struct {
	Username     string `json:"username"`
	Verification string `json:"customer_verification_ref"`
	Reason       string `json:"reason"`
}
type CreatedUser struct {
	User      User          `json:"user"`
	Wallet    credit.Wallet `json:"wallet"`
	SetupLink string        `json:"setup_link,omitempty"`
}
type UserFilter struct {
	Cursor   string
	Limit    int32
	Username string
	Status   string
}
type ManagedUser struct {
	User         User          `json:"user"`
	Wallet       credit.Wallet `json:"wallet"`
	BindingEpoch int32         `json:"binding_epoch"`
	DeviceState  string        `json:"device_state"`
}
type ManagedUserPage struct {
	Items      []ManagedUser `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func NewService(pool *pgxpool.Pool, keys *Keyring, origin string) (*Service, error) {
	u, e := url.Parse(origin)
	if pool == nil || keys == nil || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrInvalid
	}
	h, e := NewPasswordHasher(2)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dummy, e := h.Hash(ctx, "non-account timing-only credential")
	if e != nil {
		return nil, e
	}
	return &Service{pool: pool, keys: keys, passwords: h, dummy: dummy.StoredPHC(), origin: origin}, nil
}
func NewID() pgtype.UUID {
	var id pgtype.UUID
	_, _ = rand.Read(id.Bytes[:])
	id.Bytes[6] = id.Bytes[6]&15 | 64
	id.Bytes[8] = id.Bytes[8]&63 | 128
	id.Valid = true
	return id
}
func IDText(id pgtype.UUID) string {
	h := hex.EncodeToString(id.Bytes[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func ParseID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if len(s) != 36 || strings.ToLower(s) != s || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || id.Scan(s) != nil || id.Bytes == ([16]byte{}) {
		return pgtype.UUID{}, ErrInvalid
	}
	return id, nil
}
func stamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func rollbackAccount(tx pgx.Tx) {
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(c)
}
func commitAccount(ctx context.Context, tx pgx.Tx) error {
	if e := tx.Commit(ctx); e != nil {
		return ErrOutcomeUnknown
	}
	return nil
}

// Pool exposes the shared connection pool for HTTP-layer read helpers that
// compose several services; business rules stay inside the services.
func (s *Service) Pool() *pgxpool.Pool { return s.pool }

func (s *Service) transaction(ctx context.Context) (pgx.Tx, *db.Queries, error) {
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if e != nil {
		return nil, nil, e
	}
	if _, e = tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); e != nil {
		rollbackAccount(tx)
		return nil, nil, e
	}
	return tx, db.New(s.pool).WithTx(tx), nil
}
func userRow(u db.User) User {
	return User{ID: IDText(u.ID), Username: u.Username, Role: u.Role, IsActive: u.IsActive, Onboarding: u.OnboardingState}
}
func liveUser(u db.LockCreditUserRow) bool { return u.IsActive && u.RecoveryState == "NORMAL" }
func (s *Service) Allow(ctx context.Context, scope, key string, max, window int32) error {
	sum := sha256.Sum256([]byte(scope + "\x00" + key))
	q := db.New(s.pool)
	if e := q.CollectAuthRates(ctx); e != nil {
		return e
	}
	_, e := q.AuthRateAttempt(ctx, db.AuthRateAttemptParams{Scope: scope, KeyHash: sum[:], Maximum: max, WindowSeconds: window})
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrRateLimited
	}
	return e
}

// InitializeAdministrator is for the local protected CLI, never a public route.
func (s *Service) InitializeAdministrator(ctx context.Context, name string, reissue bool) (BootstrapLink, error) {
	name = normalizeUsername(name)
	if !usernamePattern.MatchString(name) {
		return BootstrapLink{}, ErrInvalid
	}
	token, e := NewToken()
	if e != nil {
		return BootstrapLink{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return BootstrapLink{}, e
	}
	defer rollbackAccount(tx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(827341981)"); e != nil {
		return BootstrapLink{}, e
	}
	u, e := q.AccountByName(ctx, name)
	if errors.Is(e, pgx.ErrNoRows) {
		if reissue {
			return BootstrapLink{}, ErrAuth
		}
		var n int64
		n, e = q.CountAdministrators(ctx)
		if e != nil {
			return BootstrapLink{}, e
		}
		if n != 0 {
			return BootstrapLink{}, ErrConflict
		}
		id := NewID()
		e = q.InsertAccount(ctx, db.InsertAccountParams{ID: id, Username: name, Role: "SUPERADMIN", OnboardingState: "ENROLLMENT_REQUIRED"})
		if e != nil {
			return BootstrapLink{}, e
		}
		if e = q.InsertZeroWallet(ctx, id); e != nil {
			return BootstrapLink{}, e
		}
		u, e = q.AccountByID(ctx, id)
	} else if e == nil {
		if !reissue || u.Role != "SUPERADMIN" || u.OnboardingState != "ENROLLMENT_REQUIRED" {
			return BootstrapLink{}, ErrConflict
		}
		if _, e = q.LockCreditUser(ctx, u.ID); e != nil {
			return BootstrapLink{}, e
		}
		if n, e := q.ResetUnreadyAdmin(ctx, u.ID); e != nil || n != 1 {
			return BootstrapLink{}, ErrConflict
		}
		if e = q.InvalidatePasswordTokens(ctx, u.ID); e != nil {
			return BootstrapLink{}, e
		}
		if e = q.InvalidatePreSessions(ctx, u.ID); e != nil {
			return BootstrapLink{}, e
		}
		if e = q.DisableEnrollingFactors(ctx, u.ID); e != nil {
			return BootstrapLink{}, e
		}
		u, e = q.AccountByID(ctx, u.ID)
	}
	if e != nil {
		return BootstrapLink{}, e
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return BootstrapLink{}, e
	}
	expires := now.Time.Add(15 * time.Minute)
	hash := token.Hash()
	if e = q.InsertPasswordToken(ctx, db.InsertPasswordTokenParams{ID: NewID(), UserID: u.ID, TokenHash: hash[:], Purpose: "ADMIN_BOOTSTRAP", AuthVersion: u.AuthVersion, ExpiresAt: stamp(expires)}); e != nil {
		return BootstrapLink{}, e
	}
	if e = q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: u.ID, Action: "ADMIN_BOOTSTRAP_ISSUED", TargetID: u.ID, RequestID: NewID(), RedactedMeta: []byte("{}")}); e != nil {
		return BootstrapLink{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return BootstrapLink{}, e
	}
	return BootstrapLink{Link: s.origin + "/setup/admin#token=" + token.Reveal(), ExpiresAt: expires}, nil
}
func (s *Service) BeginBootstrap(ctx context.Context, raw, password string) (BootstrapStart, Token, error) {
	hash, e := ParseToken(raw)
	if e != nil {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	preliminary, e := db.New(s.pool).BootstrapTokenByHash(ctx, hash[:])
	if e != nil {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	phc, e := s.passwords.Hash(ctx, password)
	if e != nil {
		return BootstrapStart{}, Token{}, ErrInvalid
	}
	secret, e := NewTOTPSecret()
	if e != nil {
		return BootstrapStart{}, Token{}, e
	}
	cookie, e := NewToken()
	if e != nil {
		return BootstrapStart{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return BootstrapStart{}, Token{}, e
	}
	defer rollbackAccount(tx)
	actor, e := q.LockCreditUser(ctx, preliminary.UserID)
	if e != nil {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	token, e := q.LockPasswordToken(ctx, preliminary.ID)
	if e != nil {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return BootstrapStart{}, Token{}, e
	}
	if !liveUser(actor) || actor.Role != "SUPERADMIN" || actor.OnboardingState != "ENROLLMENT_REQUIRED" || token.Purpose != "ADMIN_BOOTSTRAP" || token.AuthVersion != actor.AuthVersion || token.UsedAt.Valid || !now.Time.Before(token.ExpiresAt.Time) {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	if n, e := q.ConsumePasswordToken(ctx, token.ID); e != nil || n != 1 {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	if e = q.UpsertPassword(ctx, db.UpsertPasswordParams{UserID: actor.ID, PasswordPhc: phc.StoredPHC()}); e != nil {
		return BootstrapStart{}, Token{}, e
	}
	preID, factorID := NewID(), NewID()
	expires := now.Time.Add(5 * time.Minute)
	ch := cookie.Hash()
	if e = q.InsertPreSession(ctx, db.InsertPreSessionParams{ID: preID, TokenHash: ch[:], UserID: actor.ID, Purpose: "ADMIN_BOOTSTRAP", AuthVersion: actor.AuthVersion, BindingEpoch: actor.BindingEpoch, ExpiresAt: stamp(expires)}); e != nil {
		return BootstrapStart{}, Token{}, e
	}
	cipher, e := s.keys.seal(factorContext(actor.ID, factorID, 1), secret.EnrollmentSecret())
	if e != nil {
		return BootstrapStart{}, Token{}, e
	}
	if e = q.InsertEnrollingFactor(ctx, db.InsertEnrollingFactorParams{ID: factorID, UserID: actor.ID, SeedCiphertext: cipher}); e != nil {
		return BootstrapStart{}, Token{}, e
	}
	name, e := q.AccountByID(ctx, actor.ID)
	if e != nil {
		return BootstrapStart{}, Token{}, ErrAuth
	}
	if e = commitAccount(ctx, tx); e != nil {
		return BootstrapStart{}, Token{}, e
	}
	uri := "otpauth://totp/" + url.PathEscape("Invoice:"+name.Username) + "?secret=" + secret.EnrollmentSecret() + "&issuer=Invoice&algorithm=SHA1&digits=6&period=30"
	return BootstrapStart{Context: IDText(preID), Seed: secret.EnrollmentSecret(), URI: uri, ExpiresAt: expires}, cookie, nil
}
func factorContext(user, factor pgtype.UUID, version int32) string {
	return "MFA_SEED/" + IDText(user) + "/" + IDText(factor) + "/" + fmt.Sprint(version)
}
func (s *Service) pre(ctx context.Context, raw, purpose string) (db.AuthPreSession, error) {
	h, e := ParseToken(raw)
	if e != nil {
		return db.AuthPreSession{}, ErrAuth
	}
	p, e := db.New(s.pool).PreSessionByHash(ctx, h[:])
	if e != nil || p.Purpose != purpose {
		return db.AuthPreSession{}, ErrAuth
	}
	return p, nil
}
func validPre(p db.AuthPreSession, u db.LockCreditUserRow, now time.Time) bool {
	return p.UserID == u.ID && !p.UsedAt.Valid && now.Before(p.ExpiresAt.Time) && p.AuthVersion == u.AuthVersion && p.BindingEpoch == u.BindingEpoch && liveUser(u)
}
func (s *Service) Login(ctx context.Context, name, password string) (PreLogin, Token, error) {
	name = normalizeUsername(name)
	if !usernamePattern.MatchString(name) {
		return PreLogin{}, Token{}, ErrAuth
	}
	if e := s.Allow(ctx, "login-user", name, 10, 900); e != nil {
		return PreLogin{}, Token{}, e
	}
	q0 := db.New(s.pool)
	u, e := q0.AccountByName(ctx, name)
	stored := s.dummy
	var cred db.PasswordCredential
	if e == nil {
		cred, e = q0.PasswordByUser(ctx, u.ID)
		if e == nil {
			stored = cred.PasswordPhc
		}
	}
	matched, verifyErr := s.passwords.Verify(ctx, password, stored)
	if e != nil || verifyErr != nil || !matched {
		return PreLogin{}, Token{}, ErrAuth
	}
	cookie, e := NewToken()
	if e != nil {
		return PreLogin{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return PreLogin{}, Token{}, e
	}
	defer rollbackAccount(tx)
	actor, e := q.LockCreditUser(ctx, u.ID)
	if e != nil || !liveUser(actor) || actor.OnboardingState != "READY" {
		return PreLogin{}, Token{}, ErrAuth
	}
	current, e := q.PasswordByUser(ctx, u.ID)
	if e != nil || current.Version != cred.Version || actor.AuthVersion != u.AuthVersion {
		return PreLogin{}, Token{}, ErrAuth
	}
	purpose, stage := "LOGIN", "DEVICE_REQUIRED"
	if actor.Role == "SUPERADMIN" {
		// 2FA removed from administrator login by product decision: a valid
		// password yields the full session directly. The factor row stays
		// (created silently at bootstrap) so session integrity checks and the
		// schema keep working.
		factor, e := q.FactorByUser(ctx, actor.ID)
		if e != nil || factor.State != "VERIFIED" {
			return PreLogin{}, Token{}, ErrAuth
		}
		now, e := q.AccountClock(ctx)
		if e != nil {
			return PreLogin{}, Token{}, e
		}
		result, cookie, e := issueSession(ctx, q, actor, factor.FactorVersion, now.Time)
		if e != nil {
			return PreLogin{}, Token{}, e
		}
		if e = commitAccount(ctx, tx); e != nil {
			return PreLogin{}, Token{}, e
		}
		return PreLogin{Stage: "AUTHENTICATED", Context: result.Generation, ExpiresAt: result.ExpiresAt}, cookie, nil
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return PreLogin{}, Token{}, e
	}
	expires := now.Time.Add(5 * time.Minute)
	id := NewID()
	h := cookie.Hash()
	if e = q.InsertPreSession(ctx, db.InsertPreSessionParams{ID: id, TokenHash: h[:], UserID: u.ID, Purpose: purpose, AuthVersion: actor.AuthVersion, BindingEpoch: actor.BindingEpoch, ExpiresAt: stamp(expires)}); e != nil {
		return PreLogin{}, Token{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return PreLogin{}, Token{}, e
	}
	return PreLogin{Stage: stage, Context: IDText(id), ExpiresAt: expires}, cookie, nil
}
func (s *Service) FinishBootstrap(ctx context.Context, raw, contextID, code string) (SessionResult, Token, error) {
	pre, e := s.pre(ctx, raw, "ADMIN_BOOTSTRAP")
	if e != nil || contextID != IDText(pre.ID) {
		return SessionResult{}, Token{}, ErrAuth
	}
	if e = s.Allow(ctx, "mfa-user", IDText(pre.UserID), 10, 300); e != nil {
		return SessionResult{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	defer rollbackAccount(tx)
	actor, e := q.LockCreditUser(ctx, pre.UserID)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	p, e := q.LockPreSession(ctx, pre.ID)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	factor, e := q.LockEnrollingFactor(ctx, actor.ID)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	if !validPre(p, actor, now.Time) || actor.Role != "SUPERADMIN" || actor.OnboardingState != "ENROLLMENT_REQUIRED" {
		return SessionResult{}, Token{}, ErrAuth
	}
	// 2FA removed: bootstrap completes with the password alone. The factor is
	// marked verified with the current time step and recovery codes are no
	// longer issued (there is no second factor to recover).
	step := now.Time.Unix() / 30
	if n, e := q.ConsumePreSession(ctx, p.ID); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	if n, e := q.VerifyEnrollingFactor(ctx, db.VerifyEnrollingFactorParams{ID: factor.ID, LastAcceptedStep: pgtype.Int8{Int64: step, Valid: true}}); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	if n, e := q.CompleteAdminOnboarding(ctx, actor.ID); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	result, cookie, e := issueSession(ctx, q, actor, factor.FactorVersion, now.Time)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return SessionResult{}, Token{}, e
	}
	return result, cookie, nil
}
func (s *Service) matchFactor(f db.MfaFactor, code string, now time.Time) (int64, error) {
	plain, e := s.keys.open(factorContext(f.UserID, f.ID, f.FactorVersion), f.SeedCiphertext)
	if e != nil {
		return -1, ErrAuth
	}
	secret, e := ParseTOTPSecret(plain)
	if e != nil {
		return -1, ErrAuth
	}
	last := int64(-1)
	if f.LastAcceptedStep.Valid {
		last = f.LastAcceptedStep.Int64
	}
	return secret.Match(code, now.Unix(), last)
}
func issueSession(ctx context.Context, q *db.Queries, u db.LockCreditUserRow, factor int32, now time.Time) (SessionResult, Token, error) {
	token, e := NewToken()
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	h := token.Hash()
	id, generation := NewID(), NewID()
	expires := now.Add(8 * time.Hour)
	if e = q.RevokeUserSessions(ctx, u.ID); e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = q.InsertAdminSession(ctx, db.InsertAdminSessionParams{ID: id, UserID: u.ID, TokenHash: h[:], Generation: generation, AuthVersion: u.AuthVersion, BindingEpoch: u.BindingEpoch, FactorVersion: pgtype.Int4{Int32: factor, Valid: true}, ExpiresAt: stamp(expires)}); e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: u.ID, Action: "ADMIN_SESSION_ESTABLISHED", TargetID: u.ID, RequestID: NewID(), RedactedMeta: []byte("{}")}); e != nil {
		return SessionResult{}, Token{}, e
	}
	return SessionResult{State: "AUTHENTICATED", Generation: IDText(generation), ExpiresAt: expires}, token, nil
}
func (s *Service) MFAChallenge(ctx context.Context, raw, purpose string) (Challenge, error) {
	var uid, cid pgtype.UUID
	if purpose == "LOGIN" {
		p, e := s.pre(ctx, raw, "MFA")
		if e != nil {
			return Challenge{}, e
		}
		uid, cid = p.UserID, p.ID
	} else if purpose == "STEP_UP" {
		i, e := s.Resolve(ctx, raw)
		if e != nil {
			return Challenge{}, e
		}
		uid, cid = i.Principal.ActorUserID, i.Principal.SessionID
	} else {
		return Challenge{}, ErrInvalid
	}
	if e := s.Allow(ctx, "mfa-challenge", IDText(uid), 10, 60); e != nil {
		return Challenge{}, e
	}
	if e := s.Allow(ctx, "mfa-context", IDText(cid), 5, 60); e != nil {
		return Challenge{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return Challenge{}, e
	}
	defer rollbackAccount(tx)
	actor, e := q.LockCreditUser(ctx, uid)
	if e != nil || !liveUser(actor) || actor.Role != "SUPERADMIN" || actor.OnboardingState != "READY" {
		return Challenge{}, ErrAuth
	}
	if purpose == "LOGIN" {
		p, e := q.LockPreSession(ctx, cid)
		if e != nil || p.Purpose != "MFA" {
			return Challenge{}, ErrAuth
		}
		now, e := q.AccountClock(ctx)
		if e != nil || !validPre(p, actor, now.Time) {
			return Challenge{}, ErrAuth
		}
	}
	var session db.DeviceSession
	if purpose == "STEP_UP" {
		session, e = q.LockAdminSession(ctx, db.LockAdminSessionParams{ID: cid, UserID: uid})
		if e != nil {
			return Challenge{}, ErrAuth
		}
	}
	factor, e := q.LockVerifiedFactor(ctx, uid)
	if e != nil {
		return Challenge{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return Challenge{}, e
	}
	if purpose == "STEP_UP" && !validAdminSession(actor, session, factor, now.Time) {
		return Challenge{}, ErrAuth
	}
	id := NewID()
	expires := now.Time.Add(time.Minute)
	if e = q.InsertMFAChallenge(ctx, db.InsertMFAChallengeParams{ID: id, UserID: uid, ContextID: cid, Purpose: purpose, FactorVersion: factor.FactorVersion, ExpiresAt: stamp(expires)}); e != nil {
		return Challenge{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return Challenge{}, e
	}
	return Challenge{ID: IDText(id), ExpiresAt: expires}, nil
}
func (s *Service) VerifyMFA(ctx context.Context, raw, challengeID, code, recovery string, stepUp bool) (SessionResult, Token, error) {
	if (code == "") == (recovery == "") {
		return SessionResult{}, Token{}, ErrAuth
	}
	id, e := ParseID(challengeID)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	var uid, cid pgtype.UUID
	var session db.DeviceSession
	if !stepUp {
		p, e := s.pre(ctx, raw, "MFA")
		if e != nil {
			return SessionResult{}, Token{}, e
		}
		uid, cid = p.UserID, p.ID
	} else {
		i, e := s.Resolve(ctx, raw)
		if e != nil {
			return SessionResult{}, Token{}, e
		}
		uid, cid = i.Principal.ActorUserID, i.Principal.SessionID
	}
	if e = s.Allow(ctx, "mfa-user", IDText(uid), 10, 300); e != nil {
		return SessionResult{}, Token{}, e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	defer rollbackAccount(tx)
	actor, e := q.LockCreditUser(ctx, uid)
	if e != nil || !liveUser(actor) || actor.Role != "SUPERADMIN" || actor.OnboardingState != "READY" {
		return SessionResult{}, Token{}, ErrAuth
	}
	var p db.AuthPreSession
	if !stepUp {
		p, e = q.LockPreSession(ctx, cid)
		if e != nil || p.Purpose != "MFA" {
			return SessionResult{}, Token{}, ErrAuth
		}
	} else {
		h, err := ParseToken(raw)
		if err != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		session, e = q.SessionByHash(ctx, h[:])
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		session, e = q.LockAdminSession(ctx, db.LockAdminSessionParams{ID: session.ID, UserID: uid})
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
	}
	factor, e := q.LockVerifiedFactor(ctx, uid)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	ch, e := q.LockMFAChallenge(ctx, id)
	if e != nil {
		return SessionResult{}, Token{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	purpose := "LOGIN"
	if stepUp {
		purpose = "STEP_UP"
	}
	if ch.UserID != uid || ch.ContextID != cid || ch.Purpose != purpose || ch.FactorVersion != factor.FactorVersion || ch.UsedAt.Valid || !now.Time.Before(ch.ExpiresAt.Time) {
		return SessionResult{}, Token{}, ErrAuth
	}
	if !stepUp && !validPre(p, actor, now.Time) {
		return SessionResult{}, Token{}, ErrAuth
	}
	if stepUp && !validAdminSession(actor, session, factor, now.Time) {
		return SessionResult{}, Token{}, ErrAuth
	}
	if recovery != "" {
		if stepUp {
			return SessionResult{}, Token{}, ErrAuth
		}
		h, e := ParseToken(recovery)
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		rc, e := q.LockRecoveryCode(ctx, db.LockRecoveryCodeParams{FactorID: factor.ID, CodeHash: h[:]})
		if e != nil || rc.UsedAt.Valid {
			return SessionResult{}, Token{}, ErrAuth
		}
		if n, e := q.ConsumeRecoveryCode(ctx, rc.ID); e != nil || n != 1 {
			return SessionResult{}, Token{}, ErrAuth
		}
	} else {
		step, e := s.matchFactor(factor, code, now.Time)
		if e != nil {
			return SessionResult{}, Token{}, ErrAuth
		}
		if n, e := q.AcceptFactorStep(ctx, db.AcceptFactorStepParams{ID: factor.ID, LastAcceptedStep: pgtype.Int8{Int64: step, Valid: true}}); e != nil || n != 1 {
			return SessionResult{}, Token{}, ErrAuth
		}
	}
	if n, e := q.ConsumeMFAChallenge(ctx, id); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	if stepUp {
		verified, e := q.StepUpAdminSession(ctx, session.ID)
		if e != nil || !verified.Valid {
			return SessionResult{}, Token{}, ErrAuth
		}
		if e = q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: uid, Action: "ADMIN_MFA_VERIFIED", TargetID: uid, RequestID: id, RedactedMeta: []byte("{}")}); e != nil {
			return SessionResult{}, Token{}, e
		}
		if e = commitAccount(ctx, tx); e != nil {
			return SessionResult{}, Token{}, e
		}
		return SessionResult{State: "AUTHENTICATED", Generation: IDText(session.Generation), ExpiresAt: session.ExpiresAt.Time, VerifiedAt: verified.Time}, Token{}, nil
	}
	if n, e := q.ConsumePreSession(ctx, p.ID); e != nil || n != 1 {
		return SessionResult{}, Token{}, ErrAuth
	}
	result, cookie, e := issueSession(ctx, q, actor, factor.FactorVersion, now.Time)
	if e != nil {
		return SessionResult{}, Token{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return SessionResult{}, Token{}, e
	}
	return result, cookie, nil
}
func validAdminSession(u db.LockCreditUserRow, s db.DeviceSession, f db.MfaFactor, now time.Time) bool {
	return liveUser(u) && u.Role == "SUPERADMIN" && u.OnboardingState == "READY" && s.Status == "ACTIVE" && s.AuthVersion == u.AuthVersion && s.BindingEpoch == u.BindingEpoch && s.FactorVersion.Valid && s.FactorVersion.Int32 == f.FactorVersion && s.MfaVerifiedAt.Valid && !s.MfaVerifiedAt.Time.After(now) && now.Before(s.ExpiresAt.Time) && now.Before(s.LastActiveAt.Time.Add(30*time.Minute))
}
func (s *Service) Resolve(ctx context.Context, raw string) (Identity, error) {
	h, e := ParseToken(raw)
	if e != nil {
		return Identity{}, ErrAuth
	}
	q := db.New(s.pool)
	session, e := q.SessionByHash(ctx, h[:])
	if e != nil {
		return Identity{}, ErrAuth
	}
	// SUPERADMIN resolver only; USER business access uses ResolveDevice instead.
	u, e := q.AccountByID(ctx, session.UserID)
	if e == nil && u.Role == "USER" {
		return Identity{}, ErrDeviceSession
	}
	if e != nil || u.Role != "SUPERADMIN" {
		return Identity{}, ErrAuth
	}
	f, e := q.FactorByUser(ctx, u.ID)
	if e != nil {
		return Identity{}, ErrAuth
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return Identity{}, e
	}
	actor := db.LockCreditUserRow{ID: u.ID, Role: u.Role, IsActive: u.IsActive, OnboardingState: u.OnboardingState, AuthVersion: u.AuthVersion, BindingEpoch: u.BindingEpoch, RecoveryState: u.RecoveryState}
	if !validAdminSession(actor, session, f, now.Time) {
		return Identity{}, ErrAuth
	}
	wallet, e := q.AccountWallet(ctx, u.ID)
	if e != nil {
		return Identity{}, e
	}
	if n, e := q.TouchAdminSession(ctx, session.ID); e != nil || n != 1 {
		return Identity{}, ErrAuth
	}
	return Identity{User: userRow(u), Wallet: credit.Wallet{Available: fmt.Sprint(wallet.Available), Frozen: fmt.Sprint(wallet.Frozen)}, Principal: credit.Principal{ActorUserID: u.ID, SessionID: session.ID, Generation: session.Generation}, Recent: now.Time.Sub(session.MfaVerifiedAt.Time) <= 5*time.Minute}, nil
}
func (s *Service) Logout(ctx context.Context, raw string) error {
	i, e := s.Resolve(ctx, raw)
	if e != nil {
		return e
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireAdministrator(ctx, q, i.Principal); e != nil {
		return ErrAuth
	}
	if n, e := q.RevokeAdminSession(ctx, i.Principal.SessionID); e != nil || n != 1 {
		return ErrAuth
	}
	if e = q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: i.Principal.ActorUserID, Action: "ADMIN_LOGOUT", TargetID: i.Principal.ActorUserID, RequestID: NewID(), RedactedMeta: []byte("{}")}); e != nil {
		return e
	}
	return commitAccount(ctx, tx)
}
func validText(v string, max int) bool {
	if !utf8.ValidString(v) || strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}
func (s *Service) CreateUser(ctx context.Context, p credit.Principal, key pgtype.UUID, c CreateUserCommand) (CreatedUser, error) {
	c.Username = normalizeUsername(c.Username)
	if !usernamePattern.MatchString(c.Username) || !validText(c.Verification, 255) || !validText(c.Reason, 1000) || !key.Valid {
		return CreatedUser{}, ErrInvalid
	}
	c.Verification = strings.TrimSpace(c.Verification)
	c.Reason = strings.TrimSpace(c.Reason)
	b, _ := json.Marshal(c)
	rh := sha256.Sum256(b)
	kh := sha256.Sum256([]byte(IDText(key)))
	token, e := NewToken()
	if e != nil {
		return CreatedUser{}, e
	}
	id := NewID()
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return CreatedUser{}, e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireRecentAdministrator(ctx, q, p); e != nil {
		return CreatedUser{}, e
	}
	previous, e := q.CreateUserIdempotency(ctx, db.CreateUserIdempotencyParams{ActorID: p.ActorUserID, KeyHash: kh[:]})
	if e == nil {
		if !bytes.Equal(previous.RequestHash, rh[:]) {
			return CreatedUser{}, ErrConflict
		}
		return CreatedUser{}, ErrLinkIssued
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return CreatedUser{}, e
	}
	encrypted, e := s.keys.seal("CONTACT_REFERENCE/"+IDText(id), c.Verification)
	if e != nil {
		return CreatedUser{}, e
	}
	if e = q.InsertAccount(ctx, db.InsertAccountParams{ID: id, Username: c.Username, Role: "USER", OnboardingState: "NEEDS_SETUP", CustomerVerificationRef: pgtype.Text{String: string(encrypted), Valid: true}}); e != nil {
		var pe *pgconn.PgError
		if errors.As(e, &pe) && pe.Code == "23505" {
			return CreatedUser{}, ErrConflict
		}
		return CreatedUser{}, e
	}
	if e = q.InsertZeroWallet(ctx, id); e != nil {
		return CreatedUser{}, e
	}
	now, e := q.AccountClock(ctx)
	if e != nil {
		return CreatedUser{}, e
	}
	th := token.Hash()
	if e = q.InsertPasswordToken(ctx, db.InsertPasswordTokenParams{ID: NewID(), UserID: id, TokenHash: th[:], Purpose: "INITIAL_SETUP", AuthVersion: 1, ExpiresAt: stamp(now.Time.Add(24 * time.Hour))}); e != nil {
		return CreatedUser{}, e
	}
	result := CreatedUser{User: User{ID: IDText(id), Username: c.Username, Role: "USER", IsActive: true, Onboarding: "NEEDS_SETUP"}, Wallet: credit.Wallet{Available: "0", Frozen: "0"}}
	snapshot, _ := json.Marshal(result)
	if e = q.InsertCreateUserIdempotency(ctx, db.InsertCreateUserIdempotencyParams{ID: NewID(), ActorID: p.ActorUserID, KeyHash: kh[:], RequestHash: rh[:], ResourceID: id, ResultJson: snapshot}); e != nil {
		return CreatedUser{}, e
	}
	auditMeta, _ := json.Marshal(map[string]string{"reason": c.Reason})
	if e = q.AccountAudit(ctx, db.AccountAuditParams{ID: NewID(), ActorID: p.ActorUserID, Action: "USER_CREATED", TargetID: id, RequestID: key, RedactedMeta: auditMeta}); e != nil {
		return CreatedUser{}, e
	}
	if e = commitAccount(ctx, tx); e != nil {
		return CreatedUser{}, e
	}
	result.SetupLink = s.origin + "/setup#token=" + token.Reveal()
	return result, nil
}
func (s *Service) ManagedUsers(ctx context.Context, p credit.Principal, filter UserFilter) (ManagedUserPage, error) {
	if filter.Limit == 0 {
		filter.Limit = 20
	}
	if filter.Limit < 1 || filter.Limit > 100 || len(filter.Username) > 64 || (filter.Username != "" && !usernameFilterPattern.MatchString(filter.Username)) {
		return ManagedUserPage{}, ErrInvalid
	}
	switch filter.Status {
	case "", "ACTIVE", "INACTIVE", "NEEDS_SETUP", "ENROLLMENT_REQUIRED", "READY":
	default:
		return ManagedUserPage{}, ErrInvalid
	}
	var cursor pgtype.UUID
	filtersHash := sha256.Sum256([]byte("v1\x00" + filter.Username + "\x00" + filter.Status))
	cursorContext := "ADMIN_USERS_CURSOR/" + IDText(p.ActorUserID) + "/" + hex.EncodeToString(filtersHash[:])
	if filter.Cursor != "" {
		plain, e := s.keys.open(cursorContext, []byte(filter.Cursor))
		if e != nil {
			return ManagedUserPage{}, ErrInvalid
		}
		cursor, e = ParseID(plain)
		if e != nil {
			return ManagedUserPage{}, ErrInvalid
		}
	}
	tx, q, e := s.transaction(ctx)
	if e != nil {
		return ManagedUserPage{}, e
	}
	defer rollbackAccount(tx)
	if e = credit.RequireAdministrator(ctx, q, p); e != nil {
		return ManagedUserPage{}, e
	}
	if cursor.Valid {
		u, e := q.AccountByID(ctx, cursor)
		if e != nil || u.Role != "USER" {
			return ManagedUserPage{}, ErrInvalid
		}
	}
	rows, e := q.ListManagedUsers(ctx, db.ListManagedUsersParams{UsernameFilter: filter.Username, StatusFilter: filter.Status, CursorID: cursor, PageLimit: filter.Limit + 1})
	if e != nil {
		return ManagedUserPage{}, e
	}
	result := ManagedUserPage{Items: make([]ManagedUser, 0, len(rows))}
	if len(rows) > int(filter.Limit) {
		rows = rows[:filter.Limit]
		sealed, e := s.keys.seal(cursorContext, IDText(rows[len(rows)-1].ID))
		if e != nil {
			return ManagedUserPage{}, e
		}
		next := string(sealed)
		result.NextCursor = &next
	}
	for _, u := range rows {
		result.Items = append(result.Items, ManagedUser{User: User{ID: IDText(u.ID), Username: u.Username, Role: u.Role, IsActive: u.IsActive, Onboarding: u.OnboardingState}, Wallet: credit.Wallet{Available: fmt.Sprint(u.Available), Frozen: fmt.Sprint(u.Frozen)}, BindingEpoch: u.BindingEpoch, DeviceState: u.DeviceState})
	}
	if e = commitAccount(ctx, tx); e != nil {
		return ManagedUserPage{}, e
	}
	return result, nil
}
