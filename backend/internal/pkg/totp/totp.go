// Package totp implements RFC 6238 time-based one-time passwords plus the
// recovery-code helpers the account layer needs. Implemented in-tree (HMAC
// over a 30s counter is ~50 lines) so we don't take on a dependency for
// something this small and security-sensitive.
//
// Parameters are fixed to what every authenticator app defaults to:
// SHA-1, 6 digits, 30-second period. Validation accepts the previous and
// next time step to absorb clock skew between the server and the phone.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Period is the TOTP time-step in seconds.
	Period = 30
	// Digits is the OTP length.
	Digits = 6
	// skewSteps: accept codes from ±1 time step (30s each way).
	skewSteps = 1
	// secretBytes: 20 random bytes = 160 bits, the RFC 4226 recommendation.
	secretBytes = 20
)

// b32 is the unpadded base32 alphabet authenticator apps expect.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a fresh random secret, base32-encoded.
func GenerateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("totp: generate secret: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

// Code computes the 6-digit code for the given secret at time t.
func Code(secret string, t time.Time) (string, error) {
	return codeAt(secret, uint64(t.Unix())/Period)
}

// codeAt is the HOTP core (RFC 4226 §5.3) applied to a time-step counter.
func codeAt(secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(normalizeSecret(secret))
	if err != nil {
		return "", fmt.Errorf("totp: bad secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}

// Validate reports whether code is valid for secret at time t, checking the
// current step and ±1 step for clock skew. On success it also returns the
// matched counter so callers can persist it for replay protection (the same
// counter must never be accepted twice).
func Validate(secret, code string, t time.Time) (bool, uint64) {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return false, 0
	}
	now := uint64(t.Unix()) / Period
	for delta := -skewSteps; delta <= skewSteps; delta++ {
		counter := now + uint64(delta) // wraps harmlessly for negative delta at epoch 0
		if int64(now)+int64(delta) < 0 {
			continue
		}
		want, err := codeAt(secret, counter)
		if err != nil {
			return false, 0
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true, counter
		}
	}
	return false, 0
}

// ProvisioningURI builds the otpauth:// URL encoded into the QR code that
// authenticator apps scan.
func ProvisioningURI(secret, account, issuer string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", normalizeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", Digits))
	q.Set("period", fmt.Sprintf("%d", Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func normalizeSecret(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// --- recovery codes ---

// recoveryAlphabet omits 0/O/1/I/L so codes survive being read aloud or
// copied by hand.
const recoveryAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// RecoveryCodeCount is how many codes a confirm issues.
const RecoveryCodeCount = 10

// GenerateRecoveryCodes returns n plaintext codes shaped like XXXXX-XXXXX.
// Callers must store only HashRecoveryCode(code) and show the plaintext
// exactly once.
func GenerateRecoveryCodes(n int) ([]string, error) {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 10)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("totp: generate recovery code: %w", err)
		}
		var sb strings.Builder
		for j, b := range raw {
			if j == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(b)%len(recoveryAlphabet)])
		}
		out = append(out, sb.String())
	}
	return out, nil
}

// HashRecoveryCode returns the hex SHA-256 of the normalized code. Codes are
// normalized (uppercase, separators stripped) so "abcde-fghjk" and
// "ABCDEFGHJK" hash identically.
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(NormalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

// NormalizeRecoveryCode strips separators/whitespace and uppercases.
func NormalizeRecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}
