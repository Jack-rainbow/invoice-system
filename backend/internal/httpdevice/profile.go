// Package httpdevice restricts the maintained RFC9421 verifier to the fixed
// invoice profile. It never consumes a nonce or confers database authorization.
package httpdevice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/dadrus/httpsig"
	"github.com/dunglas/httpsfv"
	"invoice.local/system/backend/internal/devicekey"
)

var ErrProof = errors.New("invalid device proof")

type Proof struct {
	Nonce   string
	KeyID   string
	Created int64
	Expires int64
}
type Expectation struct {
	KeyID              string
	ContextID          string
	NonceHash          [32]byte
	IssuedAt           time.Time
	ExpiresAt          time.Time
	RequireIdempotency bool
}

func NonceHash(value string) ([32]byte, error) {
	raw, e := base64.RawURLEncoding.Strict().DecodeString(value)
	if e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != value {
		return [32]byte{}, ErrProof
	}
	return sha256.Sum256(raw), nil
}

func oneHeader(r *http.Request, name string) (string, error) {
	v := r.Header.Values(name)
	if len(v) != 1 || len(v[0]) > 4096 {
		return "", ErrProof
	}
	return v[0], nil
}
func dictionary(value string) (*httpsfv.Dictionary, error) {
	d, e := httpsfv.UnmarshalDictionary([]string{value})
	if e != nil {
		return nil, ErrProof
	}
	canonical, e := httpsfv.Marshal(d)
	// Canonical SFV also rejects overwritten duplicate members/parameters and
	// keeps interpretation identical at the profile and library boundaries.
	if e != nil || canonical != value {
		return nil, ErrProof
	}
	return d, nil
}
func readInput(r *http.Request) (Proof, httpsfv.InnerList, error) {
	raw, e := oneHeader(r, "Signature-Input")
	if e != nil {
		return Proof{}, httpsfv.InnerList{}, e
	}
	d, e := dictionary(raw)
	if e != nil || len(d.Names()) != 1 || d.Names()[0] != "sig1" {
		return Proof{}, httpsfv.InnerList{}, ErrProof
	}
	member, _ := d.Get("sig1")
	list, ok := member.(httpsfv.InnerList)
	if !ok || list.Params == nil || len(list.Params.Names()) != 6 {
		return Proof{}, list, ErrProof
	}
	p := Proof{}
	for _, name := range list.Params.Names() {
		v, _ := list.Params.Get(name)
		switch name {
		case "created":
			p.Created, ok = v.(int64)
		case "expires":
			p.Expires, ok = v.(int64)
		case "nonce":
			p.Nonce, ok = v.(string)
		case "keyid":
			p.KeyID, ok = v.(string)
		case "alg":
			ok = v == "ecdsa-p256-sha256"
		case "tag":
			ok = v == "invoice-device-v1"
		default:
			ok = false
		}
		if !ok {
			return Proof{}, list, ErrProof
		}
	}
	if _, e := NonceHash(p.Nonce); e != nil {
		return Proof{}, list, e
	}
	return p, list, nil
}
func ReadProof(r *http.Request) (Proof, error) { p, _, e := readInput(r); return p, e }

type resolver struct{ key httpsig.Key }

func (s resolver) ResolveKey(_ context.Context, id string) (httpsig.Key, error) {
	if id != s.key.KeyID {
		return httpsig.Key{}, ErrProof
	}
	return s.key, nil
}

func Verify(r *http.Request, body []byte, origin string, key devicekey.PublicKey, expected Expectation, now time.Time) error {
	if len(body) > 64<<10 || (r.Method != "GET" && r.Method != "HEAD" && r.Method != "POST" && r.Method != "PATCH" && r.Method != "PUT" && r.Method != "DELETE" && r.Method != "OPTIONS") {
		return ErrProof
	}
	p, list, e := readInput(r)
	if e != nil {
		return ErrProof
	}
	hash, e := NonceHash(p.Nonce)
	if e != nil || hash != expected.NonceHash || p.KeyID != expected.KeyID || p.Created < expected.IssuedAt.Unix() || p.Created > now.Unix() || p.Expires <= now.Unix() || p.Expires > expected.ExpiresAt.Unix() || p.Expires <= p.Created || p.Expires-p.Created > 60 {
		return ErrProof
	}
	device, e := oneHeader(r, "X-Device-ID")
	if e != nil || device != expected.KeyID {
		return ErrProof
	}
	generation, e := oneHeader(r, "X-Session-Generation")
	if e != nil || generation != expected.ContextID {
		return ErrProof
	}
	components := []string{"@method", "@target-uri", "x-device-id", "x-session-generation"}
	if r.Method == "GET" || r.Method == "HEAD" {
		if len(body) != 0 || r.ContentLength > 0 || len(r.Header.Values("Content-Type")) != 0 || len(r.Header.Values("Content-Digest")) != 0 {
			return ErrProof
		}
	} else {
		ct, e := oneHeader(r, "Content-Type")
		if e != nil || ct != "application/json" {
			return ErrProof
		}
		digest, e := oneHeader(r, "Content-Digest")
		sum := sha256.Sum256(body)
		if e != nil || digest != "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":" {
			return ErrProof
		}
		components = append(components, "content-type", "content-digest")
	}
	if len(r.Header.Values("Idempotency-Key")) != 0 || expected.RequireIdempotency {
		value, e := oneHeader(r, "Idempotency-Key")
		if e != nil || len(value) != 36 {
			return ErrProof
		}
		components = append(components, "idempotency-key")
	}
	if len(list.Items) != len(components) {
		return ErrProof
	}
	for i, v := range list.Items {
		if v.Value != components[i] || v.Params == nil || len(v.Params.Names()) != 0 {
			return ErrProof
		}
	}
	signature, e := oneHeader(r, "Signature")
	if e != nil {
		return ErrProof
	}
	d, e := dictionary(signature)
	if e != nil || len(d.Names()) != 1 || d.Names()[0] != "sig1" {
		return ErrProof
	}
	m, _ := d.Get("sig1")
	sig, ok := m.(httpsfv.Item)
	if !ok || sig.Params == nil || len(sig.Params.Names()) != 0 {
		return ErrProof
	}
	rawSig, ok := sig.Value.([]byte)
	if !ok || len(rawSig) != 64 {
		return ErrProof
	}
	escaped := r.URL.EscapedPath()
	if path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(escaped, "//") || strings.ContainsAny(escaped, "\\#") {
		return ErrProof
	}
	lower := strings.ToLower(escaped)
	for _, bad := range []string{"%2e", "%2f", "%5c"} {
		if strings.Contains(lower, bad) {
			return ErrProof
		}
	}
	public, e := url.Parse(origin + r.URL.RequestURI())
	if e != nil || public.Scheme != "https" || public.User != nil || public.Fragment != "" {
		return ErrProof
	}
	request := r.Clone(r.Context())
	request.URL = public
	request.Host = public.Host
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	verifier, e := httpsig.NewVerifier(resolver{httpsig.Key{KeyID: expected.KeyID, Key: key.HTTPVerificationKey(), Algorithm: httpsig.EcdsaP256Sha256}}, httpsig.WithRequiredComponents(components...), httpsig.WithRequiredTag("invoice-device-v1"), httpsig.WithMaxAge(60*time.Second), httpsig.WithValidityTolerance(5*time.Second), httpsig.WithValidateAllSignatures())
	// The library's NonceChecker is invoked before cryptographic verification.
	// Deliberately keep it side-effect free; the owning transaction consumes the
	// nonce only after this function succeeds and current authorization is checked.
	if e != nil || verifier.Verify(httpsig.MessageFromRequest(request)) != nil {
		return ErrProof
	}
	return nil
}
