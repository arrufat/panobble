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
	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/listenbrainz"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/tracker"
)

func cmdDaemon(args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(),
	}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	targets, err := backends(cfg)
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
		notif, err := newNotifier(cfg.Notifications, lastfmClient(targets), dataDir, log)
		if err != nil {
			log.Warn("desktop notifications unavailable", "err", err)
		} else {
			// Deferred before tr.Run so it runs right after the tracker
			// drains (which posts the final Ended) and before the queue and
			// watcher shut down.
			defer notif.close()
			tr.SetListener(notif)
			go notif.run()
			go notif.client.Run()
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
		if lb, ok := b.(*listenbrainz.Client); ok {
			go func() {
				vctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				if user, err := lb.ValidateToken(vctx); err != nil {
					log.Warn("listenbrainz token validation failed", "err", err)
				} else {
					log.Info("listenbrainz token valid", "user", user)
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

// lastfmClient returns the configured Last.fm backend, or nil.
func lastfmClient(targets []scrobble.Scrobbler) *lastfm.Client {
	for _, b := range targets {
		if c, ok := b.(*lastfm.Client); ok {
			return c
		}
	}
	return nil
}

func logLevel() slog.Level {
	if os.Getenv("PANOBBLE_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
