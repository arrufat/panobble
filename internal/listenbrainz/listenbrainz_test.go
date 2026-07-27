package listenbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("TOKEN", srv.URL)
	c.ClientVersion = "test"
	return c
}

func decodeListen(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Token TOKEN" {
		t.Errorf("Authorization = %q, want %q", got, "Token TOKEN")
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestScrobbleSingleEncoding(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/submit-listens" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body = decodeListen(t, r)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	ts := time.Unix(1700000000, 0)
	err := c.Scrobble(context.Background(), []scrobble.Track{
		{Artist: "A", Title: "T", Album: "L", Timestamp: ts, Duration: 3 * time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}

	if body["listen_type"] != "single" {
		t.Errorf("listen_type = %v, want single", body["listen_type"])
	}
	p := body["payload"].([]any)[0].(map[string]any)
	if p["listened_at"] != float64(1700000000) {
		t.Errorf("listened_at = %v", p["listened_at"])
	}
	md := p["track_metadata"].(map[string]any)
	if md["artist_name"] != "A" || md["track_name"] != "T" || md["release_name"] != "L" {
		t.Errorf("metadata = %v", md)
	}
	ai := md["additional_info"].(map[string]any)
	if ai["duration_ms"] != float64(180000) {
		t.Errorf("duration_ms = %v", ai["duration_ms"])
	}
	if ai["submission_client"] != "panobble" || ai["submission_client_version"] != "test" {
		t.Errorf("submission client = %v", ai)
	}
}

func TestScrobbleBatchUsesImport(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeListen(t, r)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	ts := time.Unix(1700000000, 0)
	err := c.Scrobble(context.Background(), []scrobble.Track{
		{Artist: "A0", Title: "T0", Timestamp: ts},
		{Artist: "A1", Title: "T1", Timestamp: ts.Add(time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["listen_type"] != "import" {
		t.Errorf("listen_type = %v, want import", body["listen_type"])
	}
	if n := len(body["payload"].([]any)); n != 2 {
		t.Errorf("payload len = %d, want 2", n)
	}
	// Empty album must be omitted entirely.
	md := body["payload"].([]any)[0].(map[string]any)["track_metadata"].(map[string]any)
	if _, ok := md["release_name"]; ok {
		t.Error("empty release_name should be omitted")
	}
}

func TestNowPlayingHasNoTimestamp(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeListen(t, r)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	err := c.NowPlaying(context.Background(),
		scrobble.Track{Artist: "A", Title: "T", Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if body["listen_type"] != "playing_now" {
		t.Errorf("listen_type = %v, want playing_now", body["listen_type"])
	}
	p := body["payload"].([]any)[0].(map[string]any)
	if _, ok := p["listened_at"]; ok {
		t.Error("playing_now must not carry listened_at")
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		check  func(error) bool
		name   string
	}{
		{401, scrobble.NeedsReauth, "NeedsReauth"},
		{400, scrobble.Terminal, "Terminal"},
		{429, scrobble.Retryable, "Retryable"},
		{503, scrobble.Retryable, "Retryable"},
	}
	for _, tc := range cases {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, `{"code":`, tc.status, `,"error":"nope"}`)
		})
		err := c.Scrobble(context.Background(),
			[]scrobble.Track{{Artist: "A", Title: "T", Timestamp: time.Now()}})
		if err == nil {
			t.Fatalf("HTTP %d: expected error", tc.status)
		}
		if !tc.check(err) {
			t.Errorf("HTTP %d: expected %s, got %v", tc.status, tc.name, err)
		}
	}
}

func TestValidateToken(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/validate-token" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"valid":true,"user_name":"adria"}`)
	})
	user, err := c.ValidateToken(context.Background())
	if err != nil || user != "adria" {
		t.Errorf("user = %q, err = %v", user, err)
	}

	c = testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"valid":false}`)
	})
	_, err = c.ValidateToken(context.Background())
	if !scrobble.NeedsReauth(err) {
		t.Errorf("invalid token should classify as NeedsReauth, got %v", err)
	}
}
