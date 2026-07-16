package main

import (
	"flag"
	"fmt"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/scrobble"
)

func cmdParse(args []string) error {
	fs := flag.NewFlagSet("parse", flag.ExitOnError)
	app := fs.String("app", "", "normalized app id (for per-app rules)")
	host := fs.String("host", "", "URL host, e.g. youtube.com or music.youtube.com")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: panobble parse [--app ID] [--host HOST] ARTIST TITLE [ALBUM]

Runs the metadata cleanup pipeline on one track and prints the result.`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 || len(rest) > 3 {
		fs.Usage()
		return fmt.Errorf("expected ARTIST TITLE [ALBUM]")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pipeline, err := clean.NewPipeline(cfg.Cleanup, cfg.Rules)
	if err != nil {
		return err
	}

	in := scrobble.Track{Artist: rest[0], Title: rest[1], AppID: *app}
	if len(rest) == 3 {
		in.Album = rest[2]
	}

	out, res := pipeline.Clean(in, *host)

	fmt.Printf("in:   artist=%q title=%q album=%q albumArtist=%q\n",
		in.Artist, in.Title, in.Album, in.AlbumArtist)
	switch {
	case res.Blocked:
		fmt.Printf("out:  BLOCKED by rule %q\n", res.BlockReason)
	case res.ParseFailed:
		fmt.Println("out:  SKIPPED (title parsing failed on strict youtube.com)")
	default:
		fmt.Printf("out:  artist=%q title=%q album=%q albumArtist=%q\n",
			out.Artist, out.Title, out.Album, out.AlbumArtist)
	}
	return nil
}
