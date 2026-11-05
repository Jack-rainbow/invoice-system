package account

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyringSealingAndCSRF(t *testing.T) {
	file := filepath.Join(t.TempDir(), "keyring.json")
	if e := GenerateKeyringFile(file); e != nil {
		t.Fatal("generate keyring")
	}
	if e := GenerateKeyringFile(file); e == nil {
		t.Fatal("must not overwrite a keyring")
	}
	k, e := LoadKeyring(file)
	if e != nil {
		t.Fatal("load private keyring")
	}
	a, e := k.seal("MFA/user-a/factor-a/1", "private seed")
	if e != nil {
		t.Fatal(e)
	}
	b, e := k.seal("MFA/user-a/factor-a/1", "private seed")
	if e != nil || string(a) == string(b) {
		t.Fatal("fresh random nonce required")
	}
	if p, e := k.open("MFA/user-a/factor-a/1", a); e != nil || p != "private seed" {
		t.Fatal("round trip")
	}
	for _, context := range []string{"MFA/user-b/factor-a/1", "MFA/user-a/factor-b/1", "MFA/user-a/factor-a/2", "CONTACT/user-a"} {
		if _, e := k.open(context, a); e == nil {
			t.Fatal("cross-context decryption accepted")
		}
	}
	tampered := append([]byte(nil), a...)
	tampered[len(tampered)-5] ^= 1
	if _, e := k.open("MFA/user-a/factor-a/1", tampered); e == nil {
		t.Fatal("ciphertext tampering accepted")
	}
	csrf := k.CSRF("session", "cookie-a")
	if !k.VerifyCSRF(csrf, "session", "cookie-a") || k.VerifyCSRF(csrf, "pre", "cookie-a") || k.VerifyCSRF(csrf, "session", "cookie-b") || k.VerifyCSRF(csrf+"x", "session", "cookie-a") {
		t.Fatal("CSRF context binding")
	}
	for _, v := range []string{"%v", "%#v", "%+v", "%s", "%x"} {
		if !strings.Contains(fmt.Sprintf(v, k), "REDACTED") {
			t.Fatal("keyring formatting must redact")
		}
	}
	if e := os.Chmod(file, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadKeyring(file); e == nil {
		t.Fatal("public keyring accepted")
	}
}

func TestUsernameCanonicalization(t *testing.T) {
	if got := normalizeUsername("  AbC.User-1  "); got != "abc.user-1" {
		t.Fatal("ASCII case and whitespace normalization")
	}
	if usernamePattern.MatchString(normalizeUsername("Kelvin")) {
		t.Fatal("non-ASCII alias must not become an ASCII username")
	}
}
