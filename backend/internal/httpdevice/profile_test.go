package httpdevice

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"invoice.local/system/backend/internal/devicekey"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Independent test signer uses RFC signature-base text and stdlib ECDSA,
// never the production verifier or the browser serializer.
func signedTestRequest(t *testing.T, method string, body []byte, idempotency ...string) (*http.Request, devicekey.PublicKey, Expectation, time.Time) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	jwk, _ := json.Marshal(devicekey.PublicJWK{Crv: "P-256", Kty: "EC", X: b64(key.X.FillBytes(make([]byte, 32))), Y: b64(key.Y.FillBytes(make([]byte, 32)))})
	public, e := devicekey.ParsePublicJWK(jwk)
	if e != nil {
		t.Fatal(e)
	}
	nonce := b64(make([]byte, 32))
	hash, _ := NonceHash(nonce)
	now := time.Now().UTC().Truncate(time.Second)
	exp := Expectation{KeyID: "11111111-1111-4111-8111-111111111111", ContextID: "22222222-2222-4222-8222-222222222222", NonceHash: hash, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	target := "/api/v1/me?format=pdf&x=%2B&x=2"
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.Header.Set("x-device-id", exp.KeyID)
	r.Header.Set("x-session-generation", exp.ContextID)
	fields := []string{"@method", "@target-uri", "x-device-id", "x-session-generation"}
	lines := []string{`"@method": ` + method, `"@target-uri": https://invoice.example` + target, `"x-device-id": ` + exp.KeyID, `"x-session-generation": ` + exp.ContextID}
	if method != "GET" {
		digest := sha256.Sum256(body)
		r.Header.Set("content-type", "application/json")
		r.Header.Set("content-digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		fields = append(fields, "content-type", "content-digest")
		lines = append(lines, `"content-type": application/json`, `"content-digest": `+r.Header.Get("content-digest"))
	}
	if len(idempotency) > 0 {
		r.Header.Set("Idempotency-Key", idempotency[0])
		exp.RequireIdempotency = true
		fields = append(fields, "idempotency-key")
		lines = append(lines, `"idempotency-key": `+idempotency[0])
	}
	for i := range fields {
		fields[i] = `"` + fields[i] + `"`
	}
	params := fmt.Sprintf("(%s);created=%d;expires=%d;nonce=\"%s\";keyid=\"%s\";alg=\"ecdsa-p256-sha256\";tag=\"invoice-device-v1\"", strings.Join(fields, " "), now.Unix(), now.Add(30*time.Second).Unix(), nonce, exp.KeyID)
	r.Header.Set("Signature-Input", "sig1="+params)
	lines = append(lines, `"@signature-params": `+params)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	rr, ss, e := ecdsa.Sign(rand.Reader, key, digest[:])
	if e != nil {
		t.Fatal(e)
	}
	signature := append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(signature)+":")
	return r, public, exp, now
}

func TestProfileIdempotencyCoverage(t *testing.T) {
	r, key, exp, now := signedTestRequest(t, "POST", []byte(`{"amount":"1"}`), "33333333-3333-4333-8333-333333333333")
	if Verify(r, []byte(`{"amount":"1"}`), "https://invoice.example", key, exp, now) != nil {
		t.Fatal("valid idempotent proof rejected")
	}
	r.Header.Set("Idempotency-Key", "44444444-4444-4444-8444-444444444444")
	if Verify(r, []byte(`{"amount":"1"}`), "https://invoice.example", key, exp, now) == nil {
		t.Fatal("changed idempotency key accepted")
	}
	plain, pub, expected, current := signedTestRequest(t, "POST", []byte(`{}`))
	expected.RequireIdempotency = true
	if Verify(plain, []byte(`{}`), "https://invoice.example", pub, expected, current) == nil {
		t.Fatal("required idempotency header missing")
	}
}
func TestProfileStandardVerificationAndTampering(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			var body []byte
			if method != "GET" {
				body = []byte(`{"amount":"1"}`)
			}
			r, key, exp, now := signedTestRequest(t, method, body)
			if e := Verify(r, body, "https://invoice.example", key, exp, now); e != nil {
				t.Fatal("valid standard signature rejected", e)
			}
			cases := map[string]func(*http.Request){
				"changed query":   func(r *http.Request) { r.URL.RawQuery = "x=2&format=pdf&x=%2B" },
				"changed method":  func(r *http.Request) { r.Method = "PUT" },
				"changed device":  func(r *http.Request) { r.Header.Set("X-Device-ID", "other") },
				"duplicate input": func(r *http.Request) { r.Header.Add("Signature-Input", r.Header.Get("Signature-Input")) },
				"duplicate parameter": func(r *http.Request) {
					r.Header.Set("Signature-Input", r.Header.Get("Signature-Input")+`;tag="invoice-device-v1"`)
				},
				"extra signature": func(r *http.Request) { r.Header.Set("Signature", r.Header.Get("Signature")+", sig2=:AA==:") },
				"wrong algorithm": func(r *http.Request) {
					r.Header.Set("Signature-Input", strings.Replace(r.Header.Get("Signature-Input"), "ecdsa-p256-sha256", "hmac-sha256", 1))
				},
				"wrong context": func(r *http.Request) { r.Header.Set("X-Session-Generation", "other") },
			}
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					copy := r.Clone(t.Context())
					copy.Header = r.Header.Clone()
					mutate(copy)
					if Verify(copy, body, "https://invoice.example", key, exp, now) == nil {
						t.Fatal("tampering accepted")
					}
				})
			}
			if Verify(r, body, "https://attacker.example", key, exp, now) == nil {
				t.Fatal("foreign origin accepted")
			}
			if Verify(r, body, "https://invoice.example", key, exp, now.Add(61*time.Second)) == nil {
				t.Fatal("expired proof accepted")
			}
			if method != "GET" && Verify(r, []byte(`{"amount":"9"}`), "https://invoice.example", key, exp, now) == nil {
				t.Fatal("body mutation accepted")
			}
		})
	}
}
