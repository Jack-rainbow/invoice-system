package account

import (
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// RFC6238 Appendix B SHA1 vectors, reduced from eight digits to this
// application's fixed six digits by the RFC4226 decimal modulus.
// https://www.rfc-editor.org/rfc/rfc6238#appendix-B
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret, err := ParseTOTPSecret(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890")))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		timestamp int64
		code      string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		step, err := secret.Match(test.code, test.timestamp, -1)
		if err != nil || step != test.timestamp/30 {
			t.Fatalf("RFC vector at %d: step=%d err=%v", test.timestamp, step, err)
		}
		if _, err := secret.Match(test.code, test.timestamp, step); err == nil {
			t.Fatal("same accepted time-step must be rejected")
		}
	}
}

func TestTOTPWindowAndInvalidInputs(t *testing.T) {
	secret, err := ParseTOTPSecret("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatal(err)
	}
	// RFC4226 counters 0,1,2,3 yield 755224,287082,359152,969429.
	for _, code := range []string{"755224", "287082", "359152"} {
		if _, err := secret.Match(code, 30, -1); err != nil {
			t.Fatal("adjacent ±1 step should be allowed")
		}
	}
	for _, test := range []struct {
		code      string
		now, last int64
	}{
		{"969429", 30, -1}, {"755224", 60, -1}, {"287082", -1, -1},
		{"287082", 30, -2}, {"287082", 30, 1}, {"287082", 30, 100},
		{"", 30, -1}, {"28708", 30, -1}, {"0287082", 30, -1},
		{"２８７０８２", 30, -1}, {"287082\n", 30, -1}, {"28708a", 30, -1},
	} {
		step, err := secret.Match(test.code, test.now, test.last)
		if err == nil || step != -1 {
			t.Fatalf("invalid input accepted or nonempty step: %q %d %d", test.code, test.now, test.last)
		}
	}
	if step, err := secret.Match("755224", 0, -1); err != nil || step != 0 {
		t.Fatal("Unix epoch boundary must not inspect a negative step")
	}
	var zero TOTPSecret
	if step, err := zero.Match("755224", 0, -1); err == nil || step != -1 || zero.EnrollmentSecret() != "" {
		t.Fatal("uninitialized secret cannot authenticate")
	}
}

func TestTOTPSecretCreationParsingAndRedaction(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	raw := secret.EnrollmentSecret()
	if len(raw) != 32 {
		t.Fatal("SHA1 profile requires 160 random secret bits")
	}
	parsed, err := ParseTOTPSecret(raw)
	if err != nil || parsed.EnrollmentSecret() != raw {
		t.Fatal("secret cannot be restored from protected enrollment value")
	}
	encoded, err := json.Marshal(secret)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("default JSON must not export seed material")
	}
	for _, value := range []string{fmt.Sprint(secret), fmt.Sprintf("%+v", secret), fmt.Sprintf("%#v", secret), fmt.Sprintf("%#v", &secret)} {
		if strings.Contains(value, raw) || !strings.Contains(value, "REDACTED") {
			t.Fatal("default formatting must redact MFA seeds")
		}
	}
	for _, invalid := range []string{"", strings.ToLower(raw), raw + "=", raw + "\n", raw[:31], strings.Repeat("A", 33), strings.Repeat("0", 32)} {
		got, err := ParseTOTPSecret(invalid)
		if err == nil || got.EnrollmentSecret() != "" {
			t.Fatal("noncanonical or wrong-sized seed accepted")
		}
	}
}

func TestTOTPFormattingNeverWalksSecretFields(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%d", "%b", "%o", "%x", "%X", "%f", "%s", "%q", "%#v", "%+v"} {
		if !strings.Contains(fmt.Sprintf(format, secret), "REDACTED") {
			t.Fatalf("format %s bypasses MFA seed redaction", format)
		}
	}
}
