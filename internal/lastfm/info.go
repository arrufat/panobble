package lastfm

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/arrufat/panobble/internal/scrobble"
)

// TrackInfo is the subset of track.getInfo that panobble displays.
type TrackInfo struct {
	Title         string
	Artist        string
	Album         string
	URL           string // the track's last.fm page
	UserPlayCount int    // scrobbles by the requesting user
	UserLoved     bool   // loved by the requesting user
}

// GetTrackInfo fetches track.getInfo. username may be empty, in which case
// the user-scoped fields stay zero. Ported from
// LastFmUnauthedRequester.getTrackInfo.
func (c *Client) GetTrackInfo(ctx context.Context, artist, title, username string) (TrackInfo, error) {
	var resp struct {
		Track struct {
			Name   string `json:"name"`
			URL    string `json:"url"`
			Artist struct {
				Name string `json:"name"`
			} `json:"artist"`
			Album struct {
				Title string `json:"title"`
			} `json:"album"`
			UserPlayCount flexInt  `json:"userplaycount"`
			UserLoved     flexBool `json:"userloved"`
		} `json:"track"`
	}
	err := c.get(ctx, map[string]string{
		"method":   "track.getInfo",
		"api_key":  c.APIKey,
		"artist":   plusQuirk(artist),
		"track":    plusQuirk(title),
		"username": username,
	}, &resp)
	if err != nil {
		return TrackInfo{}, err
	}
	return TrackInfo{
		Title:         resp.Track.Name,
		Artist:        resp.Track.Artist.Name,
		Album:         resp.Track.Album.Title,
		URL:           resp.Track.URL,
		UserPlayCount: int(resp.Track.UserPlayCount),
		UserLoved:     bool(resp.Track.UserLoved),
	}, nil
}

// TrackURL builds the track's last.fm page URL, the fallback when
// track.getInfo did not answer.
func TrackURL(artist, title string) string {
	return "https://www.last.fm/music/" + url.PathEscape(artist) + "/_/" + url.PathEscape(title)
}

// TrackNotFound reports last.fm error 6 ("Track not found").
func TrackNotFound(err error) bool {
	var apiErr *scrobble.APIError
	return errors.As(err, &apiErr) && apiErr.Code == 6
}

// Love marks the track as loved by the authenticated user.
// Ported from Lastfm.loveOrUnlove.
func (c *Client) Love(ctx context.Context, artist, title string) error {
	return c.setLoved(ctx, "track.love", artist, title)
}

// Unlove removes the track from the authenticated user's loved tracks.
func (c *Client) Unlove(ctx context.Context, artist, title string) error {
	return c.setLoved(ctx, "track.unlove", artist, title)
}

func (c *Client) setLoved(ctx context.Context, method, artist, title string) error {
	return c.post(ctx, map[string]string{
		"method":  method,
		"api_key": c.APIKey,
		"sk":      c.SessionKey,
		"artist":  artist,
		"track":   title,
	}, nil)
}

// plusQuirk works around a last.fm bug on unauthenticated GETs: a literal
// '+' must be sent double-encoded (LastFmUnauthedRequester.doubleEncodePlusParam).
func plusQuirk(s string) string {
	return strings.ReplaceAll(s, "+", "%2B")
}

// flexInt decodes a JSON number or a numeric string (last.fm sends
// userplaycount as "42"); anything unparsable reads as 0. Mirrors upstream's
// StringOrIntSerializer.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	n, err := strconv.Atoi(s)
	if err != nil {
		n = 0
	}
	*f = flexInt(n)
	return nil
}

// flexBool decodes true/false, 1/0 and their string forms (last.fm sends
// userloved as "1"/"0"). Mirrors upstream's StringOrBoolSerializer.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	*f = flexBool(s == "1" || s == "true")
	return nil
}
