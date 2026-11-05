package account

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOpaqueTokenRoundTripAndRedaction(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	raw := token.Reveal()
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 || len(raw) != 43 {
		t.Fatal("token must carry exactly 256 random bits")
	}
	want := sha256.Sum256(decoded)
	got, err := ParseToken(raw)
	if err != nil || got != want || token.Hash() != want {
		t.Fatal("stored token digest must match the decoded random bytes")
	}
	encoded, err := json.Marshal(token)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("default JSON must not export token material")
	}
	for _, formatted := range []string{fmt.Sprint(token), fmt.Sprintf("%+v", token), fmt.Sprintf("%#v", token), fmt.Sprint(&token), fmt.Sprintf("%#v", &token)} {
		if strings.Contains(formatted, raw) || !strings.Contains(formatted, "REDACTED") {
			t.Fatal("default formatting must redact tokens")
		}
	}
	other, err := NewToken()
	if err != nil || other.Hash() == token.Hash() {
		t.Fatal("independently created tokens must not reuse material")
	}
}

func TestParseTokenRejectsMalformedOrNoncanonicalInput(t *testing.T) {
	canonical := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, input := range []string{"", canonical + "=", canonical[:42], canonical + "A", canonical + "\n", " " + canonical, canonical[:42] + "B", strings.Repeat("界", 43)} {
		got, err := ParseToken(input)
		if err == nil || got != ([32]byte{}) || strings.Contains(err.Error(), input) && input != "" {
			t.Fatal("malformed token must return a zero digest and a generic error")
		}
	}
}

func TestZeroTokenCannotBecomeCredential(t *testing.T) {
	var zero Token
	if zero.Reveal() != "" || zero.Hash() != ([32]byte{}) {
		t.Fatal("uninitialized token must stay empty")
	}
}

func TestOpaqueTokenFormattingNeverWalksSecretFields(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%d", "%b", "%o", "%x", "%X", "%f", "%s", "%q", "%#v", "%+v"} {
		if !strings.Contains(fmt.Sprintf(format, token), "REDACTED") {
			t.Fatalf("format %s bypasses credential redaction", format)
		}
	}
}
