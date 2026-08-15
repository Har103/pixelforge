// Package auth handles the two secrets Pixelforge keeps: account passwords and
// the moderator keys that let somebody administer a room without an account.
//
// PBKDF2-HMAC-SHA256 is reimplemented here in a dozen lines rather than
// imported. crypto/pbkdf2 only reached the standard library in Go 1.24 and this
// module still declares 1.23, and golang.org/x/crypto would break the
// zero-dependency rule the whole project is built on. Argon2id would be the
// better primitive and is the thing to revisit if x/crypto ever becomes
// acceptable; PBKDF2-HMAC-SHA256 at 600,000 iterations is OWASP's own fallback
// recommendation, so this is a considered trade rather than a shortcut.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Iterations is the PBKDF2 work factor for new passwords. Stored hashes carry
// their own count, so raising this only affects passwords set afterwards and
// never locks anybody out.
const Iterations = 600_000

const (
	saltLen = 16
	keyLen  = 32
)

// ErrBadHash means a stored hash is not in a format this package wrote.
var ErrBadHash = errors.New("auth: unrecognised password hash format")

// HashPassword returns an encoded hash of the form
// "pbkdf2-sha256$<iterations>$<salt-b64>$<key-b64>".
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}
	key := pbkdf2SHA256([]byte(password), salt, Iterations, keyLen)
	return strings.Join([]string{
		"pbkdf2-sha256",
		strconv.Itoa(Iterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	}, "$"), nil
}

// VerifyPassword checks a password against an encoded hash in constant time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters < 1000 || iters > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iters, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NeedsRehash reports whether a stored hash was made with a weaker work factor
// than the current one, so a successful login can transparently upgrade it.
func NeedsRehash(encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return true
	}
	iters, err := strconv.Atoi(parts[1])
	return err != nil || iters < Iterations
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) specialised to HMAC-SHA-256.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	counter := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(counter, uint32(block))
		u := hmacSHA256(password, append(append([]byte{}, salt...), counter...))
		t := make([]byte, hLen)
		copy(t, u)
		for i := 1; i < iter; i++ {
			u = hmacSHA256(password, u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// ------------------------------------------------------------ moderator ----

// NewModeratorKey mints the secret that proves somebody created a room. It is
// shown to the creator once; only its HMAC is stored, so a database leak does
// not hand out control of every canvas.
func NewModeratorKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generating moderator key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ModeratorHash is what gets stored for a moderator key.
func ModeratorHash(secret []byte, key string) string {
	return hex.EncodeToString(hmacSHA256(secret, []byte("moderator:"+key)))
}

// CheckModerator compares a presented key against a stored hash in constant
// time. An empty stored hash never matches, so a room with no key recorded
// cannot be taken over by presenting an empty one.
func CheckModerator(secret []byte, storedHash, presented string) bool {
	if storedHash == "" || presented == "" {
		return false
	}
	want := ModeratorHash(secret, presented)
	return subtle.ConstantTimeCompare([]byte(want), []byte(storedHash)) == 1
}

// -------------------------------------------------------------- sessions ----

// Signer issues and validates the short signed strings used for the anonymous
// painter id and the account session cookie. Both are "value.expiry.signature"
// so a cookie cannot be edited and cannot live forever.
type Signer struct {
	secret []byte
}

// NewSigner wraps a secret.
func NewSigner(secret []byte) *Signer { return &Signer{secret: secret} }

// Sign returns a signed token that expires after ttl. A zero ttl never expires,
// which is what the painter id wants: it is an identity, not a credential.
//
// A negative ttl produces an already-expired token rather than a permanent one.
// That direction matters: a caller that computes a duration and gets the sign
// wrong should end up with a credential that does not work, not one that never
// stops working.
func (s *Signer) Sign(value string, ttl time.Duration) string {
	var exp int64
	if ttl != 0 {
		exp = time.Now().Add(ttl).Unix()
	}
	payload := value + "." + strconv.FormatInt(exp, 10)
	mac := hmacSHA256(s.secret, []byte(payload))
	return payload + "." + hex.EncodeToString(mac[:16])
}

// Verify checks a token and returns the value it carries.
func (s *Signer) Verify(token string) (string, bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return "", false
	}
	payload, sig := token[:i], token[i+1:]
	mac := hmacSHA256(s.secret, []byte(payload))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac[:16])), []byte(sig)) != 1 {
		return "", false
	}
	j := strings.LastIndexByte(payload, '.')
	if j <= 0 {
		return "", false
	}
	value, expStr := payload[:j], payload[j+1:]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", false
	}
	if exp != 0 && time.Now().Unix() > exp {
		return "", false
	}
	return value, true
}

// NewID mints a short random identifier for an anonymous painter.
func NewID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		// A predictable id under a broken CSPRNG is survivable; refusing to
		// serve the page is not.
		binary.BigEndian.PutUint64(append(raw[:0], make([]byte, 8)...), uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(raw)
}
