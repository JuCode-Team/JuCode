package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k)
}

func TestRoundTrip(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, pt := range []string{"sk-ant-abc123", "sk-proj-xyz", "中文密钥🔑", strings.Repeat("a", 4096)} {
		ct, err := c.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", pt[:8], err)
		}
		if ct == pt {
			t.Fatalf("ciphertext equals plaintext for %q", pt[:8])
		}
		got, err := c.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != pt {
			t.Fatalf("round trip = %q, want %q", got[:8], pt[:8])
		}
	}
}

func TestEmptyPassthrough(t *testing.T) {
	c, _ := New(testKey(t))
	ct, err := c.Encrypt("")
	if err != nil || ct != "" {
		t.Fatalf("Encrypt(\"\") = %q, %v; want empty", ct, err)
	}
	got, err := c.Decrypt("")
	if err != nil || got != "" {
		t.Fatalf("Decrypt(\"\") = %q, %v; want empty", got, err)
	}
}

func TestNewRejectsBadKeys(t *testing.T) {
	for _, k := range []string{"", "abc", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("zz", 32)} {
		if _, err := New(k); err == nil {
			t.Fatalf("New(%q) accepted a bad key", k)
		}
	}
}

func TestDecryptRejectsGarbage(t *testing.T) {
	c, _ := New(testKey(t))
	for _, ct := range []string{"!!!not-base64!!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := c.Decrypt(ct); err == nil {
			t.Fatalf("Decrypt(%q) succeeded on garbage", ct)
		}
	}
}

func TestWrongKeyFails(t *testing.T) {
	c1, _ := New(testKey(t))
	c2, _ := New(testKey(t))
	ct, _ := c1.Encrypt("secret")
	if _, err := c2.Decrypt(ct); err == nil {
		t.Fatal("Decrypt with a different key succeeded")
	}
}

func TestNonceRandomized(t *testing.T) {
	c, _ := New(testKey(t))
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext — nonce reuse")
	}
}
