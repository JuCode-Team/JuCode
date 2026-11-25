package biz

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entauthz "github.com/jucode/jucode/internal/data/ent/oauthauthorization"
	"github.com/jucode/jucode/internal/pkg/jwt"
)

var (
	ErrOAuthAuthorizationNotFound = errors.New("oauth authorization not found")
	ErrOAuthRefreshInvalid        = errors.New("invalid or expired refresh token")
)

const (
	// Refresh tokens outlive the 30d JWT refresh used by the web session —
	// a CLI/Desktop install should stay logged in for a long time, only
	// re-authing when the user explicitly revokes the device.
	oauthRefreshTTL      = 90 * 24 * time.Hour
	oauthResolveCacheTTL = 60 * time.Second
	refreshTokenPrefix   = "juc-rt-"
)

// oauthDefaultScopes are granted to every first-party device today: run
// inference + read account/usage. No scope-grant UI — kept minimal.
var oauthDefaultScopes = []string{"inference", "account:read"}

// OAuthSvc owns the lifecycle of OAuth device authorizations: token
// issuance/rotation, fast access-token resolution (for the gateway hot
// path), and the list/revoke operations behind 授权设备管理.
type OAuthSvc struct {
	d      *data.Data
	signer *jwt.Signer

	cacheMu sync.Mutex
	cache   map[uuid.UUID]oauthCacheEntry

	// onRevoke ends what a revoked device still holds open (relay sockets).
	onRevoke func(authzID uuid.UUID)
}

// SetOnRevoke is called with each authorization revoked from now on.
func (s *OAuthSvc) SetOnRevoke(f func(authzID uuid.UUID)) { s.onRevoke = f }

// revoked forgets a revoked authorization here: its cache entry and its open
// relay sockets.
func (s *OAuthSvc) revoked(ctx context.Context, authzID uuid.UUID) error {
	s.clearCache(authzID)
	if s.onRevoke != nil {
		s.onRevoke(authzID)
	}
	return nil
}

type oauthCacheEntry struct {
	authz     *ent.OAuthAuthorization
	user      *ent.User
	expiresAt time.Time
}

func NewOAuthSvc(d *data.Data, signer *jwt.Signer) *OAuthSvc {
	return &OAuthSvc{d: d, signer: signer, cache: make(map[uuid.UUID]oauthCacheEntry)}
}

// TokenBundle is the access+refresh pair handed to a client. The refresh
// token plaintext is returned ONCE (on create/rotate); only its hash is
// persisted.
type TokenBundle struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// CreateDevice provisions a new authorized device row and returns the
// initial token pair. clientID is a first-party id (jucode-cli /
// jucode-desktop); name is a human-facing device label.
func (s *OAuthSvc) CreateDevice(ctx context.Context, userID uuid.UUID, clientID, name, ip, ua string) (*ent.OAuthAuthorization, *TokenBundle, error) {
	u, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	refresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	refreshExp := now.Add(oauthRefreshTTL)
	authz, err := s.d.Ent.OAuthAuthorization.Create().
		SetUserID(userID).
		SetClientID(clientID).
		SetName(name).
		SetScopes(oauthDefaultScopes).
		SetRefreshTokenHash(refreshHash).
		SetRefreshExpiresAt(refreshExp).
		SetLastIP(ip).
		SetLastUserAgent(ua).
		SetLastUsedAt(now).
		Save(ctx)
	if err != nil {
		return nil, nil, err
	}
	access, accessExp, err := s.signer.IssueOAuthAccess(userID, string(u.Role), authz.ID)
	if err != nil {
		return nil, nil, err
	}
	return authz, &TokenBundle{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

// Refresh validates the presented refresh token, rotates it, and issues a
// fresh access token. Returns ErrOAuthRefreshInvalid for unknown / revoked
// / expired tokens.
func (s *OAuthSvc) Refresh(ctx context.Context, refreshToken, ip, ua string) (*TokenBundle, error) {
	oldHash := hashSecret(refreshToken)
	authz, err := s.d.Ent.OAuthAuthorization.Query().
		Where(entauthz.RefreshTokenHashEQ(oldHash), entauthz.RevokedAtIsNil()).
		WithUser().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOAuthRefreshInvalid
		}
		return nil, err
	}
	if authz.RefreshExpiresAt.Before(time.Now()) {
		return nil, ErrOAuthRefreshInvalid
	}
	u, err := authz.Edges.UserOrErr()
	if err != nil {
		return nil, err
	}
	newRefresh, newHash, err := newRefreshToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	refreshExp := now.Add(oauthRefreshTTL)
	// Rotate only if the presented token is still current: of two
	// concurrent refreshes with the same token exactly one wins, instead of
	// the later one silently invalidating the earlier one's new token.
	n, err := s.d.Ent.OAuthAuthorization.Update().
		Where(entauthz.IDEQ(authz.ID), entauthz.RefreshTokenHashEQ(oldHash), entauthz.RevokedAtIsNil()).
		SetRefreshTokenHash(newHash).
		SetRefreshExpiresAt(refreshExp).
		SetLastIP(ip).
		SetLastUserAgent(ua).
		SetLastUsedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrOAuthRefreshInvalid
	}
	s.clearCache(authz.ID)
	access, accessExp, err := s.signer.IssueOAuthAccess(u.ID, string(u.Role), authz.ID)
	if err != nil {
		return nil, err
	}
	return &TokenBundle{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     newRefresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

// ResolveAccess loads the device + owning user for an access token's
// authorization id, with a 60s cache (mirrors ApiKeySvc.Resolve). A
// revoked or missing device returns ErrOAuthAuthorizationNotFound so the
// gateway rejects in-flight tokens shortly after a revoke.
func (s *OAuthSvc) ResolveAccess(ctx context.Context, authzID uuid.UUID) (*ent.OAuthAuthorization, *ent.User, error) {
	now := time.Now()
	s.cacheMu.Lock()
	if e, ok := s.cache[authzID]; ok {
		if now.Before(e.expiresAt) {
			s.cacheMu.Unlock()
			return e.authz, e.user, nil
		}
		delete(s.cache, authzID)
	}
	s.cacheMu.Unlock()

	authz, err := s.d.Ent.OAuthAuthorization.Query().
		Where(entauthz.IDEQ(authzID), entauthz.RevokedAtIsNil()).
		WithUser().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil, ErrOAuthAuthorizationNotFound
		}
		return nil, nil, err
	}
	u, err := authz.Edges.UserOrErr()
	if err != nil {
		return nil, nil, err
	}
	s.cacheMu.Lock()
	s.cache[authzID] = oauthCacheEntry{authz: authz, user: u, expiresAt: now.Add(oauthResolveCacheTTL)}
	s.cacheMu.Unlock()
	return authz, u, nil
}

func (s *OAuthSvc) ListDevices(ctx context.Context, userID uuid.UUID) ([]*ent.OAuthAuthorization, error) {
	return s.d.Ent.OAuthAuthorization.Query().
		Where(entauthz.UserIDEQ(userID), entauthz.RevokedAtIsNil()).
		Order(ent.Desc(entauthz.FieldCreatedAt)).
		All(ctx)
}

func (s *OAuthSvc) RevokeDevice(ctx context.Context, userID, authzID uuid.UUID) error {
	n, err := s.d.Ent.OAuthAuthorization.Update().
		Where(entauthz.IDEQ(authzID), entauthz.UserIDEQ(userID), entauthz.RevokedAtIsNil()).
		SetRevokedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrOAuthAuthorizationNotFound
	}
	return s.revoked(ctx, authzID)
}

// RevokeByRefreshToken revokes the device the refresh token belongs to
// (RFC 7009). An unknown or already revoked token is not an error.
func (s *OAuthSvc) RevokeByRefreshToken(ctx context.Context, refreshToken string) error {
	authz, err := s.d.Ent.OAuthAuthorization.Query().
		Where(entauthz.RefreshTokenHashEQ(hashSecret(refreshToken))).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if _, err := s.d.Ent.OAuthAuthorization.Update().
		Where(entauthz.IDEQ(authz.ID), entauthz.RevokedAtIsNil()).
		SetRevokedAt(time.Now()).
		Save(ctx); err != nil {
		return err
	}
	return s.revoked(ctx, authz.ID)
}

// TouchLastUsed refreshes the device's activity columns. Best-effort;
// callers throttle invocation so this isn't a per-request write.
func (s *OAuthSvc) TouchLastUsed(ctx context.Context, authzID uuid.UUID, ip, ua string) {
	_, _ = s.d.Ent.OAuthAuthorization.UpdateOneID(authzID).
		SetLastUsedAt(time.Now()).
		SetLastIP(ip).
		SetLastUserAgent(ua).
		Save(ctx)
}

func (s *OAuthSvc) clearCache(authzID uuid.UUID) {
	s.cacheMu.Lock()
	delete(s.cache, authzID)
	s.cacheMu.Unlock()
}

// newRefreshToken returns (plaintext, sha256hex). Reuses biz.hashSecret
// (defined in apikey.go) so refresh tokens hash identically to api keys.
func newRefreshToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = refreshTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, hashSecret(token), nil
}
