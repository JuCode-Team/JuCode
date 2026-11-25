// Package pwd is a thin wrapper around bcrypt with our chosen cost.
//
// Cost 12 takes ~250ms on a 2024-class CPU — enough to slow offline cracking
// without blocking login traffic. Adjust only after benchmarking on prod hw.
//
// bcrypt silently truncates inputs longer than 72 bytes. To support
// arbitrary-length passphrases (and to remove the silent-truncation
// foot-gun), we pre-hash the plaintext with SHA-256 and base64 the
// digest before handing it to bcrypt. The base64 output is always 44
// bytes, well under bcrypt's limit.
//
// Backwards compatibility: Verify accepts both the new pre-hashed scheme
// and legacy bcrypt-of-raw-plaintext hashes — when only the legacy form
// matches, Verify returns nil but ErrNeedsRehash, so callers can roll
// the user forward to the new scheme on next successful login.
package pwd

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

const cost = 12

// ErrNeedsRehash signals "this password verified against a legacy hash;
// re-hash and persist the new form when convenient." Callers that don't
// care can simply ignore it.
var ErrNeedsRehash = errors.New("password verified against legacy hash; re-hash recommended")

func preHash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	encoded := make([]byte, base64.RawStdEncoding.EncodedLen(len(sum)))
	base64.RawStdEncoding.Encode(encoded, sum[:])
	return encoded
}

func Hash(plaintext string) (string, error) {
	b, err := bcrypt.GenerateFromPassword(preHash(plaintext), cost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Verify returns nil on match. When the stored hash is in the legacy
// (pre-rehash) form Verify still returns nil but pairs it with
// ErrNeedsRehash via errors.Is — callers that wish to upgrade silently
// can check for this and re-Hash. Mismatches return
// bcrypt.ErrMismatchedHashAndPassword unchanged.
func Verify(hash, plaintext string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), preHash(plaintext)); err == nil {
		return nil
	}
	// Fall through to legacy: bcrypt of raw plaintext. Only return
	// "needs rehash" — not an error — so the caller treats it as a
	// successful login.
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err == nil {
		return ErrNeedsRehash
	}
	return bcrypt.ErrMismatchedHashAndPassword
}
