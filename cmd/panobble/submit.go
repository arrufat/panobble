package main

import (
	"context"
	"flag"
	"fmt"
	"time"

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

	client, _, err := authedClient()
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
	if *np {
		if err := client.NowPlaying(ctx, t); err != nil {
			return err
		}
		fmt.Println("now playing sent")
		return nil
	}
	if err := client.Scrobble(ctx, []scrobble.Track{t}); err != nil {
		return err
	}
	fmt.Println("scrobbled")
	return nil
}
