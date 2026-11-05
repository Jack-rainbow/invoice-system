package account

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPasswordHashAndVerifyProductionParameters(t *testing.T) {
	hasher, err := NewPasswordHasher(2)
	if err != nil {
		t.Fatal(err)
	}
	password := "  密码e\u0301与passphrase  "
	stored, err := hasher.Hash(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	phc := stored.StoredPHC()
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=1$") || len(phc) > 128 {
		t.Fatal("hash must use bounded production Argon2id PHC encoding")
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || len(parts[4]) != 22 || len(parts[5]) != 43 {
		t.Fatal("hash must have a 16-byte salt and 32-byte output")
	}
	ok, err := hasher.Verify(context.Background(), password, phc)
	if err != nil || !ok {
		t.Fatal("correct password must verify")
	}
	for _, wrong := range []string{strings.TrimSpace(password), "  密码é与passphrase  ", "  密码e\u0301与passphrase ! "} {
		ok, err = hasher.Verify(context.Background(), wrong, phc)
		if err != nil || ok {
			t.Fatal("normalization, trimming, or a changed character must not verify")
		}
	}
	other, err := hasher.Hash(context.Background(), password)
	if err != nil || other.StoredPHC() == phc {
		t.Fatal("independent hashes must use independent random salts")
	}
	if string(encodedJSON(t, stored)) != "{}" {
		t.Fatal("default JSON must not reveal PHC")
	}
	for _, value := range []any{stored, &stored} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			printed := fmt.Sprintf(format, value)
			if strings.Contains(printed, phc) || !strings.Contains(printed, "REDACTED") {
				t.Fatal("formatting must not reveal PHC")
			}
		}
	}
}

func encodedJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestPasswordLengthAndUTF8Boundaries(t *testing.T) {
	hasher, err := NewPasswordHasher(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"", strings.Repeat("a", 11), strings.Repeat("a", 129), strings.Repeat("界", 128) + "a", string([]byte{'a', 0xff}) + strings.Repeat("a", 11)} {
		_, err := hasher.Hash(context.Background(), password)
		if err != ErrInvalidPassword || strings.Contains(err.Error(), password) && password != "" {
			t.Fatal("invalid password must be rejected without disclosure")
		}
	}
	for _, password := range []string{strings.Repeat("a", 12), strings.Repeat("😀", 128)} {
		if _, err := hasher.Hash(context.Background(), password); err != nil {
			t.Fatal("valid 12-code-point and 512-byte boundaries must hash")
		}
	}
}

func TestPasswordPHCRejectsMalformedAndResourceAmplification(t *testing.T) {
	hasher, err := NewPasswordHasher(1)
	if err != nil {
		t.Fatal(err)
	}
	valid := "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	parts := strings.Split(valid, "$")
	malformed := []string{
		"", strings.Repeat("A", 10000),
		strings.Replace(valid, "argon2id", "argon2i", 1),
		strings.Replace(valid, "v=19", "v=16", 1),
		strings.Replace(valid, "v=19", "v=019", 1),
		strings.Replace(valid, "m=65536", "m=4294967295", 1),
		strings.Replace(valid, "m=65536", "m=065536", 1),
		strings.Replace(valid, "t=3", "t=4294967295", 1),
		strings.Replace(valid, "p=1", "p=255", 1),
		strings.Replace(valid, "m=65536,t=3,p=1", "t=3,m=65536,p=1", 1),
		strings.Replace(valid, parts[4], parts[4]+"=", 1),
		strings.Replace(valid, parts[4], parts[4][:21]+"B", 1),
		strings.Replace(valid, parts[4], parts[4][:21], 1),
		strings.Replace(valid, parts[5], parts[5]+"=", 1),
		strings.Replace(valid, parts[5], parts[5][:42]+"B", 1),
		strings.Replace(valid, parts[5], parts[5][:42], 1),
		valid + "$extra",
	}
	for _, phc := range malformed {
		ok, err := hasher.Verify(context.Background(), strings.Repeat("a", 12), phc)
		if ok || err != ErrInvalidPasswordHash || strings.Contains(err.Error(), phc) && phc != "" {
			t.Fatal("malformed PHC must fail before Argon2 allocation with a generic error")
		}
	}
	longPHC := strings.Repeat("A", 10000)
	if allocations := testing.AllocsPerRun(100, func() {
		_, _, _ = parsePasswordPHC(longPHC)
	}); allocations != 0 {
		t.Fatal("oversized PHC must fail before allocation")
	}
}

func TestPasswordConcurrencyLimitAndCancellation(t *testing.T) {
	for _, limit := range []int{0, 9} {
		if _, err := NewPasswordHasher(limit); err == nil {
			t.Fatal("concurrency limit must be between one and eight")
		}
	}
	hasher, err := NewPasswordHasher(1)
	if err != nil {
		t.Fatal(err)
	}
	hasher.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hasher.Hash(ctx, strings.Repeat("a", 12)); err != context.Canceled {
		t.Fatal("queued hashing must honor cancellation")
	}
	if ok, err := hasher.Verify(ctx, strings.Repeat("a", 12), "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); ok || err != context.Canceled {
		t.Fatal("queued verification must honor cancellation")
	}
	if len(hasher.slots) != 1 {
		t.Fatal("canceled work must not consume or release another operation's slot")
	}
	<-hasher.slots
}
