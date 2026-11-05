package devicekey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"

	"invoice.local/system/backend/internal/strictjson"
)

var ErrInvalidKey = errors.New("invalid P-256 public key")
var ErrInvalidSignature = errors.New("invalid P-256 signature")

// PublicJWK includes only the RFC7638 required public EC members. The order is
// deliberately fixed to the RFC lexicographic order for the thumbprint input.
type PublicJWK struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type PublicKey struct {
	Thumbprint string
	key        *ecdsa.PublicKey
}

// HTTPVerificationKey supplies a defensive copy to the RFC9421 verifier.
func (k PublicKey) HTTPVerificationKey() *ecdsa.PublicKey {
	if k.key == nil {
		return nil
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).Set(k.key.X), Y: new(big.Int).Set(k.key.Y)}
}

func ParsePublicJWK(raw []byte) (PublicKey, error) {
	var jwk PublicJWK
	if err := strictjson.Decode(bytes.NewReader(raw), &jwk, 4096); err != nil {
		return PublicKey{}, ErrInvalidKey
	}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return PublicKey{}, ErrInvalidKey
	}
	x, err := coordinate(jwk.X)
	if err != nil {
		return PublicKey{}, err
	}
	y, err := coordinate(jwk.Y)
	if err != nil {
		return PublicKey{}, err
	}
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return PublicKey{}, ErrInvalidKey
	}
	canonical, err := json.Marshal(jwk)
	if err != nil {
		return PublicKey{}, ErrInvalidKey
	}
	digest := sha256.Sum256(canonical)
	return PublicKey{Thumbprint: base64.RawURLEncoding.EncodeToString(digest[:]), key: &ecdsa.PublicKey{Curve: curve, X: x, Y: y}}, nil
}

func coordinate(value string) (*big.Int, error) {
	if len(value) != 43 {
		return nil, ErrInvalidKey
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, ErrInvalidKey
	}
	return new(big.Int).SetBytes(decoded), nil
}

// VerifyP256 verifies a standard SHA256 ECDSA signature in IEEE P1363 r||s
// encoding. This primitive does not confer session or device authorization.
func VerifyP256(key PublicKey, message, signature []byte) error {
	if key.key == nil || len(signature) != 64 {
		return ErrInvalidSignature
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	digest := sha256.Sum256(message)
	if !ecdsa.Verify(key.key, digest[:], r, s) {
		return ErrInvalidSignature
	}
	return nil
}
