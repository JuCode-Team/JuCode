// admin_routing.go: live routing/cooldown views for incident triage —
// "which accounts are usable right now, and where would a given model route".
// The views read the account pool (health + the Redis runtime-cooldown keys)
// and the routing resolver; nothing here mutates routing configuration.
package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

type cooldownView struct {
	Active     bool   `json:"active"`
	Reason     string `json:"reason,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type routingBoardProvider struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Enabled      bool         `json:"enabled"`
	Health       string       `json:"health"`
	LastHealthAt *string      `json:"last_health_at,omitempty"`
	Cooldown     cooldownView `json:"cooldown"`
}

// handleRoutingBoard lists every account with its current health and live
// runtime-cooldown state (reason + remaining TTL) so an operator can see at a
// glance which accounts are cooled/unhealthy during an incident.
func (h *Handler) handleRoutingBoard(c *gin.Context) {
	ctx := c.Request.Context()
	if h.accounts == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []routingBoardProvider{}})
		return
	}
	accts, err := h.accounts.AllAccounts(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	sort.SliceStable(accts, func(i, j int) bool { return accts[i].Name < accts[j].Name })
	out := make([]routingBoardProvider, 0, len(accts))
	for _, acct := range accts {
		row := routingBoardProvider{
			ID:      acct.ID.String(),
			Name:    acct.Name,
			Kind:    accountLegacyKind(acct),
			Enabled: acct.Status == entaccount.StatusActive && acct.Schedulable,
			Health:  string(acct.Health),
		}
		if acct.LastErrorAt != nil {
			s := acct.LastErrorAt.UTC().Format("2006-01-02T15:04:05Z07:00")
			row.LastHealthAt = &s
		}
		if active, reason, ttl, cerr := h.accounts.CooldownInfo(ctx, acct.ID); cerr == nil && active {
			row.Cooldown = cooldownView{Active: true, Reason: reason, TTLSeconds: int(ttl.Seconds())}
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

type routingPreviewProvider struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Kind            string       `json:"kind"`
	Health          string       `json:"health"`
	Priority        int          `json:"priority"`
	Weight          int          `json:"weight"`
	EffectiveWeight int          `json:"effective_weight"`
	UpstreamModel   string       `json:"upstream_model"`
	Cooldown        cooldownView `json:"cooldown"`
	Eligible        bool         `json:"eligible"` // enabled, resolved, and not currently cooled
}

type routingPreviewGroup struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	BillingSource  string                   `json:"billing_source"`
	RateMultiplier float64                  `json:"rate_multiplier"`
	Chosen         bool                     `json:"chosen"`
	SkipReason     string                   `json:"skip_reason,omitempty"`
	Providers      []routingPreviewProvider `json:"providers"`
}

// handleRoutingPreview shows where a given model would route right now: the
// configured candidate order (priority tier + weighted shuffle inputs) with
// live cooldown overlaid, so operators can verify config + see who's skipped.
// "groups" carries the whole auto-route fall-through chain, not just the
// group that currently wins. The JSON field names are frozen: the frontend
// still reads "providers" even though the rows are now accounts.
func (h *Handler) handleRoutingPreview(c *gin.Context) {
	model := c.Query("model")
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model query param required"})
		return
	}
	protocol, ok := biz.ParseRequestProtocol(c.Query("protocol"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "protocol must be openai or anthropic"})
		return
	}
	ctx := c.Request.Context()
	preview, err := h.models.RoutePreview(ctx, model, protocol)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Cooldown is read once per account and shared by the chosen-group list
	// and the per-group chain, which repeat the same accounts.
	cooldowns := map[string]cooldownView{}
	overlay := func(rows []biz.RoutePreviewProvider) []routingPreviewProvider {
		out := make([]routingPreviewProvider, 0, len(rows))
		for _, p := range rows {
			row := routingPreviewProvider{
				ID:              p.ID,
				Name:            p.Name,
				Kind:            p.Kind,
				Health:          p.Health,
				Priority:        p.Priority,
				Weight:          p.Weight,
				EffectiveWeight: p.EffectiveWeight,
				UpstreamModel:   p.UpstreamModel,
				Eligible:        true,
			}
			cd, ok := cooldowns[p.ID]
			if !ok {
				if id, perr := uuid.Parse(p.ID); perr == nil && h.accounts != nil {
					if active, reason, ttl, cerr := h.accounts.CooldownInfo(ctx, id); cerr == nil && active {
						cd = cooldownView{Active: true, Reason: reason, TTLSeconds: int(ttl.Seconds())}
					}
				}
				cooldowns[p.ID] = cd
			}
			if cd.Active {
				row.Cooldown = cd
				row.Eligible = false
			}
			out = append(out, row)
		}
		return out
	}

	groups := make([]routingPreviewGroup, 0, len(preview.Groups))
	for _, g := range preview.Groups {
		groups = append(groups, routingPreviewGroup{
			ID:             g.ID,
			Name:           g.Name,
			BillingSource:  g.BillingSource,
			RateMultiplier: g.RateMultiplier,
			Chosen:         g.Chosen,
			SkipReason:     g.SkipReason,
			Providers:      overlay(g.Providers),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"model":     model,
		"protocol":  protocol,
		"group":     preview.Group,
		"group_id":  preview.GroupID,
		"providers": overlay(preview.Providers),
		"groups":    groups,
	})
}

// maxManualCooldown bounds an operator-supplied cooldown. Automatic
// cooldowns are all well under it (auth-style dead keys top out at 10m); the
// cap exists so a mistyped seconds value cannot quietly remove an account from
// rotation for a week.
const maxManualCooldown = 24 * time.Hour

type setCooldownRequest struct {
	Seconds int    `json:"seconds"`
	Reason  string `json:"reason"`
}

// cooldownSeconds resolves the requested cooldown length from the body's
// `seconds` field, falling back to the `?seconds=` query param (same
// convenience the account-cooldown endpoint offers), and enforces the bounds.
// Pure so the matrix is testable without an HTTP harness.
func cooldownSeconds(bodySeconds int, querySeconds string) (time.Duration, error) {
	seconds := bodySeconds
	if seconds <= 0 {
		if v, err := strconv.Atoi(querySeconds); err == nil {
			seconds = v
		}
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("seconds must be > 0")
	}
	d := time.Duration(seconds) * time.Second
	if d > maxManualCooldown {
		return 0, fmt.Errorf("seconds must be <= %d", int(maxManualCooldown.Seconds()))
	}
	return d, nil
}
