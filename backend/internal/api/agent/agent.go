// Package agent serves /v1/agent/* — endpoints reachable by users
// holding the "agent" functional role. Admin (primary role) also
// passes via the middleware's implicit override so admins can
// trial-drive the agent surface during onboarding.
//
// Every agent endpoint operates against the calling user's own
// agent_id; cross-agent access is admin-only via /v1/admin/agents/*.
package agent

import (
	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	agents *biz.AgentSvc
	redeem *biz.RedeemSvc
}

func New(agents *biz.AgentSvc, redeem *biz.RedeemSvc) *Handler {
	return &Handler{agents: agents, redeem: redeem}
}

func (h *Handler) Mount(rg *gin.RouterGroup) {
	rg.GET("/profile", h.getProfile)
	rg.GET("/dashboard", h.getDashboard)
	rg.GET("/redeem-codes", h.listMyCodes)
	rg.POST("/redeem-codes", h.generateCodes)
	rg.GET("/sales", h.listSales)
	rg.GET("/settlements", h.listSettlements)
	rg.GET("/refunds", h.listRefunds)
	rg.GET("/refund-preview", h.previewRefund)
	rg.POST("/refund", h.registerRefund)
	rg.GET("/coupon", h.getCoupon)
	rg.POST("/coupon", h.createCoupon)
	rg.PATCH("/coupon", h.updateCoupon)
	rg.POST("/withdraw", middleware.SessionOnly(), h.withdrawToBalance)
}
