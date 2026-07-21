// Package lastfm is a minimal Last.fm API client covering auth, now-playing
// and scrobbling.
//
// Protocol details (api_sig, batch format, OOB auth flow, error codes) are
// ported from pano-scrobbler's Lastfm.kt and LastFmUnauthedRequester.kt.
package lastfm

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

const defaultAPIRoot = "https://ws.audioscrobbler.com/2.0/"

type Client struct {
	APIKey     string
	APISecret  string
	SessionKey string // empty until authenticated
	HTTP       *http.Client
	UserAgent  string
	BaseURL    string
}

func NewClient(apiKey, apiSecret string) *Client {
	baseURL := os.Getenv("PANOBBLE_LASTFM_ROOT") // test/fault-injection override
	if baseURL == "" {
		baseURL = defaultAPIRoot
	}
	return &Client{
		APIKey:    apiKey,
		APISecret: apiSecret,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "panobble",
		BaseURL:   baseURL,
	}
}

// sign computes api_sig: drop empty values and the format param, sort keys
// case-sensitively, concatenate key+value, append the secret, md5 hex.
func (c *Client) sign(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" || strings.EqualFold(k, "format") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params[k])
	}
	b.WriteString(c.APISecret)
	return fmt.Sprintf("%032x", md5.Sum([]byte(b.String())))
}

// post sends a signed POST form request and decodes the JSON response into out.
func (c *Client) post(ctx context.Context, params map[string]string, out any) error {
	form := url.Values{}
	for k, v := range params {
		if v != "" {
			form.Set(k, v)
		}
	}
	form.Set("api_sig", c.sign(params))
	form.Set("format", "json")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

	// last.fm errors: {"error": 9, "message": "..."} with a non-2xx status
	// (sometimes 2xx). Sniff the error field first.
	var apiErr struct {
		Error   int    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != 0 {
		return &scrobble.APIError{
			Code:       apiErr.Error,
			HTTPStatus: resp.StatusCode,
			Message:    fmt.Sprintf("last.fm error %d: %s", apiErr.Error, apiErr.Message),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &scrobble.APIError{
			HTTPStatus: resp.StatusCode,
			Message:    fmt.Sprintf("last.fm HTTP %d: %.200s", resp.StatusCode, body),
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
