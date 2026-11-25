package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// StepUpVerifier is the slice of biz.Account the step-up gate needs.
// Declared here so the middleware package doesn't grow a biz dependency
// beyond what auth.go already has.
type StepUpVerifier interface {
	IsTOTPEnabled(ctx context.Context, userID uuid.UUID) (bool, error)
	StepUpSatisfied(ctx context.Context, userID uuid.UUID) bool
	StepUpVerify(ctx context.Context, userID uuid.UUID, code string) error
}

// RequireTOTPStepUp guards sensitive mutations (balance adjustments,
// refund approvals, redeem-code minting). Must
// run AFTER JWT().
//
// Accounts WITH TOTP bound must present a fresh code in the X-TOTP-Code
// header; one successful check opens a short window (see biz.stepUpTTL)
// so a burst of related actions doesn't prompt on every click.
//
// Accounts WITHOUT TOTP pass through: this is the migration grace period.
// The login/settings nudge steers them to bind; hard-failing here would
// lock every existing admin out of day-to-day operations at deploy time.
func RequireTOTPStepUp(v StepUpVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := UserIDFrom(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
			return
		}
		// A personal access token never passes step-up: not with a code, not
		// inside the owner's recent-verification window, and not when the
		// owner has no two-factor at all.
		if _, viaToken := c.Get(CtxAccessTokenID); viaToken {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "this action requires a signed-in session, not an access token",
				"code":  "session_required",
			})
			return
		}
		ctx := c.Request.Context()
		enabled, err := v.IsTOTPEnabled(ctx, uid)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "step-up check failed"})
			return
		}
		if !enabled {
			c.Next()
			return
		}
		if v.StepUpSatisfied(ctx, uid) {
			c.Next()
			return
		}
		code := strings.TrimSpace(c.GetHeader("X-TOTP-Code"))
		if code == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "this action requires a fresh two-factor code",
				"code":  "totp_stepup_required",
			})
			return
		}
		if err := v.StepUpVerify(ctx, uid, code); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid two-factor code",
				"code":  "totp_invalid_code",
			})
			return
		}
		c.Next()
	}
}
