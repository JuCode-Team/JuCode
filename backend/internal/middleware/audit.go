package middleware

import (
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
)

// sensitiveQueryKeys is the set of query parameter names whose values
// must NOT land in admin_audit_logs verbatim. We keep the key itself
// (so admins still see which fields were touched) but replace the
// value with "<redacted>".
//
// Currently empty in practice — admin endpoints take secrets via JSON
// body, not query string — but we redact a list of known-sensitive
// names proactively so a future endpoint that forgets to keep secrets
// out of the query doesn't quietly leak them into audit storage.
var sensitiveQueryKeys = map[string]struct{}{
	"password":      {},
	"new_password":  {},
	"old_password":  {},
	"secret":        {},
	"api_key":       {},
	"token":         {},
	"refresh_token": {},
	"access_token":  {},
	"code":          {}, // verification / reset codes
	"reset_code":    {},
}

// redactQuery parses the raw query string and replaces values of any
// well-known sensitive key with "<redacted>". The output is always a
// re-encoded query string; on parse error we fall back to a single
// "<unparseable>" sentinel rather than passing the original through.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		return "<unparseable>"
	}
	for k, vs := range parsed {
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(k)]; !sensitive {
			continue
		}
		for i := range vs {
			vs[i] = "<redacted>"
		}
		parsed[k] = vs
	}
	return parsed.Encode()
}

// AdminAudit logs every mutating admin request after the handler runs.
// We deliberately skip GETs — listing endpoints generate volume but no
// security surface. POST/PUT/PATCH/DELETE land in admin_audit_logs.
//
// Notes:
//   - Body is NOT recorded (it can contain plaintext API keys / new
//     passwords). The path + method + status + query string are usually
//     enough to reconstruct intent. Sensitive endpoints that need their
//     payload audited can write into the payload jsonb directly inside
//     the handler.
//   - Persistence is fire-and-forget so a slow audit insert never
//     blocks the admin response.
func AdminAudit(svc *biz.AuditSvc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		// After the handler has run: pick a deterministic action token
		// from the route pattern (FullPath) rather than the raw URL so
		// audit rows aggregate cleanly per endpoint regardless of
		// route params.
		method := c.Request.Method
		if method == "GET" || method == "HEAD" || method == "OPTIONS" {
			return
		}
		// 5xx is server-side noise, not an admin action worth logging.
		if c.Writer.Status() >= 500 {
			return
		}
		// The admin /v1/admin/* JWT middleware only stores user_id +
		// role in the context (no full *ent.User round-trip per
		// request), so we read CtxUserID directly here.
		actor, ok := UserIDFrom(c)
		if !ok {
			return
		}

		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		action := method + " " + strings.TrimPrefix(path, "/v1/admin")

		target := ""
		for _, p := range c.Params {
			if target != "" {
				target += "/"
			}
			target += p.Key + "=" + p.Value
		}

		payload := map[string]any{
			"status": c.Writer.Status(),
		}
		if q := c.Request.URL.RawQuery; q != "" {
			payload["query"] = redactQuery(q)
		}
		if id, ok := c.Get(CtxAccessTokenID); ok {
			payload["access_token_id"] = id
		}
		if detail, ok := c.Get(CtxAuditDetail); ok {
			if m, ok := detail.(map[string]any); ok {
				for k, v := range m {
					payload[k] = v
				}
			}
		}

		svc.RecordAsync(biz.AuditEntry{
			ActorUserID: actor,
			Action:      action,
			Target:      target,
			Payload:     payload,
			IP:          c.ClientIP(),
			UA:          c.Request.UserAgent(),
		})
	}
}
