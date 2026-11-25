package llm

// reconciler.go drives async generation tasks (video) to completion even when
// the client stops polling. It runs on the background leader only (started
// from main.go inside the leader callback), periodically polling in-flight
// tasks that haven't been touched recently and settling billing on completion
// via the same applyVideoPoll path the client GET uses.

import (
	"context"
	"log/slog"
	"time"
)

const (
	// Only reconcile tasks idle for at least this long, so we don't race the
	// client's own polls on active jobs.
	reconcileStaleAfter = 30 * time.Second
	// Max tasks polled per tick (bounds upstream fan-out).
	reconcileBatch = 50
	// A non-terminal task older than this is force-failed (expired) so it
	// stops being polled forever if the upstream lost it.
	reconcileMaxAge = 2 * time.Hour
)

// RunTaskReconciler loops until ctx is cancelled, reconciling in-flight tasks
// every interval. No-op when the task store isn't wired.
func (g *Gateway) RunTaskReconciler(ctx context.Context, interval time.Duration) {
	if g.tasks == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.Info("generation task reconciler started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.reconcileOnce(ctx)
		}
	}
}

func (g *Gateway) reconcileOnce(ctx context.Context) {
	tasks, err := g.tasks.Inflight(ctx, time.Now().Add(-reconcileStaleAfter), reconcileBatch)
	if err != nil {
		slog.Debug("reconciler.inflight_failed", "err", err.Error())
		return
	}
	for _, task := range tasks {
		// Expire tasks that have run far too long so they stop being polled.
		// An expired task is never billed, so its submit hold goes back too.
		if time.Since(task.CreatedAt) > reconcileMaxAge {
			g.releaseVideoHold(ctx, task)
			_ = g.tasks.SetStatus(ctx, task.ID, VideoStatusFailed)
			continue
		}
		ad := videoAdaptorFor(task.ProviderFormat)
		if ad == nil || task.ProviderID == nil || task.UpstreamTaskID == "" {
			continue
		}
		acct, err := g.accounts.Get(ctx, *task.ProviderID)
		if err != nil {
			continue
		}
		key, err := g.accounts.RequestToken(ctx, acct)
		if err != nil {
			continue
		}
		poll, err := ad.Poll(ctx, acct.BaseURL, key, task.UpstreamTaskID)
		if err != nil {
			continue
		}
		// applyVideoPoll completes the task and settles billing once on
		// success; the same idempotent MarkBilled guard prevents a race with
		// a concurrent client GET.
		g.applyVideoPoll(ctx, task, poll)
	}
}
