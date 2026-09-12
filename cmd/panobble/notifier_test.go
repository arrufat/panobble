package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/notify"
	"github.com/arrufat/panobble/internal/scrobble"
)

type fakeServer struct {
	mu      sync.Mutex
	nextID  uint32
	notifs  []notify.Notification
	closed  []uint32
	returns []uint32 // ids handed out, parallel to notifs
}

func (f *fakeServer) Notify(_ context.Context, n notify.Notification) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := n.ReplacesID
	if id == 0 {
		f.nextID++
		id = f.nextID
	}
	f.notifs = append(f.notifs, n)
	f.returns = append(f.returns, id)
	return id, nil
}

func (f *fakeServer) CloseNotification(_ context.Context, id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notifs)
}

func (f *fakeServer) last() notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifs[len(f.notifs)-1]
}

func (f *fakeServer) closedIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.closed...)
}

// fakeLastfm gates GetTrackInfo and Love on channels so tests control timing.
type fakeLastfm struct {
	infoGate chan struct{} // receive = release one GetTrackInfo
	info     lastfm.TrackInfo
	infoErr  error
	loveGate chan struct{}
	loveErr  error

	mu    sync.Mutex
	loves []string // "love:Title" / "unlove:Title"
}

func newFakeLastfm() *fakeLastfm {
	return &fakeLastfm{infoGate: make(chan struct{}, 8), loveGate: make(chan struct{}, 8)}
}

func (f *fakeLastfm) GetTrackInfo(ctx context.Context, artist, title, username string) (lastfm.TrackInfo, error) {
	select {
	case <-f.infoGate:
	case <-ctx.Done():
		return lastfm.TrackInfo{}, ctx.Err()
	}
	return f.info, f.infoErr
}

func (f *fakeLastfm) Love(ctx context.Context, artist, title string) error {
	return f.setLove(ctx, "love", title)
}

func (f *fakeLastfm) Unlove(ctx context.Context, artist, title string) error {
	return f.setLove(ctx, "unlove", title)
}

func (f *fakeLastfm) setLove(ctx context.Context, kind, title string) error {
	select {
	case <-f.loveGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	f.mu.Lock()
	f.loves = append(f.loves, kind+":"+title)
	f.mu.Unlock()
	return f.loveErr
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func settle() { time.Sleep(20 * time.Millisecond) }

func newTestNotifier(t *testing.T, lfm *fakeLastfm) (*notifier, *fakeServer) {
	t.Helper()
	srv := &fakeServer{}
	var api lastfmAPI
	if lfm != nil {
		api = lfm
	}
	n := newNotifierWith(srv, api, slog.New(slog.DiscardHandler))
	n.username = "me"
	go n.run()
	t.Cleanup(n.close)
	return n, srv
}

var (
	trackA = scrobble.Track{Artist: "A", Title: "Alpha", Album: "LP", AppID: "p", Timestamp: time.Unix(100, 0)}
	trackB = scrobble.Track{Artist: "B", Title: "Beta", Album: "LP", AppID: "p", Timestamp: time.Unix(200, 0)}
)

func TestNotifierFetchesInfoBeforeFirstNotify(t *testing.T) {
	lfm := newFakeLastfm()
	lfm.info = lastfm.TrackInfo{UserPlayCount: 42, UserLoved: true}
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	settle()
	if srv.count() != 0 {
		t.Fatal("notified before track info arrived")
	}
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })

	got := srv.last()
	if got.ReplacesID != 0 || got.Summary != "A — Alpha" {
		t.Errorf("unexpected first notification: %+v", got)
	}
	if !strings.Contains(got.Body, "42 scrobbles") || !strings.Contains(got.Body, "♥ Loved") {
		t.Errorf("body = %q", got.Body)
	}
	if len(got.Actions) != 4 || got.Actions[0] != "unlove" || got.Actions[2] != "open" {
		t.Errorf("actions = %v", got.Actions)
	}
}

func TestNotifierOpenAction(t *testing.T) {
	lfm := newFakeLastfm()
	lfm.info = lastfm.TrackInfo{URL: "https://www.last.fm/music/A/_/Alpha"}
	n, srv := newTestNotifier(t, lfm)
	var opened []string
	var mu sync.Mutex
	n.open = func(url string) error { mu.Lock(); opened = append(opened, url); mu.Unlock(); return nil }

	n.Playing(trackA)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })
	n.post(func() { n.onAction(srv.returns[0], "open") })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(opened) == 1 })
	if opened[0] != "https://www.last.fm/music/A/_/Alpha" {
		t.Errorf("opened %q", opened[0])
	}

	// Without info (lookup failed) the URL is constructed.
	lfm.infoErr = errors.New("down")
	n.Ended(trackA)
	n.Playing(trackB)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 2 })
	n.post(func() { n.onAction(srv.returns[1], "open") })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(opened) == 2 })
	if opened[1] != "https://www.last.fm/music/B/_/Beta" {
		t.Errorf("fallback opened %q", opened[1])
	}
}

func TestNotifierWithoutLastfm(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	n.Playing(trackA)
	waitFor(t, func() bool { return srv.count() == 1 })
	got := srv.last()
	if len(got.Actions) != 0 || strings.Contains(got.Body, "scrobble") {
		t.Errorf("ListenBrainz-only notification must have no love/count: %+v", got)
	}
	if got.AppIcon != "audio-x-generic" {
		t.Errorf("icon = %q", got.AppIcon)
	}
}

func TestNotifierStaleInfoDiscarded(t *testing.T) {
	lfm := newFakeLastfm()
	lfm.info = lastfm.TrackInfo{UserPlayCount: 7}
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	n.Ended(trackA)
	n.Playing(trackB)
	settle()
	lfm.infoGate <- struct{}{} // releases A's fetch (stale)
	lfm.infoGate <- struct{}{} // releases B's fetch
	waitFor(t, func() bool { return srv.count() == 1 })
	settle()
	if srv.count() != 1 || srv.last().Summary != "B — Beta" {
		t.Errorf("stale info leaked: %+v", srv.notifs)
	}
}

func TestNotifierQualifiedAndLove(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })
	id := srv.returns[0]

	n.Qualified(trackA)
	waitFor(t, func() bool { return srv.count() == 2 })
	if got := srv.last(); got.ReplacesID != id || !strings.HasPrefix(got.Summary, "✓ ") {
		t.Errorf("qualified render: %+v", got)
	}

	n.client = nil
	n.post(func() { n.onAction(id, "love") })
	lfm.loveGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 3 })
	got := srv.last()
	if got.Actions[0] != "unlove" || !strings.Contains(got.Body, "♥ Loved") {
		t.Errorf("love render: %+v", got)
	}
	if len(lfm.loves) != 1 || lfm.loves[0] != "love:Alpha" {
		t.Errorf("loves = %v", lfm.loves)
	}
}

func TestNotifierLoveRace(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })
	id := srv.returns[0]

	n.post(func() { n.onAction(id, "love") })
	n.Ended(trackA)
	n.Playing(trackB)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 2 })

	lfm.loveGate <- struct{}{} // A's love completes after B is shown
	settle()
	if srv.count() != 2 {
		t.Errorf("stale love result re-rendered: %+v", srv.notifs)
	}
	if got := srv.last(); got.Actions[0] != "love" {
		t.Errorf("B must not inherit A's love state: %+v", got)
	}
}

func TestNotifierLoveFailureKeepsState(t *testing.T) {
	lfm := newFakeLastfm()
	lfm.loveErr = errors.New("boom")
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })
	n.post(func() { n.onAction(srv.returns[0], "love") })
	lfm.loveGate <- struct{}{}
	settle()
	if srv.count() != 1 {
		t.Errorf("failed love must not re-render: %+v", srv.notifs)
	}
}

func TestNotifierDismissed(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)

	n.Playing(trackA)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 1 })
	id := srv.returns[0]

	n.post(func() { n.onClosed(id, notify.ReasonDismissed) })
	n.Qualified(trackA)
	settle()
	if srv.count() != 1 {
		t.Fatal("dismissed notification was resurrected")
	}

	n.Ended(trackA)
	settle()
	if len(srv.closedIDs()) != 0 {
		t.Error("must not close a notification the user already dismissed")
	}

	n.Playing(trackB)
	lfm.infoGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 2 })
	if got := srv.last(); got.ReplacesID != 0 {
		t.Errorf("next play must start a fresh notification, got replaces %d", got.ReplacesID)
	}
}

func TestNotifierEndedCloses(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	n.Playing(trackA)
	waitFor(t, func() bool { return srv.count() == 1 })
	n.Ended(trackA)
	waitFor(t, func() bool { return len(srv.closedIDs()) == 1 })
	if srv.closedIDs()[0] != srv.returns[0] {
		t.Errorf("closed %v, shown %v", srv.closedIDs(), srv.returns)
	}
	n.Qualified(trackA) // late event for an ended play
	settle()
	if srv.count() != 1 {
		t.Error("events after Ended must be ignored")
	}
}

func TestNotifierTrackChangeReplacesInPlace(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	n.Playing(trackA)
	waitFor(t, func() bool { return srv.count() == 1 })
	// Track change without an explicit Ended (e.g. a second player).
	n.Playing(trackB)
	waitFor(t, func() bool { return srv.count() == 2 })
	if got := srv.last(); got.ReplacesID != srv.returns[0] {
		t.Errorf("expected in-place update, got %+v", got)
	}
}

func TestNotifierCloseRemovesNotification(t *testing.T) {
	srv := &fakeServer{}
	n := newNotifierWith(srv, nil, slog.New(slog.DiscardHandler))
	go n.run()
	n.Playing(trackA)
	waitFor(t, func() bool { return srv.count() == 1 })
	n.close()
	if ids := srv.closedIDs(); len(ids) != 1 {
		t.Errorf("close must remove the notification, closed %v", ids)
	}
}

func TestBuildNotification(t *testing.T) {
	base := scrobble.Track{Artist: "Simon & Garfunkel", Title: "<Sound>", Album: "A & B"}
	for _, tc := range []struct {
		name    string
		s       shown
		canLove bool
		summary string
		body    string
		actions []string
	}{
		{"plain", shown{track: base}, false, "Simon & Garfunkel — <Sound>", "<i>A &amp; B</i>", nil},
		{"qualified, loved, plural", shown{track: base, qualified: true, hasInfo: true, count: 2, loved: true}, true,
			"✓ Simon & Garfunkel — <Sound>", "<i>A &amp; B</i> · 2 scrobbles · ♥ Loved", []string{"unlove", "♥ Unlove", "open", "Track page"}},
		{"singular", shown{track: base, hasInfo: true, count: 1}, true,
			"Simon & Garfunkel — <Sound>", "<i>A &amp; B</i> · 1 scrobble", []string{"love", "♡ Love", "open", "Track page"}},
		{"zero count omitted", shown{track: base, hasInfo: true}, true,
			"Simon & Garfunkel — <Sound>", "<i>A &amp; B</i>", []string{"love", "♡ Love", "open", "Track page"}},
		{"no info", shown{track: scrobble.Track{Artist: "A", Title: "T"}}, true,
			"A — T", "", []string{"love", "♡ Love", "open", "Track page"}},
	} {
		got := buildNotification(&tc.s, tc.canLove, "icon", urgencyNormal)
		if got.Summary != tc.summary {
			t.Errorf("%s: summary = %q, want %q", tc.name, got.Summary, tc.summary)
		}
		if got.Body != tc.body {
			t.Errorf("%s: body = %q, want %q", tc.name, got.Body, tc.body)
		}
		if strings.Join(got.Actions, ",") != strings.Join(tc.actions, ",") {
			t.Errorf("%s: actions = %v, want %v", tc.name, got.Actions, tc.actions)
		}
		if got.Hints["resident"].Value() != true || got.Timeout != -1 {
			t.Errorf("%s: hints/timeout = %v / %d", tc.name, got.Hints, got.Timeout)
		}
		if _, ok := got.Hints["image-path"]; ok {
			t.Errorf("%s: only app_icon carries the icon", tc.name)
		}
		if got.Hints["urgency"].Value() != urgencyNormal {
			t.Errorf("%s: urgency = %v", tc.name, got.Hints["urgency"].Value())
		}
	}

	silent := buildNotification(&shown{track: base}, true, "icon", urgencyLow)
	if silent.Hints["urgency"].Value() != urgencyLow {
		t.Errorf("silent urgency = %v", silent.Hints["urgency"].Value())
	}
}
