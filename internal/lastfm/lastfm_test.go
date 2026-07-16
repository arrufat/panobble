package lastfm

import (
	"context"
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Known-answer test for the api_sig algorithm: empty values and the format
// param are excluded, keys sorted case-sensitively, secret appended, md5 hex.
func TestSign(t *testing.T) {
	c := NewClient("KEY", "SECRET")
	params := map[string]string{
		"method":  "track.scrobble",
		"api_key": "KEY",
		"sk":      "SK",
		"artist":  "Artist",
		"track":   "Track",
		"album":   "",     // empty: excluded
		"format":  "json", // format: excluded
	}
	// sorted concat: api_keyKEY artistArtist methodtrack.scrobble skSK trackTrack + SECRET
	want := fmt.Sprintf("%032x", md5.Sum([]byte(
		"api_keyKEY"+"artistArtist"+"methodtrack.scrobble"+"skSK"+"trackTrack"+"SECRET")))
	if got := c.sign(params); got != want {
		t.Errorf("sign = %s, want %s", got, want)
	}
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("PANOBBLE_LASTFM_ROOT", srv.URL)
	c := NewClient("KEY", "SECRET")
	c.SessionKey = "SK"
	return c
}

func TestScrobbleBatchEncoding(t *testing.T) {
	var form url.Values
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.PostForm
		fmt.Fprint(w, `{"scrobbles":{"@attr":{"accepted":2,"ignored":0}}}`)
	})

	ts := time.Unix(1700000000, 0)
	err := c.Scrobble(context.Background(), []scrobble.Track{
		{Artist: "A0", Title: "T0", Album: "L0", Timestamp: ts, Duration: 3 * time.Minute},
		{Artist: "A1", Title: "T1", Timestamp: ts.Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}

	checks := map[string]string{
		"method":          "track.scrobble",
		"artist[0]":       "A0",
		"track[0]":        "T0",
		"album[0]":        "L0",
		"duration[0]":     "180",
		"timestamp[0]":    "1700000000",
		"artist[1]":       "A1",
		"timestamp[1]":    "1700003600",
		"chosenByUser[1]": "1",
		"format":          "json",
	}
	for k, want := range checks {
		if got := form.Get(k); got != want {
			t.Errorf("form[%s] = %q, want %q", k, got, want)
		}
	}
	if form.Get("album[1]") != "" {
		t.Error("empty album should not be sent")
	}
	if form.Get("api_sig") == "" {
		t.Error("api_sig missing")
	}
}

func TestSingleScrobbleUsesUnindexedParams(t *testing.T) {
	var form url.Values
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.PostForm
		fmt.Fprint(w, `{"scrobbles":{"@attr":{"accepted":1,"ignored":0}}}`)
	})
	err := c.Scrobble(context.Background(), []scrobble.Track{
		{Artist: "A", Title: "T", Timestamp: time.Unix(1700000000, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("artist") != "A" || form.Get("chosenByUser") != "1" {
		t.Errorf("unexpected form: %v", form)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		body        string
		status      int
		retryable   bool
		terminal    bool
		needsReauth bool
	}{
		{`{"error":9,"message":"Invalid session key"}`, 403, false, false, true},
		{`{"error":29,"message":"Rate limit exceeded"}`, 429, true, false, false},
		{`{"error":11,"message":"Service Offline"}`, 503, true, false, false},
		{`{"error":6,"message":"Invalid parameters"}`, 400, false, true, false},
		{`{"error":7,"message":"Invalid resource"}`, 400, false, true, false},
		{`not json at all`, 502, true, false, false},
	}
	for _, tc := range cases {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})
		err := c.NowPlaying(context.Background(), scrobble.Track{Artist: "A", Title: "T"})
		if err == nil {
			t.Fatalf("expected error for %q", tc.body)
		}
		if got := scrobble.Retryable(err); got != tc.retryable {
			t.Errorf("%q: Retryable = %v, want %v", tc.body, got, tc.retryable)
		}
		if got := scrobble.Terminal(err); got != tc.terminal {
			t.Errorf("%q: Terminal = %v, want %v", tc.body, got, tc.terminal)
		}
		if got := scrobble.NeedsReauth(err); got != tc.needsReauth {
			t.Errorf("%q: NeedsReauth = %v, want %v", tc.body, got, tc.needsReauth)
		}
	}
}

func TestDurationGate(t *testing.T) {
	var form url.Values
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form = r.PostForm
		fmt.Fprint(w, `{"nowplaying":{}}`)
	})
	// 10s track: duration must be omitted (safeDuration gate 30s..1h).
	c.NowPlaying(context.Background(), scrobble.Track{Artist: "A", Title: "T", Duration: 10 * time.Second})
	if form.Get("duration") != "" {
		t.Errorf("short duration should be omitted, got %q", form.Get("duration"))
	}
}
