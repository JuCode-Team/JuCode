// Package crypto wraps AES-256-GCM for at-rest encryption of small
// strings (upstream provider credentials). Output is base64.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

type Cipher struct {
	aead cipher.AEAD
}

// New takes a 64-char hex string (32 bytes of key material) and returns a
// reusable Cipher. The same hex string MUST be used across deploys or
// previously stored ciphertexts become unreadable.
func New(hexKey string) (*Cipher, error) {
	if len(hexKey) != 64 {
		return nil, fmt.Errorf("aesgcm: key must be 64 hex chars (got %d)", len(hexKey))
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: hex decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext || tag). Empty plaintext
// returns empty string (so callers can store optional fields as "").
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) Decrypt(b64 string) (string, error) {
	if b64 == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("aesgcm: base64 decode: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("aesgcm: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("aesgcm: open: %w", err)
	}
	return string(pt), nil
}
