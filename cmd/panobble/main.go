// panobble is a minimal MPRIS → Last.fm scrobbler daemon for Linux,
// porting the scrobbling core of pano-scrobbler.
package main

import (
	"fmt"
	"os"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `panobble %s — MPRIS → Last.fm scrobbler

Usage:
  panobble daemon            run the scrobbler in the foreground
  panobble auth              log in to Last.fm (opens a browser)
  panobble players           list current MPRIS players and their ids
  panobble parse [flags] ARTIST TITLE [ALBUM]
                             run the metadata cleanup pipeline and print the result
  panobble pending [--flush] list (or flush) queued offline scrobbles
  panobble version

Config: ~/.config/panobble/config.toml
`, version)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	var err error
	switch os.Args[1] {
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "auth":
		err = cmdAuth(os.Args[2:])
	case "players":
		err = cmdPlayers(os.Args[2:])
	case "parse":
		err = cmdParse(os.Args[2:])
	case "pending":
		err = cmdPending(os.Args[2:])
	case "submit": // hidden: manual end-to-end test
		err = cmdSubmit(os.Args[2:])
	case "version":
		fmt.Println("panobble", version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "panobble:", err)
		os.Exit(1)
	}
}

func cmdPlayers(args []string) error {
	w, err := mpris.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	players, err := w.ListPlayers()
	if err != nil {
		return err
	}
	if len(players) == 0 {
		fmt.Println("no MPRIS players found")
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	fmt.Printf("%-40s %-20s %s\n", "ID (for config [players].allowed)", "IDENTITY", "STATUS")
	for _, p := range players {
		id := mpris.NormalizeAppID(p.BusName)
		allowed := ""
		if mpris.MatchesApp(id, cfg.Players.Allowed) {
			allowed = "allowed"
		}
		fmt.Printf("%-40s %-20s %s\n", id, p.Identity, allowed)
	}
	return nil
}
