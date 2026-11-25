package biz

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// routeTPSWindow is the generation window TPS divides by. It matches the
// admin observability definition: from the earlier of the first visible
// token and the first model-output frame, through the complete response.
const routeTPSWindow = "(latency_ms - LEAST(ttft_ms, COALESCE((latency_detail->>'first_output_ms')::bigint, (latency_detail->>'first_sse_frame_ms')::bigint, ttft_ms)))"

// routeTPSFilter is the row set that counts as a TPS sample. Same bounds as
// the admin charts: a successful streamed answer, a positive window that is
// at least 5% of the request, and at least 2ms of window per output token.
const routeTPSFilter = "status = 'ok' AND tokens_out > 0 AND ttft_ms IS NOT NULL AND " + routeTPSWindow + " > 0" +
	" AND " + routeTPSWindow + " >= latency_ms * 0.05 AND " + routeTPSWindow + " >= tokens_out * 2"

const (
	routeSignalWindow         = 6 * time.Hour
	routeSignalTTL            = 60 * time.Second
	routeSignalMinSamples     = 5
	routeSignalMinInputTokens = 1000
)

type routeSignalCacheEntry struct {
	at      time.Time
	signals map[uuid.UUID]GroupRouteSignal
}

// routeSignals returns recent per-group stats for one model. A lookup
// failure leaves the map empty so routing falls back to the multiplier.
func (s *ModelSvc) routeSignals(ctx context.Context, model string, groupIDs []uuid.UUID) map[uuid.UUID]GroupRouteSignal {
	if s == nil || s.d == nil || s.d.DB() == nil || model == "" || len(groupIDs) == 0 {
		return nil
	}
	key := routeSignalCacheKey(model, groupIDs)
	s.signalMu.Lock()
	if s.signalCache != nil {
		if entry, ok := s.signalCache[key]; ok && time.Since(entry.at) < routeSignalTTL {
			s.signalMu.Unlock()
			return entry.signals
		}
	}
	s.signalMu.Unlock()

	signals, err := s.queryRouteSignals(ctx, model, groupIDs)
	if err != nil {
		return nil
	}
	s.signalMu.Lock()
	if s.signalCache == nil {
		s.signalCache = map[string]routeSignalCacheEntry{}
	}
	s.signalCache[key] = routeSignalCacheEntry{at: time.Now(), signals: signals}
	s.signalMu.Unlock()
	return signals
}

func routeSignalCacheKey(model string, groupIDs []uuid.UUID) string {
	parts := make([]string, len(groupIDs))
	for i, id := range groupIDs {
		parts[i] = id.String()
	}
	return model + "|" + strings.Join(parts, ",")
}

func (s *ModelSvc) queryRouteSignals(ctx context.Context, model string, groupIDs []uuid.UUID) (map[uuid.UUID]GroupRouteSignal, error) {
	args := make([]any, 0, len(groupIDs)+2)
	args = append(args, model, time.Now().Add(-routeSignalWindow))
	holders := make([]string, len(groupIDs))
	for i, id := range groupIDs {
		args = append(args, id)
		holders[i] = fmt.Sprintf("$%d", len(args))
	}
	query := fmt.Sprintf(`
SELECT group_id::text,
       SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END),
       SUM(CASE WHEN status IN ('ok', 'upstream_error') THEN 1 ELSE 0 END),
       AVG(ttft_ms) FILTER (WHERE status = 'ok' AND ttft_ms IS NOT NULL)::float8,
       COUNT(ttft_ms) FILTER (WHERE status = 'ok' AND ttft_ms IS NOT NULL),
       SUM(CASE WHEN %s THEN tokens_out::float8 END),
       SUM(CASE WHEN %s THEN %s::float8 / 1000.0 END),
       COUNT(*) FILTER (WHERE %s),
       SUM(cached_tokens_in),
       SUM(tokens_in + cached_tokens_in),
       SUM(tokens_out)
FROM usage_logs
WHERE model = $1 AND created_at > $2 AND group_id IN (%s)
  AND (api_key_id IS NULL OR api_key_id NOT IN (SELECT id FROM api_keys WHERE internal))
GROUP BY group_id`, routeTPSFilter, routeTPSFilter, routeTPSWindow, routeTPSFilter, strings.Join(holders, ","))
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]GroupRouteSignal{}
	for rows.Next() {
		var (
			groupText                       string
			good, samples, ttftN, tpsN      int64
			avgTTFT, tpsTokens, tpsSeconds  sql.NullFloat64
			cached, inputTotal, outputTotal int64
		)
		if err := rows.Scan(&groupText, &good, &samples, &avgTTFT, &ttftN, &tpsTokens, &tpsSeconds, &tpsN, &cached, &inputTotal, &outputTotal); err != nil {
			return nil, err
		}
		id, err := uuid.Parse(groupText)
		if err != nil {
			continue
		}
		signal := GroupRouteSignal{}
		if samples >= routeSignalMinSamples {
			rate := float64(good) / float64(samples)
			signal.Success = &rate
		}
		if ttftN >= routeSignalMinSamples && avgTTFT.Valid {
			ms := avgTTFT.Float64
			signal.TTFTMs = &ms
		}
		if tpsN >= routeSignalMinSamples && tpsTokens.Valid && tpsSeconds.Valid && tpsSeconds.Float64 > 0 {
			rate := tpsTokens.Float64 / tpsSeconds.Float64
			signal.TPS = &rate
		}
		if inputTotal >= routeSignalMinInputTokens && inputTotal > 0 {
			hit := float64(cached) / float64(inputTotal)
			signal.Hit = &hit
			ratio := float64(outputTotal) / float64(inputTotal)
			signal.OutputRatio = &ratio
		}
		out[id] = signal
	}
	return out, rows.Err()
}
