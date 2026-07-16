package tracker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/scrobble"
)

type fakeSubmitter struct {
	mu         sync.Mutex
	nowPlaying []scrobble.Track
	qualified  []scrobble.Track
	scrobbles  []scrobble.Track
}

func (f *fakeSubmitter) NowPlaying(t scrobble.Track) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nowPlaying = append(f.nowPlaying, t)
}

func (f *fakeSubmitter) Qualified(t scrobble.Track) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qualified = append(f.qualified, t)
}

func (f *fakeSubmitter) qualifiedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.qualified)
}

func (f *fakeSubmitter) Scrobble(t scrobble.Track) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scrobbles = append(f.scrobbles, t)
}

func (f *fakeSubmitter) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.nowPlaying), len(f.scrobbles)
}

func (f *fakeSubmitter) lastScrobble() scrobble.Track {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scrobbles) == 0 {
		return scrobble.Track{}
	}
	return f.scrobbles[len(f.scrobbles)-1]
}

type harness struct {
	tr     *Tracker
	sub    *fakeSubmitter
	events chan mpris.Event
	cancel context.CancelFunc
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Players.Allowed = []string{"testplayer"}
	if mutate != nil {
		mutate(&cfg)
	}

	pipeline, err := clean.NewPipeline(cfg.Cleanup, cfg.Rules)
	if err != nil {
		t.Fatal(err)
	}

	sub := &fakeSubmitter{}
	tr := New(cfg, pipeline, sub, slog.New(slog.DiscardHandler))
	// millisecond-scale timings
	tr.metaWait = 5 * time.Millisecond
	tr.unknownDelay = 60 * time.Millisecond
	tr.minDelayFloor = 30 * time.Millisecond

	events := make(chan mpris.Event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	go tr.Run(ctx, events)
	t.Cleanup(cancel)

	return &harness{tr: tr, sub: sub, events: events, cancel: cancel}
}

const testBus = "org.mpris.MediaPlayer2.testplayer"

func (h *harness) sendMetadata(md mpris.Metadata) {
	h.events <- mpris.Event{BusName: testBus, AppID: testBus, Metadata: &md, Position: -1, CanGoNext: true}
}

func (h *harness) sendStatus(s mpris.PlaybackStatus, pos time.Duration) {
	h.events <- mpris.Event{BusName: testBus, AppID: testBus, Status: &s, Position: pos, CanGoNext: true}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// duration 0 in these tests → unknownDelay (60ms) is the scrobble point.
var testTrack = mpris.Metadata{Artist: "Artist", Title: "Song", Album: "Album"}

func TestHappyPathScrobble(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)

	if !waitFor(t, time.Second, func() bool { np, _ := h.sub.counts(); return np == 1 }) {
		t.Fatal("now-playing not sent")
	}
	// Threshold crossed: qualified, but not yet committed.
	if !waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 }) {
		t.Fatal("track never qualified")
	}
	time.Sleep(30 * time.Millisecond)
	if _, s := h.sub.counts(); s != 0 {
		t.Fatal("scrobble committed before track end")
	}

	// Track ends (stop): commit.
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("scrobble not committed on stop")
	}
	got := h.sub.lastScrobble()
	if got.Artist != "Artist" || got.Title != "Song" {
		t.Errorf("unexpected scrobble: %+v", got)
	}
}

func TestCommitOnTrackChange(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 })

	// Next track begins: previous commits, new one plays on.
	h.sendMetadata(mpris.Metadata{Artist: "Other", Title: "Next", Album: "Album"})
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("previous track not committed on change")
	}
	if got := h.sub.lastScrobble(); got.Title != "Song" {
		t.Errorf("committed wrong track: %q", got.Title)
	}
}

func TestCommitOnPlayerExit(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 })

	h.events <- mpris.Event{BusName: testBus, AppID: testBus, Removed: true, Position: -1}
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("scrobble not committed on player exit")
	}
}

func TestCommitOnShutdown(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 })

	close(h.events) // daemon shutdown: Run drains
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("scrobble not committed on shutdown")
	}
}

func TestPauseHoldsCommit(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 })

	// A pause holds the scrobble (the user may resume)...
	h.sendStatus(mpris.StatusPaused, 30*time.Second)
	time.Sleep(50 * time.Millisecond)
	if _, s := h.sub.counts(); s != 0 {
		t.Fatal("pause must not commit")
	}
	// ...a stop commits it.
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("scrobble not committed on stop after pause")
	}
}

func TestSkipBeforeThreshold(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)

	// Wait for now-playing (meta debounce done), then switch tracks before
	// the 60ms qualification point.
	waitFor(t, time.Second, func() bool { np, _ := h.sub.counts(); return np == 1 })
	h.sendMetadata(mpris.Metadata{Artist: "Other", Title: "Next", Album: "Album"})

	// Second track qualifies; stop commits it. The first must never appear.
	waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 })
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("second track never scrobbled")
	}
	if got := h.sub.lastScrobble(); got.Title != "Next" {
		t.Errorf("first track should have been skipped, scrobbled %q", got.Title)
	}
}

func TestPauseCancelsAndResumeAccounts(t *testing.T) {
	h := newHarness(t, nil)
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { np, _ := h.sub.counts(); return np == 1 })

	h.sendStatus(mpris.StatusPaused, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if h.sub.qualifiedCount() != 0 {
		t.Fatal("paused track should not qualify")
	}

	// Resume mid-track (position beyond startPosLimit): still qualifies,
	// and a stop commits it.
	h.sendStatus(mpris.StatusPlaying, 20*time.Millisecond)
	if !waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 }) {
		t.Fatal("resumed track never qualified")
	}
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("resumed track never scrobbled")
	}
}

// Multiple pause/resume cycles must accumulate only actually-played time:
// pause gaps don't count, and earlier segments aren't double-counted (the
// bug would make the track qualify far too early). The committed scrobble
// keeps the original play-start timestamp, not the resume time.
func TestMultiPauseAccounting(t *testing.T) {
	h := newHarness(t, nil)
	h.tr.unknownDelay = 300 * time.Millisecond // threshold for this test

	t0 := time.Now()
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)

	time.Sleep(40 * time.Millisecond)
	h.sendStatus(mpris.StatusPaused, 60*time.Second) // played ~40ms
	time.Sleep(200 * time.Millisecond)               // long pause gap
	h.sendStatus(mpris.StatusPlaying, 60*time.Second)

	time.Sleep(40 * time.Millisecond)
	h.sendStatus(mpris.StatusPaused, 60*time.Second) // played ~80ms total
	time.Sleep(50 * time.Millisecond)
	resumedAt := time.Now()
	h.sendStatus(mpris.StatusPlaying, 60*time.Second)

	// ~80ms of 300ms played: needs ~220ms more. With the double-counting
	// bug, timePlayed would already exceed the threshold and qualification
	// would fire at the 30ms floor.
	time.Sleep(120 * time.Millisecond)
	if h.sub.qualifiedCount() != 0 {
		t.Fatal("qualified too early: pause gaps counted as played time")
	}
	if !waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 }) {
		t.Fatal("track never qualified after resumes")
	}

	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("never committed")
	}
	got := h.sub.lastScrobble()
	if got.Timestamp.Sub(t0) > 150*time.Millisecond {
		t.Errorf("timestamp should be the original play start (t0+%v), not near a resume (t0+%v)",
			got.Timestamp.Sub(t0), resumedAt.Sub(t0))
	}
}

func TestDisallowedPlayerIgnored(t *testing.T) {
	h := newHarness(t, nil)
	md := testTrack
	status := mpris.StatusPlaying
	h.events <- mpris.Event{BusName: "org.mpris.MediaPlayer2.other", AppID: "org.mpris.MediaPlayer2.other",
		Metadata: &md, Position: -1}
	h.events <- mpris.Event{BusName: "org.mpris.MediaPlayer2.other", AppID: "org.mpris.MediaPlayer2.other",
		Status: &status, Position: 0}

	time.Sleep(120 * time.Millisecond)
	np, s := h.sub.counts()
	if np != 0 || s != 0 {
		t.Errorf("disallowed player submitted: np=%d s=%d", np, s)
	}
}

func TestRequireAlbumDropsAlbumless(t *testing.T) {
	h := newHarness(t, nil) // require_album is on by default
	h.sendMetadata(mpris.Metadata{Artist: "Artist", Title: "Some Video"})
	h.sendStatus(mpris.StatusPlaying, 0)

	time.Sleep(120 * time.Millisecond)
	np, s := h.sub.counts()
	if np != 0 || s != 0 {
		t.Errorf("albumless track submitted: np=%d s=%d", np, s)
	}

	// With an album it goes through.
	h.sendMetadata(testTrack)
	h.sendStatus(mpris.StatusPlaying, 0)
	if !waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 }) {
		t.Fatal("track with album never qualified")
	}
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("track with album never scrobbled")
	}
}

// Chromium publishes metadata incrementally: title/artist first, album in a
// later update. The albumless intermediate is ignored (require_album), but
// the completed track must still scrobble — without any playback-status
// event in between.
func TestLateArrivingAlbumStillScrobbles(t *testing.T) {
	h := newHarness(t, nil)
	h.sendStatus(mpris.StatusPlaying, 0)
	h.sendMetadata(mpris.Metadata{Artist: "Dire Straits", Title: "Down To The Waterline"})

	time.Sleep(50 * time.Millisecond)
	if np, s := h.sub.counts(); np != 0 || s != 0 {
		t.Fatalf("albumless intermediate submitted: np=%d s=%d", np, s)
	}

	// Album arrives in a later metadata update; no new status event.
	// (No duration, so the test-scale unknownDelay applies.)
	h.sendMetadata(mpris.Metadata{Artist: "Dire Straits", Title: "Down To The Waterline",
		Album: "Dire Straits"})

	if !waitFor(t, time.Second, func() bool { return h.sub.qualifiedCount() == 1 }) {
		t.Fatal("completed track never qualified")
	}
	h.sendStatus(mpris.StatusStopped, -1)
	if !waitFor(t, time.Second, func() bool { _, s := h.sub.counts(); return s == 1 }) {
		t.Fatal("completed track never scrobbled")
	}
	if got := h.sub.lastScrobble(); got.Album != "Dire Straits" {
		t.Errorf("scrobbled without album: %+v", got)
	}
}

func TestBlockedHostnameDropped(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Players.BlockedHostnames = []string{"youtube.com"}
	})
	h.sendMetadata(mpris.Metadata{Artist: "A", Title: "T", Album: "L", URLHost: "youtube.com"})
	h.sendStatus(mpris.StatusPlaying, 0)

	time.Sleep(120 * time.Millisecond)
	if np, s := h.sub.counts(); np != 0 || s != 0 {
		t.Errorf("blocked host submitted: np=%d s=%d", np, s)
	}
}

func TestNowPlayingSuppressedOnQuickResume(t *testing.T) {
	h := newHarness(t, nil)
	md := testTrack
	md.Duration = 3 * time.Minute // known duration: window = min(135s, 4min)
	h.sendMetadata(md)
	h.sendStatus(mpris.StatusPlaying, 0)
	waitFor(t, time.Second, func() bool { np, _ := h.sub.counts(); return np == 1 })

	// Pause then resume immediately: NP must not be re-sent.
	h.sendStatus(mpris.StatusPaused, 30*time.Second)
	h.sendStatus(mpris.StatusPlaying, 30*time.Second)
	time.Sleep(50 * time.Millisecond)
	if np, _ := h.sub.counts(); np != 1 {
		t.Errorf("now-playing re-sent on quick resume: %d", np)
	}
}

func TestSpotifyAdIgnored(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Players.Allowed = []string{"spotify"}
		// off, so this test exercises the ad heuristic, not the album check
		c.Players.RequireAlbum = false
	})
	bus := "org.mpris.MediaPlayer2.spotify"
	md := mpris.Metadata{Title: "Advertisement", Artist: "Brand", AlbumArtist: "Brand"}
	h.events <- mpris.Event{BusName: bus, AppID: bus, Metadata: &md, Position: -1, CanGoNext: false}
	status := mpris.StatusPlaying
	h.events <- mpris.Event{BusName: bus, AppID: bus, Status: &status, Position: 0, CanGoNext: false}

	time.Sleep(120 * time.Millisecond)
	if np, s := h.sub.counts(); np != 0 || s != 0 {
		t.Errorf("ad submitted: np=%d s=%d", np, s)
	}
}

func TestScrobbleDelayFormula(t *testing.T) {
	const min30 = 30 * time.Second
	cases := []struct {
		duration, timePlayed time.Duration
		want                 time.Duration
	}{
		// 50% of 4min = 2min < 240s cap
		{4 * time.Minute, 0, 2 * time.Minute},
		// 50% of 10min = 5min, capped to 240s
		{10 * time.Minute, 0, 240 * time.Second},
		// 30s track: 50% = 15s, floored at 30s - 600ms
		{30 * time.Second, 0, min30 - 600*time.Millisecond},
		// unknown duration
		{0, 0, 30 * time.Second},
		// played time subtracted
		{4 * time.Minute, time.Minute, time.Minute},
		// floor at 2s
		{4 * time.Minute, 3 * time.Minute, 2 * time.Second},
	}
	for _, c := range cases {
		got := scrobbleDelay(c.duration, c.timePlayed, 50, 240*time.Second, min30,
			30*time.Second, 2*time.Second)
		if got != c.want {
			t.Errorf("scrobbleDelay(dur=%v, played=%v) = %v, want %v",
				c.duration, c.timePlayed, got, c.want)
		}
	}
}
