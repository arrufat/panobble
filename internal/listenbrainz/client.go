// Package listenbrainz is a minimal ListenBrainz API client covering token
// validation, now-playing and listen submission.
//
// Protocol details (submit-listens payload, listen types, token auth) are
// ported from pano-scrobbler's ListenBrainz.kt and ListenBrainzDataClasses.kt.
package listenbrainz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

const DefaultAPIRoot = "https://api.listenbrainz.org/"

type Client struct {
	Token         string
	BaseURL       string // trailing slash; DefaultAPIRoot if empty in config
	HTTP          *http.Client
	UserAgent     string
	ClientVersion string // reported as submission_client_version
}

func NewClient(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultAPIRoot
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	return &Client{
		Token:     token,
		BaseURL:   baseURL,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "panobble",
	}
}

// listen is the submit-listens request body.
type listen struct {
	ListenType string    `json:"listen_type"` // playing_now, single, import
	Payload    []payload `json:"payload"`
}

type payload struct {
	ListenedAt int64         `json:"listened_at,omitempty"` // unix seconds; absent for playing_now
	Metadata   trackMetadata `json:"track_metadata"`
}

type trackMetadata struct {
	ArtistName  string          `json:"artist_name"`
	TrackName   string          `json:"track_name"`
	ReleaseName string          `json:"release_name,omitempty"`
	Additional  *additionalInfo `json:"additional_info,omitempty"`
}

type additionalInfo struct {
	DurationMS              int64  `json:"duration_ms,omitempty"`
	SubmissionClient        string `json:"submission_client,omitempty"`
	SubmissionClientVersion string `json:"submission_client_version,omitempty"`
}

func (c *Client) toPayload(t scrobble.Track, withTimestamp bool) payload {
	p := payload{
		Metadata: trackMetadata{
			ArtistName:  t.Artist,
			TrackName:   t.Title,
			ReleaseName: t.Album,
			Additional: &additionalInfo{
				SubmissionClient:        "panobble",
				SubmissionClientVersion: c.ClientVersion,
			},
		},
	}
	if withTimestamp {
		p.ListenedAt = t.Timestamp.Unix()
	}
	if d, ok := t.SafeDuration(); ok {
		p.Metadata.Additional.DurationMS = d.Milliseconds()
	}
	return p
}

// ValidateToken checks the configured token and returns the account's
// username.
func (c *Client) ValidateToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"1/validate-token", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Valid    bool   `json:"valid"`
		UserName string `json:"user_name"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if !out.Valid {
		return "", &scrobble.APIError{
			Class:   scrobble.ClassNeedsReauth,
			Message: "listenbrainz: invalid token",
		}
	}
	return out.UserName, nil
}

func (c *Client) submitListens(ctx context.Context, listenType string, ps []payload) error {
	body, err := json.Marshal(listen{ListenType: listenType, Payload: ps})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"1/submit-listens", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

// do sends the request with auth headers and decodes the JSON response into
// out. Non-2xx responses become classified APIErrors.
func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Token "+c.Token)
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Code  int    `json:"code"`
			Error string `json:"error"`
		}
		msg := fmt.Sprintf("listenbrainz HTTP %d: %.200s", resp.StatusCode, body)
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			msg = fmt.Sprintf("listenbrainz HTTP %d: %s", resp.StatusCode, apiErr.Error)
		}
		return &scrobble.APIError{
			Class:      classify(resp.StatusCode),
			Code:       apiErr.Code,
			HTTPStatus: resp.StatusCode,
			Message:    msg,
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// classify maps a ListenBrainz HTTP status onto the shared taxonomy.
func classify(status int) scrobble.Class {
	switch {
	case status == 401: // invalid or revoked token
		return scrobble.ClassNeedsReauth
	case status == 400: // malformed listen: resubmitting cannot help
		return scrobble.ClassTerminal
	case status == 429 || status >= 500:
		return scrobble.ClassRetryable
	}
	return scrobble.ClassUnknown
}
