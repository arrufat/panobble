package lastfm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Session is the stored auth state.
type Session struct {
	Username   string `json:"username"`
	SessionKey string `json:"session_key"`
}

// GetToken starts the OOB (desktop) auth flow.
func (c *Client) GetToken(ctx context.Context) (string, error) {
	var resp struct {
		Token string `json:"token"`
	}
	err := c.post(ctx, map[string]string{
		"method":  "auth.getToken",
		"api_key": c.APIKey,
	}, &resp)
	if err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", errors.New("lastfm: empty token")
	}
	return resp.Token, nil
}

// AuthURL is the page the user must open to authorize the token.
func (c *Client) AuthURL(token string) string {
	return "https://www.last.fm/api/auth?api_key=" + c.APIKey + "&token=" + token
}

// PollSession polls auth.getSession until the user authorizes the token in
// the browser: every 5s for up to 2 minutes, retrying while the API returns
// code 14 ("token not authorized").
func (c *Client) PollSession(ctx context.Context, token string) (Session, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		sess, err := c.getSession(ctx, token)
		if err == nil {
			return sess, nil
		}

		var apiErr *scrobble.APIError
		notYet := errors.As(err, &apiErr) && apiErr.Code == 14
		if !notYet && !scrobble.Retryable(err) {
			return Session{}, err
		}
		if time.Now().After(deadline) {
			return Session{}, fmt.Errorf("timed out waiting for authorization: %w", err)
		}

		select {
		case <-ctx.Done():
			return Session{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Client) getSession(ctx context.Context, token string) (Session, error) {
	var resp struct {
		Session struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"session"`
	}
	err := c.post(ctx, map[string]string{
		"method":  "auth.getSession",
		"api_key": c.APIKey,
		"token":   token,
	}, &resp)
	if err != nil {
		return Session{}, err
	}
	if resp.Session.Key == "" {
		return Session{}, errors.New("lastfm: empty session key")
	}
	return Session{Username: resp.Session.Name, SessionKey: resp.Session.Key}, nil
}
