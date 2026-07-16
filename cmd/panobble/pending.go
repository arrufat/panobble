package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/store"
)

func cmdPending(args []string) error {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	flush := fs.Bool("flush", false, "submit queued scrobbles now")
	fs.Parse(args)

	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}

	if !*flush {
		entries, err := store.ListPending(dataDir)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("no pending scrobbles")
			return nil
		}
		for _, p := range entries {
			note := "failed: " + p.Reason
			if p.Deferred {
				note = "awaiting commit"
			}
			fmt.Printf("%s  %s — %s  (%s)\n",
				p.Track.Timestamp.Format("2006-01-02 15:04"),
				p.Track.Artist, p.Track.Title, note)
		}
		fmt.Printf("%d pending\n", len(entries))
		return nil
	}

	client, _, err := authedClient()
	if err != nil {
		return err
	}
	queue, err := store.OpenQueue(dataDir)
	if err != nil {
		return err
	}
	defer queue.Close()

	before, _ := queue.Len()
	if before == 0 {
		fmt.Println("no pending scrobbles")
		return nil
	}
	if err := queue.Flush(context.Background(), client); err != nil {
		return fmt.Errorf("flush incomplete: %w", err)
	}
	after, _ := queue.Len()
	fmt.Printf("flushed %d, %d remaining\n", before-after, after)
	return nil
}
