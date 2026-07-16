package lastfm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Compile-time check: *Client implements scrobble.Scrobbler.
var _ scrobble.Scrobbler = (*Client)(nil)

func (c *Client) Name() string { return "lastfm" }

// NowPlaying submits track.updateNowPlaying. An "ignored" response counts as
// success.
func (c *Client) NowPlaying(ctx context.Context, t scrobble.Track) error {
	params := map[string]string{
		"method":  "track.updateNowPlaying",
		"api_key": c.APIKey,
		"sk":      c.SessionKey,
	}
	trackParams(params, t, -1)

	var resp struct {
		NowPlaying struct {
			IgnoredMessage struct {
				Code string `json:"code"`
			} `json:"ignoredMessage"`
		} `json:"nowplaying"`
	}
	return c.post(ctx, params, &resp)
}

// Scrobble submits up to 50 tracks with track.scrobble (batch format:
// zero-indexed artist[0], track[0], ...).
func (c *Client) Scrobble(ctx context.Context, ts []scrobble.Track) error {
	if len(ts) == 0 {
		return nil
	}
	if len(ts) > 50 {
		return fmt.Errorf("lastfm: batch too large: %d > 50", len(ts))
	}

	params := map[string]string{
		"method":  "track.scrobble",
		"api_key": c.APIKey,
		"sk":      c.SessionKey,
	}
	if len(ts) == 1 {
		trackParams(params, ts[0], -1)
		params["timestamp"] = strconv.FormatInt(ts[0].Timestamp.Unix(), 10)
		params["chosenByUser"] = "1"
	} else {
		for i, t := range ts {
			trackParams(params, t, i)
			params[idx("timestamp", i)] = strconv.FormatInt(t.Timestamp.Unix(), 10)
			params[idx("chosenByUser", i)] = "1"
		}
	}

	var resp struct {
		Scrobbles struct {
			Attr struct {
				Accepted int `json:"accepted"`
				Ignored  int `json:"ignored"`
			} `json:"@attr"`
		} `json:"scrobbles"`
	}
	return c.post(ctx, params, &resp)
	// resp.Scrobbles.Attr.Ignored > 0 means some were ignored; treated as
	// success, not requeued.
}

// trackParams fills the track fields; i == -1 means unindexed (single form).
func trackParams(params map[string]string, t scrobble.Track, i int) {
	set := func(k, v string) {
		if v == "" {
			return
		}
		if i >= 0 {
			k = idx(k, i)
		}
		params[k] = v
	}
	set("artist", t.Artist)
	set("track", t.Title)
	set("album", t.Album)
	set("albumArtist", t.AlbumArtist)
	if t.TrackNumber > 0 {
		set("trackNumber", strconv.Itoa(t.TrackNumber))
	}
	if d, ok := t.SafeDuration(); ok {
		set("duration", strconv.Itoa(int(d.Seconds())))
	}
}

func idx(key string, i int) string {
	return key + "[" + strconv.Itoa(i) + "]"
}
