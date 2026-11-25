package llm

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// agentTurnHeader tags a gateway request with the JuCode Desktop agent turn
// it belongs to, so its charge lands on that agent_turns row.
const agentTurnHeader = "X-JuCode-Turn"

// agentTurnIDFrom returns the request's turn id, or "" when the header is
// absent / malformed or the caller is not an OAuth device (API-key callers
// cannot tag turns).
func agentTurnIDFrom(c *gin.Context) string {
	if middleware.AuthzIDFrom(c) == nil {
		return ""
	}
	v := c.GetHeader(agentTurnHeader)
	if !biz.AgentTurnHeaderRe.MatchString(v) {
		return ""
	}
	return v
}

// recordAgentTurn adds a charged request to its agent turn. Asynchronous and
// best-effort; see BillingSvc.RecordAgentTurnCharge.
func (g *Gateway) recordAgentTurn(userID uuid.UUID, lc logContext, route *biz.Route, cost decimal.Decimal) {
	if lc.TurnID == "" || lc.AuthorizationID == nil {
		return
	}
	group := ""
	if route != nil && route.Group != nil {
		group = route.Group.Name
	}
	g.billing.RecordAgentTurnCharge(userID, *lc.AuthorizationID, lc.TurnID, group, cost)
}
