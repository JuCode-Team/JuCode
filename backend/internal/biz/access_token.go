package biz

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entpat "github.com/jucode/jucode/internal/data/ent/personalaccesstoken"
	"github.com/jucode/jucode/internal/pkg/idgen"
)

// Personal access tokens let a user drive the console APIs from scripts and
// agents with their own current role. See middleware.JWT for where they are
// accepted and middleware.SessionOnly / RequireTOTPStepUp for where not.

const AccessTokenPrefix = "jcp-"

var (
	ErrAccessTokenInvalid  = errors.New("invalid access token")
	ErrAccessTokenNotFound = errors.New("access token not found")
)

type AccessTokenSvc struct{ d *data.Data }

func NewAccessTokenSvc(d *data.Data) *AccessTokenSvc { return &AccessTokenSvc{d: d} }

// IsAccessToken reports whether a bearer credential is a personal access
// token rather than a session JWT.
func IsAccessToken(raw string) bool { return strings.HasPrefix(raw, AccessTokenPrefix) }

// Create mints a token for userID. The plaintext is returned once and only
// its hash is stored.
func (s *AccessTokenSvc) Create(ctx context.Context, userID uuid.UUID, name string, expiresAt *time.Time) (*ent.PersonalAccessToken, string, error) {
	full, prefix, err := idgen.NewAccessTokenSecret()
	if err != nil {
		return nil, "", err
	}
	t, err := s.d.Ent.PersonalAccessToken.Create().
		SetUserID(userID).
		SetName(name).
		SetPrefix(prefix).
		SetHash(hashSecret(full)).
		SetNillableExpiresAt(expiresAt).
		Save(ctx)
	if err != nil {
		return nil, "", err
	}
	return t, full, nil
}

// List returns userID's tokens that are not revoked, newest first.
func (s *AccessTokenSvc) List(ctx context.Context, userID uuid.UUID) ([]*ent.PersonalAccessToken, error) {
	return s.d.Ent.PersonalAccessToken.Query().
		Where(entpat.UserIDEQ(userID), entpat.RevokedAtIsNil()).
		Order(ent.Desc(entpat.FieldCreatedAt)).
		All(ctx)
}

// Revoke disables one of userID's tokens immediately.
func (s *AccessTokenSvc) Revoke(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.d.Ent.PersonalAccessToken.Update().
		Where(entpat.IDEQ(id), entpat.UserIDEQ(userID), entpat.RevokedAtIsNil()).
		SetRevokedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAccessTokenNotFound
	}
	return nil
}

// Resolve checks a presented token and returns it with its owner. The owner's
// role is read now, not when the token was made, so a demotion applies to
// the very next request. Account status is left to the caller's status check.
func (s *AccessTokenSvc) Resolve(ctx context.Context, raw string) (*ent.PersonalAccessToken, *ent.User, error) {
	t, err := s.d.Ent.PersonalAccessToken.Query().
		Where(entpat.HashEQ(hashSecret(raw))).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	if t.RevokedAt != nil || (t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)) {
		return nil, nil, ErrAccessTokenInvalid
	}
	u, err := s.d.Ent.User.Get(ctx, t.UserID)
	if ent.IsNotFound(err) {
		return nil, nil, ErrAccessTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	// Best effort, off the request path and at most once a minute, like the
	// API key and device paths, so a busy agent does not queue on this row.
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > time.Minute {
		go func(id uuid.UUID) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.d.Ent.PersonalAccessToken.UpdateOneID(id).SetLastUsedAt(now).Exec(ctx)
		}(t.ID)
	}
	return t, u, nil
}
