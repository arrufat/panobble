package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/tracker"
)

// tokenValidator is a backend holding a bare credential that no auth flow
// confirmed, so it is worth checking at startup.
type tokenValidator interface {
	ValidateToken(ctx context.Context) (string, error)
}

func cmdDaemon(args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(),
	}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	targets, lfm, err := backends(cfg)
	if err != nil {
		return err
	}
	if len(cfg.Players.Allowed) == 0 {
		path, _ := config.Path()
		return fmt.Errorf("no players allowed — run `panobble players` and add ids to [players].allowed in %s", path)
	}

	pipeline, err := clean.NewPipeline(cfg.Cleanup, cfg.Rules)
	if err != nil {
		return err
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}

	sub, err := newSubmitter(targets, dataDir, log)
	if err != nil {
		return err
	}
	defer sub.close()

	watcher, err := mpris.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	tr := tracker.New(cfg, pipeline, sub, log)

	if cfg.Notifications.Enabled {
		notif, err := newNotifier(cfg.Notifications, lfm, dataDir, log)
		if err != nil {
			log.Warn("desktop notifications unavailable", "err", err)
		} else {
			// Runs after the tracker drains, before the queue and watcher close.
			defer notif.close()
			tr.SetListener(notif)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := watcher.Run(); err != nil {
			log.Error("mpris watcher stopped", "err", err)
			stop()
		}
	}()
	go sub.flushLoop(ctx)

	// Best-effort token check so a bad token surfaces at startup, not at the
	// first scrobble.
	for _, b := range targets {
		if v, ok := b.(tokenValidator); ok {
			go func() {
				vctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				if user, err := v.ValidateToken(vctx); err != nil {
					log.Warn("token validation failed", "service", b.Name(), "err", err)
				} else {
					log.Info("token valid", "service", b.Name(), "user", user)
				}
			}()
		}
	}

	log.Info("panobble daemon started", "allowed", cfg.Players.Allowed,
		"services", sub.serviceNames(), "notifications", cfg.Notifications.Enabled)
	tr.Run(ctx, watcher.Events())
	log.Info("panobble daemon stopped")
	return nil
}

func logLevel() slog.Level {
	if os.Getenv("PANOBBLE_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
