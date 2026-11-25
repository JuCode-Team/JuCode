package pwd

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(h, "correct horse battery staple"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	h, _ := Hash("right")
	if err := Verify(h, "wrong"); !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("Verify wrong password = %v, want ErrMismatchedHashAndPassword", err)
	}
}

func TestLongPassphraseNotTruncated(t *testing.T) {
	// bcrypt silently truncates at 72 bytes; the SHA-256 pre-hash must make
	// passphrases longer than that work, and two passphrases sharing a
	// 72-byte prefix must NOT collide.
	long1 := strings.Repeat("a", 72) + "-suffix-1"
	long2 := strings.Repeat("a", 72) + "-suffix-2"
	h, err := Hash(long1)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(h, long1); err != nil {
		t.Fatalf("Verify long passphrase: %v", err)
	}
	if err := Verify(h, long2); err == nil {
		t.Fatal("72-byte-prefix collision: long2 verified against long1's hash")
	}
}

func TestLegacyHashNeedsRehash(t *testing.T) {
	// Legacy form: bcrypt of the raw plaintext (no SHA-256 pre-hash).
	legacy, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("legacy hash: %v", err)
	}
	err = Verify(string(legacy), "hunter2")
	if !errors.Is(err, ErrNeedsRehash) {
		t.Fatalf("Verify legacy = %v, want ErrNeedsRehash", err)
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{"", "not-bcrypt", "$2a$12$tooshort"} {
		if err := Verify(h, "x"); err == nil {
			t.Fatalf("Verify(%q) succeeded", h)
		}
	}
}
