package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"invoice.local/system/backend/internal/strictjson"
)

var ErrKeyring = errors.New("invalid server keyring")
var keyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

type Keyring struct {
	active string
	keys   map[string][32]byte
}

func (Keyring) String() string                 { return "Keyring[REDACTED]" }
func (Keyring) GoString() string               { return "Keyring[REDACTED]" }
func (Keyring) Format(state fmt.State, _ rune) { formatRedacted(state, "Keyring") }

type keyFile struct {
	Active string            `json:"active_key_id"`
	Keys   map[string]string `json:"keys"`
}

// LoadKeyring accepts only private, regular secret files, never a default key.
func LoadKeyring(path string) (*Keyring, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrKeyring
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, ErrKeyring
	}
	var input keyFile
	if strictjson.Decode(f, &input, 16<<10) != nil || !keyIDPattern.MatchString(input.Active) || len(input.Keys) < 1 || len(input.Keys) > 8 {
		return nil, ErrKeyring
	}
	k := &Keyring{active: input.Active, keys: make(map[string][32]byte)}
	for id, s := range input.Keys {
		raw, e := base64.RawURLEncoding.Strict().DecodeString(s)
		if !keyIDPattern.MatchString(id) || e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != s {
			return nil, ErrKeyring
		}
		var key [32]byte
		copy(key[:], raw)
		clear(raw)
		k.keys[id] = key
	}
	if _, ok := k.keys[k.active]; !ok {
		return nil, ErrKeyring
	}
	return k, nil
}
func GenerateKeyringFile(path string) error {
	var key [32]byte
	if _, e := rand.Read(key[:]); e != nil {
		return e
	}
	defer clear(key[:])
	b, e := json.Marshal(keyFile{Active: "key1", Keys: map[string]string{"key1": base64.RawURLEncoding.EncodeToString(key[:])}})
	if e != nil {
		return ErrKeyring
	}
	defer clear(b)
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.Write(b)
	if e != nil {
		return ErrKeyring
	}
	return f.Sync()
}
func (k *Keyring) seal(context, plain string) ([]byte, error) {
	if k == nil || len(plain) == 0 || len(plain) > 4096 || len(context) == 0 {
		return nil, ErrKeyring
	}
	key, ok := k.keys[k.active]
	if !ok {
		return nil, ErrKeyring
	}
	block, e := aes.NewCipher(key[:])
	if e != nil {
		return nil, ErrKeyring
	}
	a, e := cipher.NewGCMWithRandomNonce(block)
	if e != nil {
		return nil, ErrKeyring
	}
	ciphertext := a.Seal(nil, nil, []byte(plain), []byte("invoice-sealed-v1|"+k.active+"|"+context))
	return []byte("1." + k.active + "." + base64.RawURLEncoding.EncodeToString(ciphertext)), nil
}
func (k *Keyring) open(context string, sealed []byte) (string, error) {
	if k == nil || len(sealed) > 8192 {
		return "", ErrKeyring
	}
	p := strings.Split(string(sealed), ".")
	if len(p) != 3 || p[0] != "1" {
		return "", ErrKeyring
	}
	key, ok := k.keys[p[1]]
	if !ok {
		return "", ErrKeyring
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(p[2])
	if e != nil {
		return "", ErrKeyring
	}
	block, e := aes.NewCipher(key[:])
	if e != nil {
		return "", ErrKeyring
	}
	a, e := cipher.NewGCMWithRandomNonce(block)
	if e != nil {
		return "", ErrKeyring
	}
	plain, e := a.Open(nil, nil, b, []byte("invoice-sealed-v1|"+p[1]+"|"+context))
	if e != nil {
		return "", ErrKeyring
	}
	defer clear(plain)
	return string(plain), nil
}

// SealCursor and OpenCursor bind a server-issued pagination cursor to a
// server-defined context. The context must include the owner and the complete
// filter set so a cursor cannot be replayed across accounts or filters.
func (k *Keyring) SealCursor(context, plain string) (string, error) {
	sealed, e := k.seal(context, plain)
	if e != nil {
		return "", e
	}
	return string(sealed), nil
}

func (k *Keyring) OpenCursor(context, sealed string) (string, error) {
	return k.open(context, []byte(sealed))
}

func (k *Keyring) CSRF(kind, context string) string {
	expiry := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	return k.active + "." + expiry + "." + k.csrfMAC(k.active, kind, context, expiry)
}
func (k *Keyring) csrfMAC(id, kind, context, expiry string) string {
	key := k.keys[id]
	derive := hmac.New(sha256.New, key[:])
	derive.Write([]byte("invoice/csrf/v1"))
	h := hmac.New(sha256.New, derive.Sum(nil))
	h.Write([]byte(kind + "\x00" + context + "\x00" + expiry))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (k *Keyring) VerifyCSRF(value, kind, context string) bool {
	if k == nil || len(value) > 128 {
		return false
	}
	p := strings.Split(value, ".")
	if len(p) != 3 {
		return false
	}
	if _, ok := k.keys[p[0]]; !ok {
		return false
	}
	expiry, e := strconv.ParseInt(p[1], 10, 64)
	now := time.Now().Unix()
	if e != nil || strconv.FormatInt(expiry, 10) != p[1] || expiry <= now || expiry > now+601 {
		return false
	}
	return hmac.Equal([]byte(p[2]), []byte(k.csrfMAC(p[0], kind, context, p[1])))
}
