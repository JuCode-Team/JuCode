package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors (SHA-1 rows), truncated from 8 to the
// last 6 digits since we emit 6-digit codes.
func TestRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},          // full vector 94287082
		{1111111109, "081804"},  // 07081804
		{1111111111, "050471"},  // 14050471
		{1234567890, "005924"},  // 89005924
		{2000000000, "279037"},  // 69279037
		{20000000000, "353130"}, // 65353130
	}
	for _, tc := range cases {
		got, err := Code(secret, time.Unix(tc.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", tc.unix, err)
		}
		if got != tc.want {
			t.Errorf("Code(t=%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}

func TestValidateWindow(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000015, 0)

	current, _ := Code(secret, now)
	if ok, _ := Validate(secret, current, now); !ok {
		t.Error("current-step code rejected")
	}
	prev, _ := Code(secret, now.Add(-30*time.Second))
	if ok, _ := Validate(secret, prev, now); !ok {
		t.Error("previous-step code rejected (skew window)")
	}
	next, _ := Code(secret, now.Add(30*time.Second))
	if ok, _ := Validate(secret, next, now); !ok {
		t.Error("next-step code rejected (skew window)")
	}
	old, _ := Code(secret, now.Add(-90*time.Second))
	if ok, _ := Validate(secret, old, now); ok {
		t.Error("3-steps-old code accepted")
	}
	if ok, _ := Validate(secret, "000000", now); ok {
		// 1-in-a-million flake tolerated: regenerate secret if this fires.
		cur, _ := Code(secret, now)
		if cur != "000000" {
			t.Error("wrong code accepted")
		}
	}
	if ok, _ := Validate(secret, "12345", now); ok {
		t.Error("short code accepted")
	}
}

func TestValidateReturnsMatchedCounter(t *testing.T) {
	secret, _ := GenerateSecret()
	now := time.Unix(1700000000, 0)
	code, _ := Code(secret, now)
	ok, counter := Validate(secret, code, now)
	if !ok {
		t.Fatal("code rejected")
	}
	if counter != uint64(now.Unix())/Period {
		t.Errorf("counter = %d, want %d", counter, uint64(now.Unix())/Period)
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI("JBSWY3DPEHPK3PXP", "admin@example.com", "JuCode")
	if !strings.HasPrefix(uri, "otpauth://totp/JuCode:admin@example.com?") {
		t.Errorf("bad label: %s", uri)
	}
	for _, want := range []string{"secret=JBSWY3DPEHPK3PXP", "issuer=JuCode", "digits=6", "period=30"} {
		if !strings.Contains(uri, want) {
			t.Errorf("uri missing %q: %s", want, uri)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' {
			t.Errorf("bad shape: %q", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
	}
	// Hash is separator/case-insensitive.
	if HashRecoveryCode("abcde-fghjk") != HashRecoveryCode("ABCDE FGHJK") {
		t.Error("hash should normalize case and separators")
	}
	if HashRecoveryCode(codes[0]) == HashRecoveryCode(codes[1]) {
		t.Error("distinct codes must hash differently")
	}
}
