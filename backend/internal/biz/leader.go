package biz

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type LeaderRunner struct {
	rdb      *redis.Client
	key      string
	owner    string
	ttl      time.Duration
	logger   *slog.Logger
	acquired chan struct{}
}

func NewLeaderRunner(rdb *redis.Client, key string, ttl time.Duration, logger *slog.Logger) *LeaderRunner {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LeaderRunner{rdb: rdb, key: key, owner: uuid.NewString(), ttl: ttl, logger: logger.With("leader_key", key), acquired: make(chan struct{})}
}

func (r *LeaderRunner) Start(ctx context.Context, run func(context.Context)) {
	go r.loop(ctx, run)
}

func (r *LeaderRunner) loop(ctx context.Context, run func(context.Context)) {
	for ctx.Err() == nil {
		ok, err := r.rdb.SetNX(ctx, r.key, r.owner, r.ttl).Result()
		if err != nil {
			r.logger.Warn("leader acquire failed", "err", err.Error())
			sleepContext(ctx, 5*time.Second)
			continue
		}
		if !ok {
			sleepContext(ctx, 5*time.Second)
			continue
		}

		r.logger.Info("leader acquired")
		leaderCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			run(leaderCtx)
		}()

		if !r.renewUntilLost(leaderCtx, done) {
			cancel()
			<-done
		}
		cancel()
		_ = r.release(context.Background())
		r.logger.Warn("leader lost")
	}
}

func (r *LeaderRunner) renewUntilLost(ctx context.Context, done <-chan struct{}) bool {
	interval := r.ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return true
		case <-ticker.C:
			ok, err := renewLeader(ctx, r.rdb, r.key, r.owner, r.ttl)
			if err != nil {
				r.logger.Warn("leader renew failed", "err", err.Error())
				return false
			}
			if !ok {
				return false
			}
		}
	}
}

func (r *LeaderRunner) release(ctx context.Context) error {
	_, err := r.rdb.Eval(ctx, `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`, []string{r.key}, r.owner).Result()
	return err
}

func renewLeader(ctx context.Context, rdb *redis.Client, key, owner string, ttl time.Duration) (bool, error) {
	v, err := rdb.Eval(ctx, `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`, []string{key}, owner, int(ttl.Milliseconds())).Int()
	return v == 1, err
}

func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
