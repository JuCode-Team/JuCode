package biz

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
)

// RequestUsage is what one gateway request cost, for the app to show the
// exact figure under a reply. input_tokens counts every input token,
// cached_tokens the cache hits among them. Reasoning tokens are billed as
// output and not recorded apart, so reasoning_tokens is always 0.
type RequestUsage struct {
	RequestID       string          `json:"request_id"`
	Model           string          `json:"model"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	CachedTokens    int64           `json:"cached_tokens"`
	ReasoningTokens int64           `json:"reasoning_tokens"`
	Cost            decimal.Decimal `json:"cost"`
	Currency        string          `json:"currency"`
}

// UsageByRequest sums the user's usage_logs rows of one request id (the
// X-Request-Id the gateway answered with). Rows are written asynchronously,
// so a request that just finished may not have one yet: nil, nil.
func UsageByRequest(ctx context.Context, d *data.Data, userID uuid.UUID, requestID string) (*RequestUsage, error) {
	out := RequestUsage{RequestID: requestID}
	var n int
	var model, currency *string
	// request_id <> '' lets the planner use the partial request_id index.
	err := d.DB().QueryRowContext(ctx, `
SELECT COUNT(*),
	(array_agg(model ORDER BY created_at DESC))[1],
	COALESCE(SUM(tokens_in + cached_tokens_in + cache_creation_tokens_in + audio_tokens_in + image_tokens_in), 0),
	COALESCE(SUM(tokens_out + audio_tokens_out + image_tokens_out), 0),
	COALESCE(SUM(cached_tokens_in), 0),
	COALESCE(SUM(cost_final), 0),
	(array_agg(currency ORDER BY created_at DESC))[1]
FROM usage_logs WHERE request_id = $1 AND request_id <> '' AND user_id = $2`, requestID, userID).
		Scan(&n, &model, &out.InputTokens, &out.OutputTokens, &out.CachedTokens, &out.Cost, &currency)
	if err != nil || n == 0 {
		return nil, err
	}
	out.Model, out.Currency = *model, *currency
	return &out, nil
}
