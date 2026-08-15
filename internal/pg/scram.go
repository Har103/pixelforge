package pg

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 client, per RFC 5802 with the SHA-256 profile from RFC 7677.
// PostgreSQL wraps this in SASL messages (AuthenticationSASL / SASLContinue /
// SASLFinal); see "SASL Authentication" in the protocol docs.
//
// PBKDF2 is implemented here rather than imported: crypto/pbkdf2 only landed in
// the standard library in Go 1.24, and this driver targets anything from 1.21
// up. It is a dozen lines anyway.

const scramSHA256 = "SCRAM-SHA-256"

type scramClient struct {
	password    string
	clientFirst string // the "bare" portion, without the gs2 header
	nonce       string
	saltedPass  []byte
	authMsg     string
}

func newSCRAM(password string) (*scramClient, error) {
	// 18 raw bytes -> 24 base64 characters, none of which are ',' or '=' in a
	// position that would confuse the attribute parser.
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("pg: generating SCRAM nonce: %w", err)
	}
	c := &scramClient{
		password: saslPrep(password),
		nonce:    base64.StdEncoding.EncodeToString(raw),
	}
	c.clientFirst = "n=,r=" + c.nonce
	return c, nil
}

// first returns the initial client message including the gs2 header. "n,," means
// the client does not support channel binding.
func (c *scramClient) first() []byte {
	return []byte("n,," + c.clientFirst)
}

// step consumes the server-first-message and produces client-final-message.
func (c *scramClient) step(serverFirst []byte) ([]byte, error) {
	attrs, err := parseSCRAMAttrs(string(serverFirst))
	if err != nil {
		return nil, err
	}
	combinedNonce := attrs["r"]
	saltB64 := attrs["s"]
	iterStr := attrs["i"]
	if combinedNonce == "" || saltB64 == "" || iterStr == "" {
		return nil, fmt.Errorf("pg: malformed SCRAM server-first-message %q", serverFirst)
	}
	if !strings.HasPrefix(combinedNonce, c.nonce) {
		// The server must extend our nonce; anything else means we are not
		// talking to the party we started the exchange with.
		return nil, fmt.Errorf("pg: SCRAM server nonce does not extend client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("pg: decoding SCRAM salt: %w", err)
	}
	iters, err := strconv.Atoi(iterStr)
	if err != nil || iters <= 0 {
		return nil, fmt.Errorf("pg: bad SCRAM iteration count %q", iterStr)
	}
	if iters > 1_000_000 {
		return nil, fmt.Errorf("pg: refusing SCRAM iteration count %d", iters)
	}

	c.saltedPass = pbkdf2SHA256([]byte(c.password), salt, iters, sha256.Size)

	clientKey := hmacSHA256(c.saltedPass, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	// "c=biws" is base64("n,,"), the channel-binding data we advertised.
	finalWithoutProof := "c=biws,r=" + combinedNonce
	c.authMsg = c.clientFirst + "," + string(serverFirst) + "," + finalWithoutProof

	clientSig := hmacSHA256(storedKey[:], []byte(c.authMsg))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSig[i]
	}

	return []byte(finalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)), nil
}

// verify checks the server-final-message so a man in the middle cannot complete
// the handshake without knowing the stored key.
func (c *scramClient) verify(serverFinal []byte) error {
	attrs, err := parseSCRAMAttrs(string(serverFinal))
	if err != nil {
		return err
	}
	if e := attrs["e"]; e != "" {
		return fmt.Errorf("pg: SCRAM authentication failed: %s", e)
	}
	sigB64 := attrs["v"]
	if sigB64 == "" {
		return fmt.Errorf("pg: SCRAM server-final-message has no verifier")
	}
	got, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("pg: decoding SCRAM verifier: %w", err)
	}
	serverKey := hmacSHA256(c.saltedPass, []byte("Server Key"))
	want := hmacSHA256(serverKey, []byte(c.authMsg))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return fmt.Errorf("pg: SCRAM server verifier mismatch")
	}
	return nil
}

// parseSCRAMAttrs splits "k=v,k=v" into a map. Values may themselves contain
// '=' (base64 padding), so only the first '=' separates key from value.
func parseSCRAMAttrs(s string) (map[string]string, error) {
	out := make(map[string]string, 4)
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || len(k) != 1 {
			return nil, fmt.Errorf("pg: malformed SCRAM attribute %q", part)
		}
		out[k] = v
	}
	return out, nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) specialised to HMAC-SHA-256.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(buf, uint32(block))
		u := hmacSHA256(password, append(append([]byte{}, salt...), buf...))
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

// saslPrep is a deliberately partial stand-in for RFC 4013. Full SASLprep needs
// the Unicode normalisation tables, which would mean pulling in golang.org/x/text
// and breaking the zero-dependency rule. ASCII passwords - the overwhelming
// majority, and all that managed providers generate - pass through unchanged,
// and non-ASCII ones are sent as-is, which matches what libpq does when it
// cannot normalise. Control characters are stripped because they are prohibited
// outright and would otherwise corrupt the exchange.
func saslPrep(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}
