// Package idgen generates random secrets (API keys, refresh-token jti).
package idgen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
)

// NewAPIKeySecret returns a base32-ish 32-byte secret stripped of '/' and '+'
// to keep it shell-safe. Returned as "sk-juc-<43chars>".
//
// 32 random bytes = 256 bits of entropy → impractical to brute-force.
func NewAPIKeySecret() (full, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	full = "sk-juc-" + s
	// prefix shown in UI = the literal first 8 chars of the random tail
	if len(s) >= 8 {
		prefix = "sk-juc-" + s[:8]
	} else {
		prefix = full
	}
	return full, prefix, nil
}

// NewManagementSecret returns a 32-byte random secret formatted as
// "jcm-<43chars>" — used for the server-to-server Management API
// (X-Management-Key header). Mirrors NewAPIKeySecret entropy / shell
// safety; distinct prefix so the two key kinds aren't confusable in
// logs or admin UIs.
func NewManagementSecret() (full, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	full = "jcm-" + s
	if len(s) >= 8 {
		prefix = "jcm-" + s[:8]
	} else {
		prefix = full
	}
	return full, prefix, nil
}

// NewAccessTokenSecret returns a 32-byte random personal access token
// formatted as "jcp-<43chars>", with the same entropy and shell safety as
// NewAPIKeySecret and a prefix of its own so it is never mistaken for one.
func NewAccessTokenSecret() (full, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	return "jcp-" + s, "jcp-" + s[:8], nil
}

// NewJTI returns a 16-byte hex string for use as a JWT jti claim.
func NewJTI() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
