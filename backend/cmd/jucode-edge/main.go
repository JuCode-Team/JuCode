// Command jucode-edge is the EXPERIMENTAL regional edge node (docs/edge.md).
// It serves the OpenAI/Anthropic-compatible API in its own region: it keeps
// upstream credentials and account policy in memory/Redis, forwards requests
// to the upstream accounts it is allowed to use, spends credit leased from
// the control plane (the main server), and flushes usage events back to it.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // timezone must load without the host's zoneinfo

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/edge"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "configs/edge.yaml", "path to edge config")
	flag.Parse()
	cfg, err := edge.LoadConfig(configPath)
	if err != nil {
		slog.Error("edge config failed", "err", err)
		os.Exit(2)
	}
	slog.Warn("jucode-edge is EXPERIMENTAL: its protocol, config keys and storage may change without notice; see docs/edge.md")
	if err := biz.SetTimezone(cfg.Timezone); err != nil {
		slog.Error("edge timezone failed", "err", err)
		os.Exit(2)
	}
	svc, err := edge.NewService(cfg, edge.NewControlPlane(cfg))
	if err != nil {
		slog.Error("edge init failed", "err", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: svc.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		slog.Info("jucode-edge started", "addr", cfg.Listen, "region", cfg.RegionID, "control", cfg.ControlURL != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("edge server stopped", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdown)
}
