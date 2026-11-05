package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // RFC6238 SHA1 profile, used only as HMAC, not password hashing.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrInvalidTOTPSecret = errors.New("invalid MFA secret")
	ErrInvalidTOTP       = errors.New("invalid MFA proof")
)

var seedEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// TOTPSecret is this application's RFC6238 SHA1/6-digit/30-second profile.
// The secret has 160 random bits and is exposed only for one-time enrollment or
// the server's encryption adapter, never through default JSON or formatting.
type TOTPSecret struct {
	seed        [20]byte
	initialized bool
}

func NewTOTPSecret() (TOTPSecret, error) {
	var s TOTPSecret
	if _, err := rand.Read(s.seed[:]); err != nil {
		return TOTPSecret{}, err
	}
	s.initialized = true
	return s, nil
}

func ParseTOTPSecret(input string) (TOTPSecret, error) {
	if len(input) != 32 {
		return TOTPSecret{}, ErrInvalidTOTPSecret
	}
	decoded, err := seedEncoding.DecodeString(input)
	if err != nil || len(decoded) != 20 || seedEncoding.EncodeToString(decoded) != input {
		return TOTPSecret{}, ErrInvalidTOTPSecret
	}
	var s TOTPSecret
	copy(s.seed[:], decoded)
	s.initialized = true
	return s, nil
}

func (s TOTPSecret) EnrollmentSecret() string {
	if !s.initialized {
		return ""
	}
	return seedEncoding.EncodeToString(s.seed[:])
}

func (TOTPSecret) String() string   { return "TOTPSecret[REDACTED]" }
func (TOTPSecret) GoString() string { return "TOTPSecret[REDACTED]" }
func (TOTPSecret) Format(state fmt.State, _ rune) {
	formatRedacted(state, "TOTPSecret")
}

// Match checks the current time-step and its ±1 neighbors, returning the highest
// matching step newer than lastAcceptedStep (-1 for no prior accepted step).
// All failures return -1 and a generic proof error. No clock is read internally.
//
// This method DOES NOT consume a proof. The service MUST lock the current MFA
// factor, call Match against its current last_accepted_step, and conditionally
// update that step in the same real database transaction as the auth change.
// Rate limiting, factor version, account state and challenge checks are also
// required before any service may issue a session.
func (s TOTPSecret) Match(code string, unixSeconds, lastAcceptedStep int64) (int64, error) {
	if !s.initialized || unixSeconds < 0 || lastAcceptedStep < -1 || len(code) != 6 {
		return -1, ErrInvalidTOTP
	}
	for i := range code {
		if code[i] < '0' || code[i] > '9' {
			return -1, ErrInvalidTOTP
		}
	}
	current := unixSeconds / 30
	matched := int64(-1)
	for delta := int64(-1); delta <= 1; delta++ {
		step := current + delta
		if step < 0 {
			continue
		}
		expected := s.codeAt(step)
		if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1 && step > lastAcceptedStep {
			matched = step
		}
	}
	if matched < 0 {
		return -1, ErrInvalidTOTP
	}
	return matched, nil
}

// RFC4226 dynamic truncation, used by RFC6238 section 4.2.
// https://www.rfc-editor.org/rfc/rfc6238#section-4.2
func (s TOTPSecret) codeAt(step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, s.seed[:])
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}
