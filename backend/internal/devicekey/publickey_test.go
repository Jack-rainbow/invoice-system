package devicekey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const generatorJWK = `{"crv":"P-256","kty":"EC","x":"axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY","y":"T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"}`

func TestPublicKeyUsesCanonicalRFC7638Thumbprint(t *testing.T) {
	key, err := ParsePublicJWK([]byte(generatorJWK))
	if err != nil {
		t.Fatal(err)
	}
	if key.Thumbprint != "xx0BcA-wMohw8atYDJOe6peGModklG2wRHBlXHMvl0M" {
		t.Fatalf("thumbprint=%s", key.Thumbprint)
	}
	const reverseOrder = ` { "y":"T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU", "x":"axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY", "kty":"EC", "crv":"P-256" } `
	if reverseOrder == generatorJWK {
		t.Fatal("fixture must exercise a different input order")
	}
	reordered, err := ParsePublicJWK([]byte(reverseOrder))
	if err != nil || reordered.Thumbprint != key.Thumbprint {
		t.Fatalf("order affected key: %v", err)
	}
}

func TestRejectsPrivateUnknownAliasAndInvalidCurvePoints(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(generatorJWK, `"kty":"EC"`, `"kty":"RSA"`, 1),
		strings.Replace(generatorJWK, `"P-256"`, `"P-384"`, 1),
		strings.Replace(generatorJWK, `"kty":"EC"`, `"KTY":"EC"`, 1),
		strings.Replace(generatorJWK, `"kty":"EC"`, `"kty":"EC","d":"private"`, 1),
		strings.Replace(generatorJWK, `"kty":"EC"`, `"kty":"EC","alg":"HS256"`, 1),
		strings.Replace(generatorJWK, `"kty":"EC"`, `"kty":"EC","kty":"EC"`, 1),
		strings.Replace(generatorJWK, `axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY`, `AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA`, 1),
		strings.Replace(generatorJWK, `axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY`, `axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY=`, 1),
		`{}`, `null`, generatorJWK + ` {}`,
	} {
		if _, err := ParsePublicJWK([]byte(raw)); err == nil {
			t.Fatalf("invalid key accepted: %.90s", raw)
		}
	}
}

func TestVerifyP256UsesRawSignatureAndRejectsChangedInputs(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk := PublicJWK{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(priv.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(priv.Y.FillBytes(make([]byte, 32)))}
	b, _ := json.Marshal(jwk)
	key, err := ParsePublicJWK(b)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("trusted message bytes")
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	if err := VerifyP256(key, message, sig); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{nil, make([]byte, 64), sig[:63], append(append([]byte{}, sig...), 0)} {
		if err := VerifyP256(key, message, changed); err == nil {
			t.Fatal("invalid signature accepted")
		}
	}
	if err := VerifyP256(key, []byte("modified message bytes"), sig); err == nil {
		t.Fatal("modified message accepted")
	}
	other, err := ParsePublicJWK([]byte(generatorJWK))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyP256(other, message, sig); err == nil {
		t.Fatal("wrong public key accepted")
	}
	der, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyP256(key, message, der); err == nil {
		t.Fatal("DER signature incorrectly accepted as r||s")
	}
}

func TestVerifiesWebCryptoGeneratedVectors(t *testing.T) {
	data, err := os.ReadFile("../../../frontend/tests/device/vectors/webcrypto-p256.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		PublicJWK  json.RawMessage `json:"public_jwk"`
		Thumbprint string          `json:"thumbprint"`
		Vectors    []struct {
			Method    string `json:"method"`
			Base      string `json:"signature_base_utf8_base64"`
			Signature string `json:"signature_base64"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := ParsePublicJWK(fixture.PublicJWK)
	if err != nil {
		t.Fatal(err)
	}
	if key.Thumbprint != fixture.Thumbprint {
		t.Fatal("Go and WebCrypto thumbprints differ")
	}
	if len(fixture.Vectors) != 4 {
		t.Fatalf("expected GET/POST/PATCH/DELETE vectors, got %d", len(fixture.Vectors))
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Method, func(t *testing.T) {
			message, err := base64.StdEncoding.DecodeString(vector.Base)
			if err != nil {
				t.Fatal(err)
			}
			sig, err := base64.StdEncoding.DecodeString(vector.Signature)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyP256(key, message, sig); err != nil {
				t.Fatalf("WebCrypto signature rejected: %v", err)
			}
			message = append(message, '!')
			if err := VerifyP256(key, message, sig); err == nil {
				t.Fatal("changed signature base accepted")
			}
		})
	}
}
