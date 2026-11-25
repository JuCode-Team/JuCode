// totp.go: TOTP two-factor auth on top of Account. Covers the whole
// lifecycle — setup (pending secret), confirm (enable + recovery codes),
// the second login step (MFA ticket), recovery-code fallback, step-up
// re-verification for sensitive admin actions, and disable.
package biz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserrole "github.com/jucode/jucode/internal/data/ent/userrole"
	"github.com/jucode/jucode/internal/pkg/pwd"
	"github.com/jucode/jucode/internal/pkg/totp"
)

var (
	ErrTOTPAlreadyEnabled = errors.New("totp already enabled")
	ErrTOTPNotEnabled     = errors.New("totp not enabled")
	ErrTOTPNoPendingSetup = errors.New("no pending totp setup")
	ErrTOTPInvalidCode    = errors.New("invalid totp code")
	ErrMFATicketInvalid   = errors.New("invalid or expired mfa ticket")
)

const (
	// mfaTicketTTL bounds the gap between password success and code entry.
	mfaTicketTTL = 5 * time.Minute
	// mfaMaxAttempts: wrong codes allowed per ticket before it's burned
	// and the user must re-enter the password.
	mfaMaxAttempts = 5
	// stepUpTTL is how long one successful step-up verification covers
	// subsequent sensitive actions in the same session.
	stepUpTTL = 10 * time.Minute
)

func mfaTicketKey(tok string) string   { return "mfa:ticket:" + tok }
func mfaAttemptsKey(tok string) string { return "mfa:attempts:" + tok }
func totpUsedKey(uid uuid.UUID, counter uint64) string {
	return fmt.Sprintf("totp:used:%s:%d", uid.String(), counter)
}
func stepUpKey(uid uuid.UUID) string { return "totp:stepup:" + uid.String() }

// BeginTOTPSetup generates a fresh secret for the user and stores it as a
// pending (unconfirmed) setup. Calling it again before confirm simply
// rotates the pending secret. Returns the secret and the otpauth:// URI
// the frontend renders as a QR code.
func (a *Account) BeginTOTPSetup(ctx context.Context, userID uuid.UUID) (secret, uri string, err error) {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if u.TotpEnabled {
		return "", "", ErrTOTPAlreadyEnabled
	}
	secret, err = totp.GenerateSecret()
	if err != nil {
		return "", "", err
	}
	if _, err := a.d.Ent.User.UpdateOneID(userID).SetTotpSecret(secret).Save(ctx); err != nil {
		return "", "", err
	}
	issuer := siteDefaults.SiteName
	if a.settings != nil {
		if cfg, serr := a.settings.GetSite(ctx); serr == nil && strings.TrimSpace(cfg.SiteName) != "" {
			issuer = cfg.SiteName
		}
	}
	return secret, totp.ProvisioningURI(secret, u.Email, issuer), nil
}

// ConfirmTOTPSetup verifies the first code against the pending secret,
// enables TOTP, and returns the freshly generated recovery codes. The
// plaintext codes exist only in this return value — the DB keeps hashes.
func (a *Account) ConfirmTOTPSetup(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.TotpEnabled {
		return nil, ErrTOTPAlreadyEnabled
	}
	if u.TotpSecret == nil || *u.TotpSecret == "" {
		return nil, ErrTOTPNoPendingSetup
	}
	if err := a.verifyTOTPAgainstSecret(ctx, userID, *u.TotpSecret, code); err != nil {
		return nil, err
	}
	codes, err := totp.GenerateRecoveryCodes(totp.RecoveryCodeCount)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = totp.HashRecoveryCode(c)
	}
	if _, err := a.d.Ent.User.UpdateOneID(userID).
		SetTotpEnabled(true).
		SetTotpConfirmedAt(time.Now()).
		SetTotpRecoveryCodes(hashes).
		Save(ctx); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTOTP turns TOTP off. The caller must prove both the account
// password and a currently valid TOTP code (or an unused recovery code) —
// a hijacked session alone must not be enough to strip the second factor.
func (a *Account) DisableTOTP(ctx context.Context, userID uuid.UUID, password, code string) error {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	if !u.TotpEnabled {
		return ErrTOTPNotEnabled
	}
	if verr := pwd.Verify(u.PasswordHash, password); verr != nil && !errors.Is(verr, pwd.ErrNeedsRehash) {
		return ErrInvalidCredential
	}
	if err := a.verifyTOTPOrRecovery(ctx, u, code); err != nil {
		return err
	}
	_, err = a.d.Ent.User.UpdateOneID(userID).
		SetTotpEnabled(false).
		ClearTotpSecret().
		ClearTotpConfirmedAt().
		ClearTotpRecoveryCodes().
		Save(ctx)
	return err
}

// VerifyTOTP checks a normal 6-digit code for an enabled user, with replay
// protection: each (user, time-step) is accepted at most once.
func (a *Account) VerifyTOTP(ctx context.Context, u *ent.User, code string) error {
	if !u.TotpEnabled || u.TotpSecret == nil {
		return ErrTOTPNotEnabled
	}
	return a.verifyTOTPAgainstSecret(ctx, u.ID, *u.TotpSecret, code)
}

func (a *Account) verifyTOTPAgainstSecret(ctx context.Context, userID uuid.UUID, secret, code string) error {
	ok, counter := totp.Validate(secret, code, time.Now())
	if !ok {
		return ErrTOTPInvalidCode
	}
	// Replay guard: SETNX on the matched counter. TTL covers the whole
	// acceptance window (±1 step) with margin. Redis may be nil in unit
	// tests — replay protection is then skipped, never the code check.
	if a.d.Redis != nil {
		fresh, err := a.d.Redis.SetNX(ctx, totpUsedKey(userID, counter), "1", 3*totp.Period*time.Second).Result()
		if err == nil && !fresh {
			return ErrTOTPInvalidCode
		}
	}
	return nil
}

// ConsumeRecoveryCode validates a recovery code and burns it (one-time).
// Runs in a transaction with a row lock so two concurrent uses of the
// same code cannot both succeed.
func (a *Account) ConsumeRecoveryCode(ctx context.Context, u *ent.User, code string) error {
	if !u.TotpEnabled {
		return ErrTOTPNotEnabled
	}
	hash := totp.HashRecoveryCode(code)
	tx, err := a.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := tx.User.Query().
		Where(entuser.IDEQ(u.ID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return err
	}
	remaining := make([]string, 0, len(locked.TotpRecoveryCodes))
	found := false
	for _, h := range locked.TotpRecoveryCodes {
		if !found && h == hash {
			found = true
			continue
		}
		remaining = append(remaining, h)
	}
	if !found {
		return ErrTOTPInvalidCode
	}
	if _, err := tx.User.UpdateOneID(u.ID).SetTotpRecoveryCodes(remaining).Save(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// TOTPRequiredForUser reports whether the user belongs to a role that we
// steer toward TOTP: primary admin, or any functional role.
// Extensible: flip the returned policy for plain users later without
// schema changes.
func (a *Account) TOTPRequiredForUser(ctx context.Context, u *ent.User) bool {
	if string(u.Role) == "admin" {
		return true
	}
	has, err := a.d.Ent.UserRole.Query().
		Where(entuserrole.UserIDEQ(u.ID)).
		Exist(ctx)
	if err != nil {
		return false
	}
	return has
}

// --- two-step login ---

// LoginResult is what Login hands the HTTP layer. Exactly one of Tokens /
// MFAToken is set: MFAToken means "password ok, now ask for the code".
type LoginResult struct {
	User   *ent.User
	Tokens *TokenPair
	// MFAToken is the opaque short-lived ticket for the second step.
	MFAToken string
	// TOTPSetupRequired: privileged account that has NOT bound TOTP yet.
	// Login proceeds (migration grace period) but the frontend must steer
	// the user to Settings → bind.
	TOTPSetupRequired bool
}

// issueMFATicket stores a single-purpose random ticket → userID mapping.
func (a *Account) issueMFATicket(ctx context.Context, userID uuid.UUID) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := a.d.Redis.Set(ctx, mfaTicketKey(tok), userID.String(), mfaTicketTTL).Err(); err != nil {
		return "", err
	}
	return tok, nil
}

// CompleteTOTPLogin resolves the MFA ticket and verifies either a TOTP
// code or a recovery code, then issues the token pair. Wrong codes burn
// an attempt; after mfaMaxAttempts the ticket dies and the user restarts
// from the password step.
func (a *Account) CompleteTOTPLogin(ctx context.Context, mfaToken, code, recoveryCode string) (*ent.User, *TokenPair, error) {
	uidStr, err := a.d.Redis.Get(ctx, mfaTicketKey(mfaToken)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil, ErrMFATicketInvalid
		}
		return nil, nil, err
	}
	uid, err := uuid.Parse(uidStr)
	if err != nil {
		return nil, nil, ErrMFATicketInvalid
	}
	u, err := a.d.Ent.User.Get(ctx, uid)
	if err != nil {
		return nil, nil, ErrMFATicketInvalid
	}
	// Re-check status: it may have changed between the two steps.
	switch u.Status {
	case entuser.StatusBanned:
		return nil, nil, ErrUserBanned
	case entuser.StatusPendingApproval:
		return nil, nil, ErrUserPendingApproval
	case entuser.StatusRejected:
		return nil, nil, ErrUserRejected
	}
	var verr error
	switch {
	case recoveryCode != "":
		verr = a.ConsumeRecoveryCode(ctx, u, recoveryCode)
	case code != "":
		verr = a.VerifyTOTP(ctx, u, code)
	default:
		verr = ErrTOTPInvalidCode
	}
	if verr != nil {
		if errors.Is(verr, ErrTOTPInvalidCode) {
			attempts, _ := a.d.Redis.Incr(ctx, mfaAttemptsKey(mfaToken)).Result()
			_ = a.d.Redis.Expire(ctx, mfaAttemptsKey(mfaToken), mfaTicketTTL).Err()
			if attempts >= mfaMaxAttempts {
				_ = a.d.Redis.Del(ctx, mfaTicketKey(mfaToken), mfaAttemptsKey(mfaToken)).Err()
				return nil, nil, ErrMFATicketInvalid
			}
		}
		return nil, nil, verr
	}
	_ = a.d.Redis.Del(ctx, mfaTicketKey(mfaToken), mfaAttemptsKey(mfaToken)).Err()
	pair, err := a.issuePair(ctx, u.ID, string(u.Role))
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

// --- step-up for sensitive actions ---

// StepUpSatisfied reports whether the user verified a code recently
// enough (stepUpTTL) to skip re-entering one.
func (a *Account) StepUpSatisfied(ctx context.Context, userID uuid.UUID) bool {
	if a.d.Redis == nil {
		return false
	}
	v, err := a.d.Redis.Get(ctx, stepUpKey(userID)).Result()
	return err == nil && v == "1"
}

// StepUpVerify checks a TOTP code and, on success, opens a stepUpTTL
// window during which further sensitive actions pass without a code.
func (a *Account) StepUpVerify(ctx context.Context, userID uuid.UUID, code string) error {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	if err := a.VerifyTOTP(ctx, u, code); err != nil {
		return err
	}
	if a.d.Redis != nil {
		_ = a.d.Redis.Set(ctx, stepUpKey(userID), "1", stepUpTTL).Err()
	}
	return nil
}

// IsTOTPEnabled is a light read used by the step-up middleware.
func (a *Account) IsTOTPEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.TotpEnabled, nil
}

// verifyTOTPOrRecovery accepts either form for flows (disable) where
// losing the phone must not brick the account.
func (a *Account) verifyTOTPOrRecovery(ctx context.Context, u *ent.User, code string) error {
	normalized := strings.TrimSpace(code)
	if len(totp.NormalizeRecoveryCode(normalized)) == 10 && len(normalized) != 6 {
		return a.ConsumeRecoveryCode(ctx, u, normalized)
	}
	return a.VerifyTOTP(ctx, u, normalized)
}
