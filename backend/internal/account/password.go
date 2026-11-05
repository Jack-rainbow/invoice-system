package account

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemoryKiB   = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 1
	passwordSaltBytes   = 16
	passwordHashBytes   = 32
	maximumPHCBytes     = 128
)

var (
	ErrInvalidPassword     = errors.New("invalid password")
	ErrInvalidPasswordHash = errors.New("invalid password hash")
	ErrInvalidHasher       = errors.New("invalid password hasher")
	ErrPasswordHashFailure = errors.New("password hashing failed")
)

// PasswordHasher bounds concurrent Argon2id work per instance. Share one
// instance across the service process; separate instances have separate limits.
type PasswordHasher struct {
	slots chan struct{}
}

// NewPasswordHasher caps in-flight Argon2id operations at 1..8. Production
// capacity must account for all hasher instances and their 64 MiB per call.
func NewPasswordHasher(maxConcurrent int) (*PasswordHasher, error) {
	if maxConcurrent < 1 || maxConcurrent > 8 {
		return nil, ErrInvalidHasher
	}
	return &PasswordHasher{slots: make(chan struct{}, maxConcurrent)}, nil
}

// PasswordHash is an opaque database value. Call StoredPHC explicitly at the
// persistence boundary; ordinary formatting and JSON never expose the PHC.
type PasswordHash struct {
	phc string
}

func (h PasswordHash) StoredPHC() string { return h.phc }
func (PasswordHash) String() string      { return "PasswordHash[REDACTED]" }
func (PasswordHash) GoString() string    { return "PasswordHash[REDACTED]" }
func (PasswordHash) Format(state fmt.State, _ rune) {
	formatRedacted(state, "PasswordHash")
}

func validPassword(password string) bool {
	if len(password) > 512 || !utf8.ValidString(password) {
		return false
	}
	runes := utf8.RuneCountInString(password)
	return runes >= 12 && runes <= 128
}

func (h *PasswordHasher) acquire(ctx context.Context) error {
	if h == nil || h.slots == nil {
		return ErrInvalidHasher
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.slots <- struct{}{}:
		// Cancellation may race with an available slot. Release it before work.
		if err := ctx.Err(); err != nil {
			<-h.slots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Hash uses Argon2id v19 with a new random 16-byte salt and the production
// parameters. Cancellation can stop queueing; once Argon2 starts, the slot is
// held through completion because IDKey itself has no cancellation hook.
func (h *PasswordHasher) Hash(ctx context.Context, password string) (PasswordHash, error) {
	if !validPassword(password) {
		return PasswordHash{}, ErrInvalidPassword
	}
	if err := h.acquire(ctx); err != nil {
		return PasswordHash{}, err
	}
	defer func() { <-h.slots }()
	var salt [passwordSaltBytes]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return PasswordHash{}, ErrPasswordHashFailure
	}
	if err := ctx.Err(); err != nil {
		return PasswordHash{}, err
	}
	key := argon2.IDKey([]byte(password), salt[:], passwordIterations, passwordMemoryKiB, passwordParallelism, passwordHashBytes)
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return PasswordHash{}, err
	}
	phc := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		passwordMemoryKiB, passwordIterations, passwordParallelism,
		base64.RawStdEncoding.EncodeToString(salt[:]),
		base64.RawStdEncoding.EncodeToString(key))
	return PasswordHash{phc: phc}, nil
}

// Verify accepts only the supported bounded PHC profile. An invalid stored
// value is rejected before any Argon2 allocation. For a valid value, the
// computed and stored 32-byte hashes are compared in constant time.
func (h *PasswordHasher) Verify(ctx context.Context, password, storedPHC string) (bool, error) {
	if !validPassword(password) {
		return false, ErrInvalidPassword
	}
	salt, expected, err := parsePasswordPHC(storedPHC)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer func() { <-h.slots }()
	key := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemoryKiB, passwordParallelism, passwordHashBytes)
	defer clear(key)
	matched := subtle.ConstantTimeCompare(key, expected) == 1
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return matched, nil
}

func parsePasswordPHC(phc string) ([]byte, []byte, error) {
	const prefix = "$argon2id$v=19$m=65536,t=3,p=1$"
	const minimumPHCBytes = len(prefix) + 22 + 1 + 43
	if len(phc) > maximumPHCBytes || len(phc) < minimumPHCBytes || phc[:len(prefix)] != prefix {
		return nil, nil, ErrInvalidPasswordHash
	}
	remainder := phc[len(prefix):]
	separator := -1
	for i := range len(remainder) {
		if remainder[i] == '$' {
			if separator != -1 {
				return nil, nil, ErrInvalidPasswordHash
			}
			separator = i
		}
	}
	if separator < 0 || separator < base64.RawStdEncoding.EncodedLen(passwordSaltBytes) ||
		separator > base64.RawStdEncoding.EncodedLen(32) ||
		len(remainder)-separator-1 != base64.RawStdEncoding.EncodedLen(passwordHashBytes) {
		return nil, nil, ErrInvalidPasswordHash
	}
	saltText, hashText := remainder[:separator], remainder[separator+1:]
	encoding := base64.RawStdEncoding.Strict()
	salt, saltErr := encoding.DecodeString(saltText)
	expected, hashErr := encoding.DecodeString(hashText)
	if saltErr != nil || hashErr != nil || len(salt) < passwordSaltBytes || len(salt) > 32 ||
		len(expected) != passwordHashBytes ||
		base64.RawStdEncoding.EncodeToString(salt) != saltText ||
		base64.RawStdEncoding.EncodeToString(expected) != hashText {
		return nil, nil, ErrInvalidPasswordHash
	}
	return salt, expected, nil
}
