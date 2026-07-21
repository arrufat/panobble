package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/lastfm"
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

// authedClient loads the config and stored session.
func authedClient() (*lastfm.Client, config.Config, error) {
	client, cfg, err := unauthedClient()
	if err != nil {
		return nil, cfg, err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, cfg, err
	}
	sess, err := store.LoadSession(dataDir)
	if err != nil {
		return nil, cfg, errors.New("not logged in — run: panobble auth")
	}
	client.SessionKey = sess.SessionKey
	return client, cfg, nil
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
	exec.Command("xdg-open", authURL).Start()

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
