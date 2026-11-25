// Package biz contains domain logic. It depends on data (ent + redis) but
// never on HTTP/Gin. Handlers in internal/api are thin shells over this.
package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entevt "github.com/jucode/jucode/internal/data/ent/emailverificationtoken"
	entpat "github.com/jucode/jucode/internal/data/ent/personalaccesstoken"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/pkg/jwt"
	"github.com/jucode/jucode/internal/pkg/pwd"
)

var (
	ErrEmailExists                  = errors.New("email already registered")
	ErrInvalidCredential            = errors.New("invalid email or password")
	ErrUserBanned                   = errors.New("user is banned")
	ErrUserPendingApproval          = errors.New("account is pending approval")
	ErrUserRejected                 = errors.New("registration was rejected")
	ErrInvalidRefresh               = errors.New("invalid or expired refresh token")
	ErrRegistrationDisabled         = errors.New("registration is disabled")
	ErrRegisterVerificationDisabled = errors.New("register verification is disabled")
	ErrEmailAlreadyVerified         = errors.New("email already verified")
)

type Account struct {
	d        *data.Data
	signer   *jwt.Signer
	settings *SettingsSvc
	verify   *VerificationSvc // optional; nil when email feature disabled
	invites  *InviteSvc       // optional; nil when invite program not wired
}

func NewAccount(d *data.Data, signer *jwt.Signer, settings *SettingsSvc) *Account {
	return &Account{d: d, signer: signer, settings: settings}
}

// SetVerification injects the verification service after construction
// (Account is built before VerificationSvc which itself needs
// SettingsSvc).
func (a *Account) SetVerification(v *VerificationSvc) { a.verify = v }

// SetInvites injects the invite service (same late-wiring pattern as
// SetVerification: InviteSvc is built after Account).
func (a *Account) SetInvites(inv *InviteSvc) { a.invites = inv }

func (a *Account) SendRegisterVerifyCode(ctx context.Context, email, ip string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return errors.New("email required")
	}
	cfg, err := a.settings.GetSite(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if !cfg.RegistrationEnabled {
		return ErrRegistrationDisabled
	}
	if !cfg.RequireRegisterVerification {
		return ErrRegisterVerificationDisabled
	}
	if a.verify == nil {
		return ErrEmailDisabled
	}
	exists, err := a.d.Ent.User.Query().Where(entuser.EmailEQ(email)).Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		return ErrEmailExists
	}
	return a.verify.IssueRegisterCode(ctx, email, ip)
}

// RegisterResult tells the API handler whether to mint tokens or just
// confirm "submitted, awaiting approval".
type RegisterResult struct {
	User            *ent.User
	PendingApproval bool
}

type TokenPair struct {
	Access           string
	AccessExpiresAt  time.Time
	Refresh          string
	RefreshExpiresAt time.Time
}

// RegisterInput carries everything Register needs. InviteCode/IP/UserAgent
// are optional; they only feed the invite-binding side channel.
type RegisterInput struct {
	Email      string
	Password   string
	Nickname   string
	VerifyCode string
	InviteCode string
	IP         string
	UserAgent  string
}

// Register creates a new user. Email is lowercased before storage.
//
// Behavior depends on SiteSettings:
//   - RegistrationEnabled=false → ErrRegistrationDisabled (no row created)
//   - RequireRegisterVerification=true → verifyCode must match before row creation
//   - RegistrationRequiresApproval=true → verified row created with status="pending_approval"
//   - otherwise → row created with status="active" (legacy default)
//
// A provided invite code is validated before the row is created: a
// malformed or unknown code fails the registration with
// ErrInviteCodeMalformed / ErrInviteCodeUnknown. It cannot be waved
// through, because invite_bindings.invitee_user_id is unique and the
// binding is only ever written here — a code dropped at this point can
// never be re-attached. Everything else invite-side stays best-effort
// and never fails the registration.
func (a *Account) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	password, nickname, verifyCode := in.Password, in.Nickname, in.VerifyCode
	if email == "" || password == "" {
		return nil, errors.New("email and password required")
	}

	cfg, err := a.settings.GetSite(ctx)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if !cfg.RegistrationEnabled {
		return nil, ErrRegistrationDisabled
	}
	// Before the verification code below, which consumes its Redis key on
	// success: burning the visitor's code and then refusing the registration
	// would force them to request a new one just to fix a typo.
	if in.InviteCode != "" && a.invites != nil {
		if _, err := a.invites.ResolveInviter(ctx, in.InviteCode); err != nil {
			return nil, err
		}
	}
	if cfg.RequireRegisterVerification {
		if a.verify == nil {
			return nil, ErrEmailDisabled
		}
		if err := a.verify.VerifyRegisterCode(ctx, email, verifyCode); err != nil {
			return nil, err
		}
	}

	hash, err := pwd.Hash(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	status := entuser.StatusActive
	if cfg.RegistrationRequiresApproval {
		status = entuser.StatusPendingApproval
	}

	q := a.d.Ent.User.Create().
		SetEmail(email).
		SetPasswordHash(hash).
		SetStatus(status).
		SetRegisterIP(in.IP)
	if cfg.RequireRegisterVerification {
		now := time.Now()
		q = q.SetEmailVerified(true).SetEmailVerifiedAt(now)
	}
	if nickname != "" {
		q = q.SetNickname(nickname)
	}
	u, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, ErrEmailExists
		}
		return nil, err
	}
	if a.invites != nil {
		// Both are best-effort: the account exists either way.
		if _, err := a.invites.EnsureInviteCode(ctx, u.ID); err != nil {
			slog.Warn("register: invite code generation failed", "user_id", u.ID.String(), "err", err.Error())
		}
		if in.InviteCode != "" {
			if err := a.invites.BindOnRegister(ctx, u.ID, in.InviteCode, in.IP, in.UserAgent); err != nil {
				slog.Warn("register: invite bind failed", "user_id", u.ID.String(), "err", err.Error())
			}
		}
	}
	res := &RegisterResult{
		User:            u,
		PendingApproval: status == entuser.StatusPendingApproval,
	}
	return res, nil
}

// Login verifies credentials. Identifier may be an email (contains '@')
// or a bare username — usernames came from the newapi import where many
// users never set an email. Both stored lowercased so case doesn't gate
// authentication.
//
// When the account has TOTP enabled, no tokens are issued: the result
// carries an MFAToken instead and the caller must complete the second
// step via CompleteTOTPLogin.
func (a *Account) Login(ctx context.Context, identifier, password string) (*LoginResult, error) {
	ident := strings.ToLower(strings.TrimSpace(identifier))
	q := a.d.Ent.User.Query()
	if strings.Contains(ident, "@") {
		q = q.Where(entuser.EmailEQ(ident))
	} else {
		q = q.Where(entuser.UsernameEQ(ident))
	}
	u, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrInvalidCredential
		}
		return nil, err
	}
	switch u.Status {
	case entuser.StatusBanned:
		return nil, ErrUserBanned
	case entuser.StatusPendingApproval:
		return nil, ErrUserPendingApproval
	case entuser.StatusRejected:
		return nil, ErrUserRejected
	}
	// Verify returns nil on match. ErrNeedsRehash also means "match", but
	// signals "your stored hash predates the pre-sha256 scheme — upgrade
	// it now while we're holding the plaintext."
	verr := pwd.Verify(u.PasswordHash, password)
	if verr != nil && !errors.Is(verr, pwd.ErrNeedsRehash) {
		return nil, ErrInvalidCredential
	}
	if errors.Is(verr, pwd.ErrNeedsRehash) {
		if newHash, herr := pwd.Hash(password); herr == nil {
			_, _ = a.d.Ent.User.UpdateOneID(u.ID).SetPasswordHash(newHash).Save(ctx)
		}
	}
	if u.TotpEnabled {
		ticket, terr := a.issueMFATicket(ctx, u.ID)
		if terr != nil {
			return nil, terr
		}
		return &LoginResult{User: u, MFAToken: ticket}, nil
	}
	pair, err := a.issuePair(ctx, u.ID, string(u.Role))
	if err != nil {
		return nil, err
	}
	return &LoginResult{
		User:   u,
		Tokens: pair,
		// Privileged accounts without TOTP keep working during the
		// migration window but the frontend steers them to bind.
		TOTPSetupRequired: a.TOTPRequiredForUser(ctx, u),
	}, nil
}

// RecordLogin stamps the client IP of a completed login (tokens issued).
// Best-effort: a failed write must not fail the login itself. Repeat logins
// from the same IP within an hour skip the write.
func (a *Account) RecordLogin(ctx context.Context, u *ent.User, ip string) {
	if ip == "" || (u.LastLoginIP == ip && u.LastLoginAt != nil && time.Since(*u.LastLoginAt) < time.Hour) {
		return
	}
	if err := a.d.Ent.User.UpdateOneID(u.ID).
		SetLastLoginIP(ip).
		SetLastLoginAt(time.Now()).
		Exec(ctx); err != nil {
		slog.Warn("login: record ip failed", "user_id", u.ID.String(), "err", err.Error())
	}
}

// Refresh validates the refresh token (signature + jti allow-set), then
// issues a new pair AND invalidates the old refresh's jti (rotation).
func (a *Account) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	c, err := a.signer.Parse(refreshToken)
	if err != nil || c.Kind != jwt.KindRefresh {
		return nil, ErrInvalidRefresh
	}
	// Verify jti is still allow-listed.
	key := refreshKey(c.ID)
	val, err := a.d.Redis.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrInvalidRefresh
		}
		return nil, err
	}
	if val != c.UserID.String() {
		return nil, ErrInvalidRefresh
	}
	// Rotate: delete old jti, issue new pair.
	_ = a.d.Redis.Del(ctx, key).Err()
	if err := a.removeJTIFromUserIndex(ctx, c.UserID, c.ID); err != nil {
		return nil, err
	}

	// Re-fetch user to grab current role/status (could have been promoted/banned).
	u, err := a.d.Ent.User.Get(ctx, c.UserID)
	if err != nil {
		return nil, ErrInvalidRefresh
	}
	switch u.Status {
	case entuser.StatusBanned:
		return nil, ErrUserBanned
	case entuser.StatusPendingApproval:
		return nil, ErrUserPendingApproval
	case entuser.StatusRejected:
		return nil, ErrUserRejected
	}
	return a.issuePair(ctx, u.ID, string(u.Role))
}

// Logout invalidates a single refresh token by jti.
func (a *Account) Logout(ctx context.Context, refreshToken string) error {
	c, err := a.signer.Parse(refreshToken)
	if err != nil || c.Kind != jwt.KindRefresh {
		return nil // treat invalid tokens as already-logged-out
	}
	_ = a.d.Redis.Del(ctx, refreshKey(c.ID)).Err()
	return a.removeJTIFromUserIndex(ctx, c.UserID, c.ID)
}

// IsUserActive returns true when the user row's status == active. Used
// by the JWT middleware so a banned / pending_approval / rejected user
// can't keep using a still-valid access token (JWT is stateless; this
// adds a per-request status freshness check). Cached in Redis for 60s
// to keep the per-request DB cost negligible — admin bans propagate to
// auth gates within a minute, which is acceptable for the security
// model (refresh tokens are revoked synchronously on RevokeAll, only
// the in-flight access token gets a brief grace period).
func (a *Account) IsUserActive(ctx context.Context, userID uuid.UUID) (bool, error) {
	const ttl = 60 * time.Second
	key := "auth:active:" + userID.String()
	if v, err := a.d.Redis.Get(ctx, key).Result(); err == nil {
		return v == "1", nil
	}
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	active := u.Status == entuser.StatusActive
	val := "0"
	if active {
		val = "1"
	}
	_ = a.d.Redis.Set(ctx, key, val, ttl).Err()
	return active, nil
}

// InvalidateUserActiveCache wipes the auth:active:<uid> entry so the
// next JWT request observes the latest status without waiting out the
// TTL. Called after ban / approve / reject / role-change paths.
func (a *Account) InvalidateUserActiveCache(ctx context.Context, userID uuid.UUID) {
	_ = a.d.Redis.Del(ctx, "auth:active:"+userID.String()).Err()
}

// RevokeAll invalidates every refresh token and personal access token for
// the given user (used on password change, password reset, email change).
// A failure revoking one kind does not stop revoking the other.
func (a *Account) RevokeAll(ctx context.Context, userID uuid.UUID) error {
	// Revocation must finish even if the request that changed the password
	// is cancelled right after that change committed.
	ctx = context.WithoutCancel(ctx)
	tokenErr := a.d.Ent.PersonalAccessToken.Update().
		Where(entpat.UserIDEQ(userID), entpat.RevokedAtIsNil()).
		SetRevokedAt(time.Now()).
		Exec(ctx)
	return errors.Join(tokenErr, a.revokeRefreshTokens(ctx, userID))
}

func (a *Account) revokeRefreshTokens(ctx context.Context, userID uuid.UUID) error {
	idxKey := userJTIIndexKey(userID)
	jtis, err := a.d.Redis.SMembers(ctx, idxKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	pipe := a.d.Redis.Pipeline()
	for _, jti := range jtis {
		pipe.Del(ctx, refreshKey(jti))
	}
	pipe.Del(ctx, idxKey)
	_, err = pipe.Exec(ctx)
	return err
}

func (a *Account) issuePair(ctx context.Context, userID uuid.UUID, role string) (*TokenPair, error) {
	access, _, accessExp, err := a.signer.IssueAccess(userID, role)
	if err != nil {
		return nil, err
	}
	refresh, refreshJTI, refreshExp, err := a.signer.IssueRefresh(userID, role)
	if err != nil {
		return nil, err
	}
	// Persist refresh jti -> user_id with the same TTL as the token.
	ttl := time.Until(refreshExp)
	if ttl <= 0 {
		return nil, errors.New("refresh ttl non-positive")
	}
	pipe := a.d.Redis.Pipeline()
	pipe.Set(ctx, refreshKey(refreshJTI), userID.String(), ttl)
	pipe.SAdd(ctx, userJTIIndexKey(userID), refreshJTI)
	pipe.Expire(ctx, userJTIIndexKey(userID), a.signer.RefreshTTL()+1*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return &TokenPair{
		Access:           access,
		AccessExpiresAt:  accessExp,
		Refresh:          refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

func (a *Account) removeJTIFromUserIndex(ctx context.Context, userID uuid.UUID, jti string) error {
	return a.d.Redis.SRem(ctx, userJTIIndexKey(userID), jti).Err()
}

func refreshKey(jti string) string         { return "refresh:jti:" + jti }
func userJTIIndexKey(uid uuid.UUID) string { return "refresh:user:" + uid.String() }

// --- profile mutations ---

// ChangePassword verifies oldPassword against the stored hash and
// swaps in a new one. All existing refresh tokens are revoked so a
// password change kicks the user out of every other session.
func (a *Account) ChangePassword(ctx context.Context, userID uuid.UUID, oldPwd, newPwd string) error {
	if newPwd == "" {
		return errors.New("new password required")
	}
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	// Match (including legacy-hash match) is acceptable; we'll persist a
	// fresh hash below regardless, so no separate rehash step needed.
	verr := pwd.Verify(u.PasswordHash, oldPwd)
	if verr != nil && !errors.Is(verr, pwd.ErrNeedsRehash) {
		return ErrInvalidCredential
	}
	hash, err := pwd.Hash(newPwd)
	if err != nil {
		return err
	}
	if _, err := a.d.Ent.User.UpdateOneID(userID).SetPasswordHash(hash).Save(ctx); err != nil {
		return err
	}
	if err := a.RevokeAll(ctx, userID); err != nil {
		slog.Warn("revoke sessions and access tokens", "user_id", userID, "err", err)
	}
	return nil
}

// ResetUserPassword sets a user's password without requiring the old
// password. It is for trusted operator workflows only; callers must enforce
// admin/cs authorization before invoking it.
func (a *Account) ResetUserPassword(ctx context.Context, userID uuid.UUID, newPwd string) error {
	if newPwd == "" {
		return errors.New("new password required")
	}
	hash, err := pwd.Hash(newPwd)
	if err != nil {
		return err
	}
	if _, err := a.d.Ent.User.UpdateOneID(userID).SetPasswordHash(hash).Save(ctx); err != nil {
		return err
	}
	if err := a.RevokeAll(ctx, userID); err != nil {
		slog.Warn("revoke sessions and access tokens", "user_id", userID, "err", err)
	}
	return nil
}

// RequestEmailVerification sends a link to an existing user's current email.
func (a *Account) RequestEmailVerification(ctx context.Context, user *ent.User, ip string) error {
	if user.EmailVerified {
		return ErrEmailAlreadyVerified
	}
	if a.verify == nil {
		return ErrEmailDisabled
	}
	return a.verify.IssueRegister(ctx, user, ip)
}

// RequestEmailChange validates that newEmail isn't already taken,
// then dispatches a verification email to it. The user's stored
// email doesn't change until they click the link and we
// ConsumeEmailChange. Verification is delegated to the same flow as
// register-time verification.
//
// When SiteSettings.RequireChangeEmailVerification = false we still
// run the verify-email flow (UX-wise users expect the link); admins
// who want to skip verification can set this flag to false AND
// route the call to UpdateEmailNoVerify (admin-only path).
func (a *Account) RequestEmailChange(ctx context.Context, user *ent.User, newEmail, ip string) error {
	if a.verify == nil {
		return ErrEmailDisabled
	}
	newEmail = strings.ToLower(strings.TrimSpace(newEmail))
	if newEmail == "" {
		return errors.New("new email required")
	}
	if newEmail == strings.ToLower(user.Email) {
		return errors.New("new email is the same as current")
	}
	// Uniqueness pre-check (token-stage; final check is at consume time too)
	exists, err := a.d.Ent.User.Query().Where(entuser.EmailEQ(newEmail)).Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		return ErrEmailExists
	}
	return a.verify.IssueChangeEmail(ctx, user, newEmail, ip)
}

// ConsumeEmailChange flips the user's email after they click the link.
// Re-checks uniqueness because someone could have signed up with the
// pending address in the meantime.
func (a *Account) ConsumeEmailChange(ctx context.Context, code string) (*ent.User, error) {
	if a.verify == nil {
		return nil, ErrEmailDisabled
	}
	t, err := a.verify.Consume(ctx, code, "change_email")
	if err != nil {
		return nil, err
	}
	exists, err := a.d.Ent.User.Query().Where(entuser.EmailEQ(t.PendingEmail)).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrEmailExists
	}
	now := time.Now()
	u, err := a.d.Ent.User.UpdateOneID(t.UserID).
		SetEmail(t.PendingEmail).
		SetEmailVerified(true).
		SetEmailVerifiedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	// Force re-login because the email = identity changed.
	if err := a.RevokeAll(ctx, t.UserID); err != nil {
		slog.Warn("revoke sessions and access tokens", "user_id", t.UserID, "err", err)
	}
	// The new address is verified — may qualify a pending signup reward.
	a.invites.EvaluateSignupAsync(t.UserID)
	a.invites.ReleaseEmailHoldsAsync(t.UserID)
	return u, nil
}

// VerifyEmail uses the stored token type because old links omitted kind.
func (a *Account) VerifyEmail(ctx context.Context, code string) (*ent.User, entevt.Kind, error) {
	token, err := a.d.Ent.EmailVerificationToken.Query().
		Where(entevt.CodeEQ(strings.TrimSpace(code))).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			err = ErrVerificationNotFound
		}
		return nil, "", err
	}
	var user *ent.User
	switch token.Kind {
	case entevt.KindRegister:
		user, err = a.ConsumeRegisterVerification(ctx, code)
	case entevt.KindChangeEmail:
		user, err = a.ConsumeEmailChange(ctx, code)
	default:
		return nil, "", ErrVerificationNotFound
	}
	return user, token.Kind, err
}

// ConsumeRegisterVerification flips email_verified=true and (when the
// user was in pending_approval purely due to the verification flow)
// promotes them to active. Admin-mandated pending_approval (from
// SiteSettings.RegistrationRequiresApproval) is NOT auto-promoted —
// the email verification only proves email ownership, not admin
// approval.
func (a *Account) ConsumeRegisterVerification(ctx context.Context, code string) (*ent.User, error) {
	if a.verify == nil {
		return nil, ErrEmailDisabled
	}
	t, err := a.verify.Consume(ctx, code, "register")
	if err != nil {
		return nil, err
	}
	cfg, err := a.settings.GetSite(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u, err := a.d.Ent.User.Get(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	q := a.d.Ent.User.UpdateOneID(t.UserID).
		SetEmailVerified(true).
		SetEmailVerifiedAt(now)
	// A link sent before an email change cannot verify the new address.
	if t.PendingEmail != "" {
		if u.Email != t.PendingEmail {
			return nil, ErrVerificationNotFound
		}
		q = q.Where(entuser.EmailEQ(t.PendingEmail))
	}
	// Promote to active only if pending was caused by verification
	// (admin-approval flow is independent and stays pending).
	if u.Status == entuser.StatusPendingApproval && !cfg.RegistrationRequiresApproval {
		q = q.SetStatus(entuser.StatusActive)
	}
	res, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	// email_verified may qualify a pending signup reward, and releases
	// this user's own payouts held for their unverified email.
	a.invites.EvaluateSignupAsync(t.UserID)
	a.invites.ReleaseEmailHoldsAsync(t.UserID)
	return res, nil
}

// ForgotPassword dispatches a reset link if the email matches a user.
// Always returns nil (no disclosure of whether the email exists).
func (a *Account) ForgotPassword(ctx context.Context, email, ip string) error {
	if a.verify == nil {
		return nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	u, err := a.d.Ent.User.Query().Where(entuser.EmailEQ(email)).Only(ctx)
	if err != nil {
		return nil // silent on lookup miss
	}
	_ = a.verify.IssueResetPassword(ctx, u, ip)
	return nil
}

// ResetPasswordWithToken validates a reset-password token and sets
// the new password. Revokes all sessions so the attacker is kicked
// out if the link was intercepted.
func (a *Account) ResetPasswordWithToken(ctx context.Context, code, newPwd string) error {
	if a.verify == nil {
		return ErrEmailDisabled
	}
	if newPwd == "" {
		return errors.New("new password required")
	}
	t, err := a.verify.Consume(ctx, code, "reset_password")
	if err != nil {
		return err
	}
	hash, err := pwd.Hash(newPwd)
	if err != nil {
		return err
	}
	if _, err := a.d.Ent.User.UpdateOneID(t.UserID).SetPasswordHash(hash).Save(ctx); err != nil {
		return err
	}
	if err := a.RevokeAll(ctx, t.UserID); err != nil {
		slog.Warn("revoke sessions and access tokens", "user_id", t.UserID, "err", err)
	}
	return nil
}
