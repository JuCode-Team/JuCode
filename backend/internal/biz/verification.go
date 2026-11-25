// Package biz — email verification token service.
//
// VerificationSvc owns the email_verification_tokens table:
// generate, send the email, and validate inbound clicks. Three kinds:
//
//	register         created at signup, target = user.email
//	change_email     created when user requests email change,
//	                 target = pending_email (NEW address)
//	reset_password   created via /auth/forgot-password
//
// Tokens are URL-safe random 32-byte base64; they live in Redis-free
// land (Postgres only) so a restart doesn't drop pending requests.
package biz

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entevt "github.com/jucode/jucode/internal/data/ent/emailverificationtoken"
)

var (
	ErrVerificationNotFound  = errors.New("verification: token not found or expired")
	ErrVerificationExpired   = errors.New("verification: token expired")
	ErrVerificationUsed      = errors.New("verification: token already used")
	ErrInvalidVerifyCode     = errors.New("invalid or expired verification code")
	ErrVerifyCodeTooFrequent = errors.New("please wait before requesting a new verification code")
)

const (
	verifyRegisterTTL    = 24 * time.Hour
	verifyChangeEmailTTL = 24 * time.Hour
	verifyResetPwdTTL    = 1 * time.Hour
	registerCodeTTL      = 15 * time.Minute
	registerCodeCooldown = 60 * time.Second
	registerCodeDigits   = "0123456789"
)

type VerificationSvc struct {
	d        *data.Data
	settings *SettingsSvc
	email    *EmailSvc
}

func NewVerificationSvc(d *data.Data, settings *SettingsSvc, email *EmailSvc) *VerificationSvc {
	return &VerificationSvc{d: d, settings: settings, email: email}
}

// IssueRegisterCode sends a short-lived code before account creation.
// This is the register-page flow used when RequireRegisterVerification
// is enabled.
func (s *VerificationSvc) IssueRegisterCode(ctx context.Context, email, ip string) error {
	if !s.email.Enabled(ctx) {
		return ErrEmailDisabled
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("email required")
	}
	key := registerCodeKey(email)
	if s.d.Redis != nil {
		if ttl, err := s.d.Redis.TTL(ctx, key).Result(); err == nil && ttl > registerCodeTTL-registerCodeCooldown {
			return ErrVerifyCodeTooFrequent
		}
	}
	code, err := numericCode(6)
	if err != nil {
		return err
	}
	if s.d.Redis == nil {
		return errors.New("redis unavailable")
	}
	if err := s.d.Redis.Set(ctx, key, code, registerCodeTTL).Err(); err != nil {
		return err
	}
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		_ = s.d.Redis.Del(ctx, key).Err()
		return err
	}
	subject, body := RegisterCodeEmail(cfg.SiteName, code)
	if err := s.email.Send(ctx, email, subject, body); err != nil {
		_ = s.d.Redis.Del(ctx, key).Err()
		return err
	}
	return nil
}

// VerifyRegisterCode consumes the pre-registration email code.
func (s *VerificationSvc) VerifyRegisterCode(ctx context.Context, email, code string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	code = strings.TrimSpace(code)
	if email == "" || code == "" || s.d.Redis == nil {
		return ErrInvalidVerifyCode
	}
	key := registerCodeKey(email)
	want, err := s.d.Redis.Get(ctx, key).Result()
	if err != nil {
		return ErrInvalidVerifyCode
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(code)) != 1 {
		return ErrInvalidVerifyCode
	}
	return s.d.Redis.Del(ctx, key).Err()
}

// IssueRegister creates a verification token for the user's current
// email and sends the confirmation email, including for existing users.
func (s *VerificationSvc) IssueRegister(ctx context.Context, user *ent.User, ip string) error {
	if !s.email.Enabled(ctx) {
		return ErrEmailDisabled
	}
	if s.d.Redis == nil {
		return errors.New("redis unavailable")
	}
	// Atomic per-user cooldown also covers concurrent clicks and tabs.
	key := "auth:email_verify_cooldown:" + user.ID.String()
	allowed, err := s.d.Redis.SetNX(ctx, key, "1", registerCodeCooldown).Result()
	if err != nil {
		return err
	}
	if !allowed {
		return ErrVerifyCodeTooFrequent
	}
	tok, err := s.create(ctx, user.ID, entevt.KindRegister, user.Email, verifyRegisterTTL, ip)
	if err != nil {
		_ = s.d.Redis.Del(ctx, key).Err()
		return err
	}
	if err := s.sendVerifyEmail(ctx, user.Email, tok); err != nil {
		_ = s.d.Redis.Del(ctx, key).Err()
		return err
	}
	return nil
}

// IssueChangeEmail creates a token bound to newEmail. The token's
// pending_email field carries newEmail; on consume we flip
// user.email to that value and mark email_verified=true.
func (s *VerificationSvc) IssueChangeEmail(ctx context.Context, user *ent.User, newEmail, ip string) error {
	if !s.email.Enabled(ctx) {
		return ErrEmailDisabled
	}
	newEmail = strings.ToLower(strings.TrimSpace(newEmail))
	if newEmail == "" {
		return errors.New("new email required")
	}
	tok, err := s.create(ctx, user.ID, entevt.KindChangeEmail, newEmail, verifyChangeEmailTTL, ip)
	if err != nil {
		return err
	}
	return s.sendVerifyEmail(ctx, newEmail, tok)
}

// IssueResetPassword creates a token for the forgot-password flow.
// The email is always sent to the user's current address — we never
// reveal whether the email is registered to an unauthenticated caller,
// so the caller upstream of this should also return 204 either way.
func (s *VerificationSvc) IssueResetPassword(ctx context.Context, user *ent.User, ip string) error {
	if !s.email.Enabled(ctx) {
		return ErrEmailDisabled
	}
	tok, err := s.create(ctx, user.ID, entevt.KindResetPassword, "", verifyResetPwdTTL, ip)
	if err != nil {
		return err
	}
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return err
	}
	siteName := cfg.SiteName
	link := s.buildLink(cfg.SiteURL, "/reset-password", tok.Code)
	subject, body := PasswordResetEmail(siteName, link)
	return s.email.Send(ctx, user.Email, subject, body)
}

// Consume validates a token and marks it used atomically. Two concurrent
// callers presenting the same code only one will succeed: the UPDATE is
// gated on used_at IS NULL via rows-affected (the SELECT-then-SetUsedAt
// pattern lets both pass, which would allow a single reset-password
// link to be redeemed twice).
//
// kind is checked to prevent a register-token being submitted to the
// reset-password endpoint.
func (s *VerificationSvc) Consume(ctx context.Context, code string, kind entevt.Kind) (*ent.EmailVerificationToken, error) {
	t, err := s.d.Ent.EmailVerificationToken.Query().
		Where(entevt.CodeEQ(strings.TrimSpace(code))).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrVerificationNotFound
		}
		return nil, err
	}
	if t.Kind != kind {
		return nil, ErrVerificationNotFound
	}
	if t.UsedAt != nil {
		return nil, ErrVerificationUsed
	}
	if time.Now().After(t.ExpiresAt) {
		return nil, ErrVerificationExpired
	}
	now := time.Now()
	n, err := s.d.Ent.EmailVerificationToken.Update().
		Where(entevt.IDEQ(t.ID), entevt.UsedAtIsNil()).
		SetUsedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// Another caller consumed it between our SELECT and UPDATE.
		return nil, ErrVerificationUsed
	}
	t.UsedAt = &now
	return t, nil
}

func (s *VerificationSvc) create(ctx context.Context, uid uuid.UUID, kind entevt.Kind, pendingEmail string, ttl time.Duration, ip string) (*ent.EmailVerificationToken, error) {
	// Revoke any previous unused token of the same (user, kind) so an
	// attacker can't replay an old link after the user re-issued.
	_, _ = s.d.Ent.EmailVerificationToken.Update().
		Where(entevt.UserIDEQ(uid), entevt.KindEQ(kind), entevt.UsedAtIsNil()).
		SetUsedAt(time.Now()).
		Save(ctx)

	code, err := randCode(32)
	if err != nil {
		return nil, err
	}
	return s.d.Ent.EmailVerificationToken.Create().
		SetUserID(uid).
		SetKind(kind).
		SetCode(code).
		SetPendingEmail(pendingEmail).
		SetExpiresAt(time.Now().Add(ttl)).
		SetIP(ip).
		Save(ctx)
}

// sendVerifyEmail composes + sends the register / change_email link.
// We use a single template for both: the click flow on the frontend
// is identical (POST /verify-email with the code).
func (s *VerificationSvc) sendVerifyEmail(ctx context.Context, toEmail string, tok *ent.EmailVerificationToken) error {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return err
	}
	link := s.buildLink(cfg.SiteURL, "/verify-email", tok.Code)
	link += "&kind=" + url.QueryEscape(string(tok.Kind))
	subject, body := VerificationEmail(cfg.SiteName, link)
	return s.email.Send(ctx, toEmail, subject, body)
}

func (s *VerificationSvc) buildLink(siteURL, path, code string) string {
	base := strings.TrimRight(siteURL, "/")
	if base == "" {
		base = "http://localhost"
	}
	q := url.Values{}
	q.Set("code", code)
	return fmt.Sprintf("%s%s?%s", base, path, q.Encode())
}

// Vacuum drops tokens that have been expired for more than retain.
// Wired by main as a periodic cron (or one-shot at startup).
func (s *VerificationSvc) Vacuum(ctx context.Context, retain time.Duration) (int, error) {
	cutoff := time.Now().Add(-retain)
	return s.d.Ent.EmailVerificationToken.Delete().
		Where(entevt.ExpiresAtLT(cutoff)).
		Exec(ctx)
}

func randCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func numericCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = registerCodeDigits[int(b[i])%len(registerCodeDigits)]
	}
	return string(b), nil
}

func registerCodeKey(email string) string {
	return "auth:register_code:" + strings.ToLower(strings.TrimSpace(email))
}
