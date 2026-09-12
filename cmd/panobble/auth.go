package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/listenbrainz"
	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/store"
)

func newLastfmClient(cfg config.Config) (*lastfm.Client, error) {
	if cfg.Lastfm.APIKey == "" || cfg.Lastfm.APISecret == "" {
		path, _ := config.Path()
		return nil, fmt.Errorf(
			"lastfm api_key/api_secret not set in %s\nCreate an API account at https://www.last.fm/api/account/create", path)
	}
	c := lastfm.NewClient(cfg.Lastfm.APIKey, cfg.Lastfm.APISecret)
	c.UserAgent = "panobble " + version
	return c, nil
}

// unauthedClient loads the config and builds a client without a session.
func unauthedClient() (*lastfm.Client, config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cfg, err
	}
	client, err := newLastfmClient(cfg)
	return client, cfg, err
}

// authedLastfm builds the Last.fm client with the stored session.
func authedLastfm(cfg config.Config) (*lastfm.Client, error) {
	client, err := newLastfmClient(cfg)
	if err != nil {
		return nil, err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, err
	}
	sess, err := store.LoadSession(dataDir)
	if err != nil {
		return nil, errors.New("not logged in — run: panobble auth")
	}
	client.SessionKey = sess.SessionKey
	client.Username = sess.Username
	return client, nil
}

// backends assembles the configured scrobble targets: Last.fm when api_key
// is set (needs a stored session), ListenBrainz when a token is set. The
// Last.fm client is also returned on its own (nil if not configured) for
// features beyond scrobbling.
func backends(cfg config.Config) ([]scrobble.Scrobbler, *lastfm.Client, error) {
	var out []scrobble.Scrobbler
	var lfm *lastfm.Client
	if cfg.Lastfm.APIKey != "" || cfg.Lastfm.APISecret != "" {
		client, err := authedLastfm(cfg)
		if err != nil {
			return nil, nil, err
		}
		lfm = client
		out = append(out, client)
	}
	if cfg.Listenbrainz.Token != "" {
		lb := listenbrainz.NewClient(cfg.Listenbrainz.Token, cfg.Listenbrainz.APIURL)
		lb.UserAgent = "panobble " + version
		lb.ClientVersion = version
		out = append(out, lb)
	}
	if len(out) == 0 {
		path, _ := config.Path()
		return nil, nil, fmt.Errorf(
			"no scrobble targets configured — set [lastfm] api_key/api_secret and/or [listenbrainz] token in %s", path)
	}
	return out, lfm, nil
}

// openBrowser opens url in the user's browser without waiting.
func openBrowser(url string) error {
	return exec.Command("xdg-open", url).Start()
}

func cmdAuth(args []string) error {
	client, _, err := unauthedClient()
	if err != nil {
		return err
	}

	ctx := context.Background()
	token, err := client.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("getting token: %w", err)
	}

	authURL := client.AuthURL(token)
	fmt.Println("Opening browser to authorize panobble with your Last.fm account…")
	fmt.Println("If it does not open, visit:")
	fmt.Println("  " + authURL)
	openBrowser(authURL)

	fmt.Println("Waiting for authorization (up to 2 minutes)…")
	sess, err := client.PollSession(ctx, token)
	if err != nil {
		return err
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return err
	}
	if err := store.SaveSession(dataDir, sess); err != nil {
		return err
	}
	fmt.Printf("Logged in as %s. Session saved.\n", sess.Username)
	return nil
}
