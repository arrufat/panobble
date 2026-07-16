package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/tracker"
)

func cmdDaemon(args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel(),
	}))

	client, cfg, err := authedClient()
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

	sub, err := newSubmitter(client, dataDir, log)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := watcher.Run(); err != nil {
			log.Error("mpris watcher stopped", "err", err)
			stop()
		}
	}()
	go sub.flushLoop(ctx)

	log.Info("panobble daemon started", "allowed", cfg.Players.Allowed)
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
