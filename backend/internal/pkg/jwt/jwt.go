// Package jwt issues and verifies the two-token pair (access + refresh).
//
// Access tokens: HS256, short-lived, stateless — verified anywhere by the
// shared secret without touching storage.
//
// Refresh tokens: HS256, long-lived, BUT we additionally verify the jti
// against a Redis allow-set. Logout / password-change / ban simply
// deletes the user's jti set, instantly invalidating every refresh
// without rotating the signing key.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/pkg/idgen"
)

type TokenKind string

const (
	KindAccess  TokenKind = "access"
	KindRefresh TokenKind = "refresh"
	// KindOAuthAccess is the short-lived access token minted by the
	// first-party OAuth device flow (CLI / Desktop). It carries the
	// authorizing device's id in AuthorizationID so the gateway can
	// attribute usage to a device and verify the device isn't revoked.
	KindOAuthAccess TokenKind = "oauth_access"
)

type Claims struct {
	UserID uuid.UUID `json:"uid"`
	Role   string    `json:"role"`
	Kind   TokenKind `json:"knd"`
	// AuthorizationID is set only for KindOAuthAccess tokens — the
	// oauth_authorizations row (device) that minted this token.
	AuthorizationID uuid.UUID `json:"azid,omitempty"`
	jwtv5.RegisteredClaims
}

type Signer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// minSecretBytes is the minimum byte length we accept for the HS256
// signing secret. 32 bytes ≈ 256 bits — anything shorter is well within
// modern brute-force budgets, and a misconfigured deploy that drops a
// default empty string here would let anyone forge tokens.
const minSecretBytes = 32

// NewSigner panics on a too-short secret. We deliberately fail fast at
// process start rather than silently accepting a weak key.
func NewSigner(secret string, accessTTL, refreshTTL time.Duration) *Signer {
	if len(secret) < minSecretBytes {
		panic(fmt.Sprintf("jwt: signing secret must be >= %d bytes (got %d) — set a strong JWT_SECRET", minSecretBytes, len(secret)))
	}
	return &Signer{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

func (s *Signer) AccessTTL() time.Duration  { return s.accessTTL }
func (s *Signer) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccess returns (token, jti, expiresAt, err).
func (s *Signer) IssueAccess(userID uuid.UUID, role string) (string, string, time.Time, error) {
	return s.issue(userID, role, KindAccess, s.accessTTL)
}

// IssueRefresh returns (token, jti, expiresAt, err).
func (s *Signer) IssueRefresh(userID uuid.UUID, role string) (string, string, time.Time, error) {
	return s.issue(userID, role, KindRefresh, s.refreshTTL)
}

// IssueOAuthAccess mints a KindOAuthAccess token for an OAuth device,
// reusing the access TTL. Returns (token, expiresAt, err).
func (s *Signer) IssueOAuthAccess(userID uuid.UUID, role string, authzID uuid.UUID) (string, time.Time, error) {
	jti, err := idgen.NewJTI()
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now()
	exp := now.Add(s.accessTTL)
	claims := Claims{
		UserID:          userID,
		Role:            role,
		Kind:            KindOAuthAccess,
		AuthorizationID: authzID,
		RegisteredClaims: jwtv5.RegisteredClaims{
			ID:        jti,
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(exp),
			NotBefore: jwtv5.NewNumericDate(now),
		},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

func (s *Signer) issue(userID uuid.UUID, role string, kind TokenKind, ttl time.Duration) (string, string, time.Time, error) {
	jti, err := idgen.NewJTI()
	if err != nil {
		return "", "", time.Time{}, err
	}
	now := time.Now()
	exp := now.Add(ttl)
	claims := Claims{
		UserID: userID,
		Role:   role,
		Kind:   kind,
		RegisteredClaims: jwtv5.RegisteredClaims{
			ID:        jti,
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(exp),
			NotBefore: jwtv5.NewNumericDate(now),
		},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return signed, jti, exp, nil
}

var ErrInvalidToken = errors.New("invalid token")

// Parse verifies the signature and standard claims, returning the typed claims.
// It does NOT check the jti allow-set — callers that need that (refresh flow)
// must consult redis after.
func (s *Signer) Parse(raw string) (*Claims, error) {
	var c Claims
	t, err := jwtv5.ParseWithClaims(raw, &c, func(t *jwtv5.Token) (any, error) {
		if t.Method.Alg() != jwtv5.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected alg: %s", t.Method.Alg())
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, ErrInvalidToken
	}
	return &c, nil
}
