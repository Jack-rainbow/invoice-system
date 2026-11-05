// Package account provides the account primitives used by persistent services.
// These helpers alone do not grant a session or consume any credential.
package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

var ErrInvalidToken = errors.New("invalid credential token")

// Token carries 256 random bits. Its zero value cannot be used as a credential.
// Material is exposed only through an explicit Reveal call at issuance; default
// JSON and formatting do not expose it. Persist Hash, never Reveal's value.
type Token struct {
	raw         [32]byte
	initialized bool
}

func NewToken() (Token, error) {
	var t Token
	if _, err := rand.Read(t.raw[:]); err != nil {
		return Token{}, err
	}
	t.initialized = true
	return t, nil
}

func (t Token) Reveal() string {
	if !t.initialized {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(t.raw[:])
}

func (t Token) Hash() [32]byte {
	if !t.initialized {
		return [32]byte{}
	}
	return sha256.Sum256(t.raw[:])
}

func (Token) String() string   { return "Token[REDACTED]" }
func (Token) GoString() string { return "Token[REDACTED]" }
func (Token) Format(state fmt.State, _ rune) {
	formatRedacted(state, "Token")
}

// Stringer alone does not cover non-string formatting verbs such as %d; fmt
// otherwise recursively walks even unexported struct fields. Cover every verb.
func formatRedacted(state fmt.State, kind string) {
	_, _ = io.WriteString(state, kind+"[REDACTED]")
}

// ParseToken accepts exactly the issuance encoding, rejecting alternate padding,
// whitespace and unused-bit aliases. It returns only the database lookup hash;
// the caller still checks purpose, expiry, account and single-use state in a Tx.
func ParseToken(input string) ([32]byte, error) {
	if len(input) != 43 {
		return [32]byte{}, ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(input)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != input {
		return [32]byte{}, ErrInvalidToken
	}
	return sha256.Sum256(raw), nil
}
