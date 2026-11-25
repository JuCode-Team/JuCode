package jwt

import (
	"strings"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestAccessRoundTrip(t *testing.T) {
	s := NewSigner(testSecret, 15*time.Minute, 7*24*time.Hour)
	uid := uuid.New()
	tok, jti, exp, err := s.IssueAccess(uid, "admin")
	if err != nil {
		t.Fatalf("IssueAccess: %v", err)
	}
	if jti == "" {
		t.Fatal("empty jti")
	}
	if exp.Before(time.Now()) {
		t.Fatal("already expired")
	}
	c, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.UserID != uid || c.Role != "admin" || c.Kind != KindAccess {
		t.Fatalf("claims = %+v", c)
	}
	if c.ID != jti {
		t.Fatalf("jti = %q, want %q", c.ID, jti)
	}
}

func TestRefreshKind(t *testing.T) {
	s := NewSigner(testSecret, time.Minute, time.Hour)
	tok, _, _, err := s.IssueRefresh(uuid.New(), "user")
	if err != nil {
		t.Fatalf("IssueRefresh: %v", err)
	}
	c, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Kind != KindRefresh {
		t.Fatalf("kind = %q, want refresh", c.Kind)
	}
}

func TestOAuthAccessCarriesAuthorizationID(t *testing.T) {
	s := NewSigner(testSecret, time.Minute, time.Hour)
	uid, azid := uuid.New(), uuid.New()
	tok, _, err := s.IssueOAuthAccess(uid, "user", azid)
	if err != nil {
		t.Fatalf("IssueOAuthAccess: %v", err)
	}
	c, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Kind != KindOAuthAccess || c.AuthorizationID != azid {
		t.Fatalf("claims = %+v", c)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	s1 := NewSigner(testSecret, time.Minute, time.Hour)
	s2 := NewSigner("fedcba9876543210fedcba9876543210", time.Minute, time.Hour)
	tok, _, _, _ := s1.IssueAccess(uuid.New(), "user")
	if _, err := s2.Parse(tok); err == nil {
		t.Fatal("Parse accepted a token signed with a different secret")
	}
}

func TestParseRejectsExpired(t *testing.T) {
	s := NewSigner(testSecret, -time.Minute, time.Hour) // already expired
	tok, _, _, _ := s.IssueAccess(uuid.New(), "user")
	if _, err := s.Parse(tok); err == nil {
		t.Fatal("Parse accepted an expired token")
	}
}

func TestParseRejectsAlgNone(t *testing.T) {
	s := NewSigner(testSecret, time.Minute, time.Hour)
	// Forge an alg=none token — the keyfunc must reject non-HS256.
	claims := Claims{
		UserID: uuid.New(), Role: "admin", Kind: KindAccess,
		RegisteredClaims: jwtv5.RegisteredClaims{ExpiresAt: jwtv5.NewNumericDate(time.Now().Add(time.Hour))},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodNone, claims)
	raw, err := tok.SignedString(jwtv5.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("forge: %v", err)
	}
	if _, err := s.Parse(raw); err == nil {
		t.Fatal("Parse accepted an alg=none token")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	s := NewSigner(testSecret, time.Minute, time.Hour)
	for _, raw := range []string{"", "not.a.jwt", strings.Repeat("x", 500)} {
		if _, err := s.Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded", raw[:12])
		}
	}
}

func TestNewSignerPanicsOnShortSecret(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewSigner did not panic on a short secret")
		}
	}()
	NewSigner("short", time.Minute, time.Hour)
}
