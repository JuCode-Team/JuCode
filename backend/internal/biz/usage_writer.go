package biz

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/conf"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
)

// ErrUsageLogDropped is returned by WriteUsageLog when the batch writer's
// buffer is full. The row is gone; the caller cannot retry it. Most callers
// ignore the error (they already did before batching) — the authoritative
// signal is UsageLogWriter.Dropped().
var ErrUsageLogDropped = errors.New("usage log buffer full; row dropped")

// Hard ceiling on rows per INSERT. usage_logs has 45 columns and Postgres
// caps a statement at 65535 bound parameters, so 65535/45 = 1456 rows is the
// real limit. Anything the config asks for above this is clamped.
const usageLogMaxRowsPerInsert = 1400

const (
	defaultUsageLogBufferSize   = 8192
	defaultUsageLogFlushMaxRows = 500
	defaultUsageLogFlushMaxAge  = time.Second
	// A flush holds one DB connection for its duration; keep it well under
	// the 20s the synchronous path used so a stalled DB can't wedge the
	// writer indefinitely.
	usageLogFlushTimeout = 30 * time.Second
)

// usageLogEntry is one queued write. The observation is resolved and
// normalized at submit time (not at flush time) because it depends on the
// request context — see normalizeAvailabilityInput.
type usageLogEntry struct {
	log         WriteUsageLogInput
	observation *RecordAvailabilityInput
}

// UsageLogWriter batches usage_logs + availability_observations writes.
//
// Before this existed, every LLM request ended with its own transaction —
// two INSERTs and a commit on the request goroutine, against a 32-connection
// pool. Batching turns N transactions into one and N*2 INSERT statements into
// two multi-VALUES statements, and moves the write off the request path.
//
// The structure: buffered channel, single writer goroutine, one-shot age
// timer, drain-on-Stop. Usage rows are money, which shapes two choices:
//
//   - runWriter does NOT select on ctx.Done(). Root-ctx cancellation races
//     with http.Server.Shutdown draining in-flight requests; exiting there
//     would silently discard rows those requests are still submitting. Only
//     the explicit Stop() ends the loop.
//   - a failed flush is retried once before the batch is dropped.
type UsageLogWriter struct {
	d      *data.Data
	in     chan usageLogEntry
	stopCh chan struct{}
	doneCh chan struct{}
	logger *slog.Logger

	maxRows int
	maxAge  time.Duration

	dropped atomic.Int64
}

func NewUsageLogWriter(d *data.Data, cfg conf.UsageLogConfig, logger *slog.Logger) *UsageLogWriter {
	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = defaultUsageLogBufferSize
	}
	maxRows := cfg.FlushMaxRows
	if maxRows <= 0 {
		maxRows = defaultUsageLogFlushMaxRows
	}
	if maxRows > usageLogMaxRowsPerInsert {
		maxRows = usageLogMaxRowsPerInsert
	}
	maxAge := cfg.FlushMaxAge
	if maxAge <= 0 {
		maxAge = defaultUsageLogFlushMaxAge
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &UsageLogWriter{
		d:       d,
		in:      make(chan usageLogEntry, bufSize),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		logger:  logger.With("svc", "usage_log_writer"),
		maxRows: maxRows,
		maxAge:  maxAge,
	}
}

// Start launches the writer goroutine. Call once at boot.
//
// ctx is used only for the periodic drop report, never to end the write loop
// (see the type comment).
func (w *UsageLogWriter) Start(ctx context.Context) {
	if w == nil {
		return
	}
	go w.runWriter()
	go w.reportDrops(ctx)
}

// Stop flushes whatever is buffered and waits for the writer to finish.
// Safe to call more than once.
func (w *UsageLogWriter) Stop() {
	if w == nil {
		return
	}
	select {
	case <-w.stopCh:
		return
	default:
		close(w.stopCh)
	}
	select {
	case <-w.doneCh:
	case <-time.After(15 * time.Second):
		w.logger.Error("usage log writer stop timed out; rows may be lost",
			"queued", len(w.in), "dropped_total", w.dropped.Load())
	}
}

// Dropped reports how many rows were lost to a full buffer or a failed flush
// since boot. Non-zero means usage data (and therefore billing audit trail)
// is incomplete.
func (w *UsageLogWriter) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Submit queues one row. Non-blocking: a full buffer drops the row rather
// than stalling the request that produced it.
func (w *UsageLogWriter) Submit(entry usageLogEntry) bool {
	if w == nil {
		return false
	}
	select {
	case w.in <- entry:
		return true
	default:
		w.dropped.Add(1)
		w.logger.Error("usage log buffer full; dropping row",
			"request_id", entry.log.RequestID, "user_id", entry.log.UserID)
		return false
	}
}

func (w *UsageLogWriter) runWriter() {
	defer close(w.doneCh)

	buf := make([]usageLogEntry, 0, w.maxRows)

	// One-shot timer armed when the buffer goes from empty to non-empty,
	// rather than a sub-second ticker that would spin while idle.
	var ageTimer *time.Timer
	armTimer := func() {
		if ageTimer == nil {
			ageTimer = time.NewTimer(w.maxAge)
		}
	}
	stopTimer := func() {
		if ageTimer != nil {
			if !ageTimer.Stop() {
				select {
				case <-ageTimer.C:
				default:
				}
			}
			ageTimer = nil
		}
	}

	flush := func(reason string) {
		if len(buf) == 0 {
			return
		}
		w.flushBatch(buf, reason)
		buf = buf[:0]
		stopTimer()
	}

	for {
		var timerCh <-chan time.Time
		if ageTimer != nil {
			timerCh = ageTimer.C
		}
		select {
		case entry, ok := <-w.in:
			if !ok {
				flush("channel-closed")
				return
			}
			buf = append(buf, entry)
			if len(buf) == 1 {
				armTimer()
			}
			if len(buf) >= w.maxRows {
				flush("full")
			}
		case <-timerCh:
			ageTimer = nil
			flush("age")
		case <-w.stopCh:
			flush("stop")
			// Drain anything that landed concurrently with the stop signal.
			for {
				select {
				case entry := <-w.in:
					buf = append(buf, entry)
					if len(buf) >= w.maxRows {
						flush("drain-full")
					}
				default:
					flush("drain")
					return
				}
			}
		}
	}
}

// flushBatch writes one batch, retrying once. Both attempts failing means the
// rows are lost — that is the deliberate trade (never block the gateway on the
// audit trail), and it is counted and logged at Error.
func (w *UsageLogWriter) flushBatch(buf []usageLogEntry, reason string) {
	err := w.writeBatch(buf)
	if err != nil {
		w.logger.Warn("usage log batch write failed; retrying once",
			"reason", reason, "count", len(buf), "err", err.Error())
		err = w.writeBatch(buf)
	}
	if err != nil {
		w.dropped.Add(int64(len(buf)))
		w.logger.Error("usage log batch write failed twice; dropping rows",
			"reason", reason, "count", len(buf), "err", err.Error(),
			"dropped_total", w.dropped.Load())
		return
	}
	w.logger.Debug("usage log batch written", "reason", reason, "count", len(buf))
}

func (w *UsageLogWriter) writeBatch(buf []usageLogEntry) error {
	// Detached from any request context on purpose: this batch outlives the
	// requests that produced it.
	ctx, cancel := context.WithTimeout(context.Background(), usageLogFlushTimeout)
	defer cancel()

	tx, err := w.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	client := tx.Client()

	logBuilders := make([]*ent.UsageLogCreate, 0, len(buf))
	for i := range buf {
		logBuilders = append(logBuilders, usageLogCreateBuilder(client, buf[i].log))
	}
	if err := client.UsageLog.CreateBulk(logBuilders...).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}

	if obsBuilders := availabilityBuilders(client, buf); len(obsBuilders) > 0 {
		if err := client.AvailabilityObservation.CreateBulk(obsBuilders...).
			OnConflict().
			DoNothing().
			Exec(ctx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// availabilityBuilders collects the observation rows for a batch, dropping
// duplicates on (request_id, group_id).
//
// availability_observations has a partial unique index on that pair precisely
// so retries cannot inflate public availability numbers. ON CONFLICT DO
// NOTHING covers collisions with rows already in the table, but two entries
// colliding *within the same statement* is murkier ground, and it is reachable
// here: a video task can be settled twice (client poll and the leader-side
// reconciler), and both settlements can land in one batch. Deduping in Go
// keeps the statement unambiguous.
func availabilityBuilders(client *ent.Client, buf []usageLogEntry) []*ent.AvailabilityObservationCreate {
	type dedupKey struct {
		requestID string
		groupID   uuid.UUID
	}
	var (
		builders []*ent.AvailabilityObservationCreate
		seen     map[dedupKey]struct{}
	)
	for i := range buf {
		obs := buf[i].observation
		if obs == nil {
			continue
		}
		// An empty request_id is outside the unique index, so those rows are
		// intentionally never deduped.
		if obs.RequestID != "" {
			k := dedupKey{requestID: obs.RequestID, groupID: obs.GroupID}
			if seen == nil {
				seen = make(map[dedupKey]struct{}, len(buf))
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		builders = append(builders, availabilityCreateBuilder(client, *obs))
	}
	return builders
}

// reportDrops surfaces the drop counter periodically. A silent drop counter is
// worse than none: the whole point of the buffer is that losing rows is
// preferable to stalling requests, which only holds if the loss is visible.
func (w *UsageLogWriter) reportDrops(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			total := w.dropped.Load()
			if total == last {
				continue
			}
			w.logger.Error("usage log rows dropped in the last minute",
				"delta", total-last, "total", total, "queued", len(w.in))
			last = total
		}
	}
}
