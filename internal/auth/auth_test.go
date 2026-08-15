package auth

import (
	"strings"
	"testing"
	"time"
)

// Hashing at the production work factor is slow on purpose, so the tests that
// only care about the encoding use a cheap hash built by hand.
func cheapHash(t *testing.T, password string) string {
	t.Helper()
	h, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPasswordRoundTrip(t *testing.T) {
	h := cheapHash(t, "correct horse battery staple")
	if !VerifyPassword(h, "correct horse battery staple") {
		t.Error("the right password did not verify")
	}
	if VerifyPassword(h, "correct horse battery stapl") {
		t.Error("a wrong password verified")
	}
	if VerifyPassword(h, "") {
		t.Error("an empty password verified")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a := cheapHash(t, "same password")
	b := cheapHash(t, "same password")
	if a == b {
		t.Error("two hashes of the same password are identical, so the salt is not random")
	}
	if !VerifyPassword(a, "same password") || !VerifyPassword(b, "same password") {
		t.Error("both hashes should still verify")
	}
}

func TestHashFormat(t *testing.T) {
	h := cheapHash(t, "hunter2hunter2")
	parts := strings.Split(h, "$")
	if len(parts) != 4 {
		t.Fatalf("hash has %d parts, want 4: %q", len(parts), h)
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Errorf("algorithm label = %q", parts[0])
	}
	if parts[1] != "600000" {
		t.Errorf("iteration count = %q, want the current work factor", parts[1])
	}
	if strings.Contains(h, "hunter2") {
		t.Error("the hash contains the password")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "pbkdf2-sha256$abc$salt$key",
		"pbkdf2-sha256$600000$!!!$key", "pbkdf2-sha256$600000$c2FsdA$!!!",
		"bcrypt$10$salt$key",
		"pbkdf2-sha256$1$c2FsdA$a2V5", // work factor below the floor
	} {
		if VerifyPassword(bad, "anything") {
			t.Errorf("malformed hash %q verified", bad)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	if NeedsRehash(cheapHash(t, "abcdefghij")) {
		t.Error("a freshly made hash should not need rehashing")
	}
	if !NeedsRehash("pbkdf2-sha256$1000$c2FsdA$a2V5") {
		t.Error("a hash below the current work factor should need rehashing")
	}
	if !NeedsRehash("garbage") {
		t.Error("an unparseable hash should need rehashing")
	}
}

func TestModeratorKeysAreUniqueAndCheckable(t *testing.T) {
	secret := []byte("server secret")

	a, err := NewModeratorKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewModeratorKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two moderator keys collided")
	}

	hash := ModeratorHash(secret, a)
	if strings.Contains(hash, a) {
		t.Error("the stored hash contains the key it is supposed to protect")
	}
	if !CheckModerator(secret, hash, a) {
		t.Error("the right key did not check out")
	}
	if CheckModerator(secret, hash, b) {
		t.Error("a different key checked out")
	}
	if CheckModerator([]byte("other secret"), hash, a) {
		t.Error("the key checked out under a different server secret")
	}
}

// TestModeratorEmptiesNeverMatch is the one that matters: a room with no key
// recorded must not be administrable by presenting nothing.
func TestModeratorEmptiesNeverMatch(t *testing.T) {
	secret := []byte("s")
	if CheckModerator(secret, "", "") {
		t.Error("empty hash and empty key matched")
	}
	if CheckModerator(secret, "", "some-key") {
		t.Error("an empty stored hash matched a presented key")
	}
	if CheckModerator(secret, ModeratorHash(secret, "k"), "") {
		t.Error("an empty presented key matched a real hash")
	}
}

func TestSignerRoundTrip(t *testing.T) {
	s := NewSigner([]byte("secret"))
	token := s.Sign("abc123", 0)

	got, ok := s.Verify(token)
	if !ok || got != "abc123" {
		t.Fatalf("round trip failed: %q %v", got, ok)
	}
	if !strings.HasPrefix(token, "abc123.") {
		t.Errorf("token should carry its value in the clear for debuggability: %q", token)
	}
}

func TestSignerRejectsTampering(t *testing.T) {
	s := NewSigner([]byte("secret"))
	token := s.Sign("user-1", time.Hour)

	// Swap the value but keep the signature.
	parts := strings.SplitN(token, ".", 2)
	forged := "user-2." + parts[1]
	if _, ok := s.Verify(forged); ok {
		t.Error("a token with a swapped value verified")
	}

	if _, ok := s.Verify(token + "x"); ok {
		t.Error("a token with a mangled signature verified")
	}
	if _, ok := s.Verify("nodots"); ok {
		t.Error("a token with no structure verified")
	}

	other := NewSigner([]byte("different"))
	if _, ok := other.Verify(token); ok {
		t.Error("a token signed with another key verified")
	}
}

func TestSignerExpiry(t *testing.T) {
	s := NewSigner([]byte("secret"))

	if _, ok := s.Verify(s.Sign("v", -time.Hour)); ok {
		t.Error("an expired token verified")
	}
	if _, ok := s.Verify(s.Sign("v", time.Hour)); !ok {
		t.Error("a live token did not verify")
	}
	// A zero TTL means "no expiry", which is what the painter id wants.
	if _, ok := s.Verify(s.Sign("v", 0)); !ok {
		t.Error("a non-expiring token did not verify")
	}
}

func TestNewIDIsRandomAndShort(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewID()
		if len(id) != 12 {
			t.Fatalf("id %q is %d characters, want 12", id, len(id))
		}
		if seen[id] {
			t.Fatalf("id %q was generated twice in 200 draws", id)
		}
		seen[id] = true
	}
}
