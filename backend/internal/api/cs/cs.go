// Package cs serves /v1/cs/* — endpoints reachable by users holding the
// "cs" functional role (customer-service agents) or admin (implicit
// override). The Handler owns the shared dependency wiring; per-domain
// handlers live in sibling files (cs_tickets.go, cs_users.go).
package cs

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	users   *ent.Client
	tickets *biz.TicketSvc
	plans   *biz.PlanSvc
	apikeys *biz.ApiKeySvc
	roles   *biz.RoleSvc
	account *biz.Account
	finance *biz.FinanceSvc
	rawDB   *sql.DB
	// stepUp guards refund approval + balance adjustment with a fresh
	// TOTP check. Wired via SetStepUpGuard; nil = no gate (tests).
	stepUp gin.HandlerFunc
}

func New(users *ent.Client, tickets *biz.TicketSvc, plans *biz.PlanSvc, apikeys *biz.ApiKeySvc, roles *biz.RoleSvc, account *biz.Account, finance *biz.FinanceSvc, rawDB *sql.DB) *Handler {
	return &Handler{users: users, tickets: tickets, plans: plans, apikeys: apikeys, roles: roles, account: account, finance: finance, rawDB: rawDB}
}

// SetStepUpGuard wires the TOTP step-up middleware for sensitive routes.
func (h *Handler) SetStepUpGuard(fn gin.HandlerFunc) { h.stepUp = fn }

func (h *Handler) stepUpGuard() gin.HandlerFunc {
	if h.stepUp != nil {
		return h.stepUp
	}
	return func(c *gin.Context) { c.Next() }
}

func (h *Handler) Mount(rg *gin.RouterGroup) {
	rg.GET("/tickets", h.listTickets)
	rg.GET("/tickets/counts", h.ticketCounts)
	rg.GET("/tickets/assignees", h.ticketAssignees)
	rg.GET("/tickets/:id", h.getTicket)
	rg.GET("/tickets/:id/attachments/:file", h.getTicketAttachment)
	rg.POST("/tickets/:id/reply", h.replyTicket)
	rg.POST("/tickets/:id/assign", h.assignTicket)
	rg.POST("/tickets/:id/status", h.setTicketStatus)
	rg.POST("/tickets/:id/refunds", h.createTicketRefund)
	rg.GET("/orders/:id/refundable", h.orderRefundable)

	rg.GET("/refunds", h.listRefunds)
	rg.POST("/refunds/:id/approve", h.stepUpGuard(), h.approveRefund)
	rg.POST("/refunds/:id/reject", h.stepUpGuard(), h.rejectRefund)

	rg.GET("/users", h.listUsers)
	rg.GET("/users/:id", h.getUserDetail)
	rg.POST("/users/:id/balance/adjust", h.stepUpGuard(), h.adjustBalance)
	// Setting someone's password yields a login that outlives any token.
	rg.POST("/users/:id/reset-password", middleware.SessionOnly(), h.resetUserPassword)
	rg.POST("/users/:id/plans/:user_plan_id/cancel", h.cancelUserPlan)
	rg.POST("/users/:id/plans/:user_plan_id/extend", h.extendUserPlan)
}
