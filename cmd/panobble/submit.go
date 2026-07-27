package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/scrobble"
)

// cmdSubmit is a hidden test command: submit one scrobble right now.
func cmdSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	artist := fs.String("artist", "", "artist")
	track := fs.String("track", "", "track title")
	album := fs.String("album", "", "album")
	np := fs.Bool("np", false, "send now-playing instead of a scrobble")
	fs.Parse(args)

	if *artist == "" || *track == "" {
		return fmt.Errorf("--artist and --track are required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	targets, err := backends(cfg)
	if err != nil {
		return err
	}

	t := scrobble.Track{
		Artist:    *artist,
		Title:     *track,
		Album:     *album,
		Timestamp: time.Now(),
	}

	ctx := context.Background()
	for _, c := range targets {
		if *np {
			if err := c.NowPlaying(ctx, t); err != nil {
				return fmt.Errorf("%s: %w", c.Name(), err)
			}
			fmt.Println(c.Name() + ": now playing sent")
		} else {
			if err := c.Scrobble(ctx, []scrobble.Track{t}); err != nil {
				return fmt.Errorf("%s: %w", c.Name(), err)
			}
			fmt.Println(c.Name() + ": scrobbled")
		}
	}
	return nil
}
