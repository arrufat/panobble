package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	fail    bool
}

func (f *fakeServer) Notify(_ context.Context, n notify.Notification) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errors.New("server down")
	}
	id := n.ReplacesID
	if id == 0 {
		f.nextID++
		id = f.nextID
	}
	f.notifs = append(f.notifs, n)
	f.returns = append(f.returns, id)
	return id, nil
}

func (f *fakeServer) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
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

func (f *fakeLastfm) GetTrackInfo(ctx context.Context, artist, title string) (lastfm.TrackInfo, error) {
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
	n.start()
	t.Cleanup(n.close)
	return n, srv
}

// showTrack starts a play, releases its info fetch and waits for the
// notification; it returns the notification id.
func showTrack(t *testing.T, n *notifier, lfm *fakeLastfm, srv *fakeServer, track scrobble.Track) uint32 {
	t.Helper()
	before := srv.count()
	n.Playing(track)
	if lfm != nil {
		lfm.infoGate <- struct{}{}
	}
	waitFor(t, func() bool { return srv.count() == before+1 })
	return srv.returns[before]
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

	id := showTrack(t, n, lfm, srv, trackA)
	n.post(func() { n.onAction(id, "open") })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(opened) == 1 })
	if opened[0] != "https://www.last.fm/music/A/_/Alpha" {
		t.Errorf("opened %q", opened[0])
	}

	// Without info (lookup failed) the URL is constructed.
	lfm.infoErr = errors.New("down")
	n.Ended(trackA)
	id = showTrack(t, n, lfm, srv, trackB)
	n.post(func() { n.onAction(id, "open") })
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(opened) == 2 })
	if opened[1] != "https://www.last.fm/music/B/_/Beta" {
		t.Errorf("fallback opened %q", opened[1])
	}
}

func TestNotifierWithoutLastfm(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	showTrack(t, n, nil, srv, trackA)
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
	id := showTrack(t, n, lfm, srv, trackA)

	n.Qualified(trackA)
	waitFor(t, func() bool { return srv.count() == 2 })
	if got := srv.last(); got.ReplacesID != id || !strings.HasPrefix(got.Summary, "✓ ") {
		t.Errorf("qualified render: %+v", got)
	}

	n.post(func() { n.onAction(id, "love") })
	lfm.loveGate <- struct{}{}
	waitFor(t, func() bool { return srv.count() == 3 })
	got := srv.last()
	if got.Actions[0] != "unlove" || !strings.Contains(got.Body, "♥ Loved") {
		t.Errorf("love render: %+v", got)
	}
	if !slices.Equal(lfm.loves, []string{"love:Alpha"}) {
		t.Errorf("loves = %v", lfm.loves)
	}
}

func TestNotifierLoveRace(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)
	id := showTrack(t, n, lfm, srv, trackA)

	n.post(func() { n.onAction(id, "love") })
	n.Ended(trackA)
	showTrack(t, n, lfm, srv, trackB)

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
	id := showTrack(t, n, lfm, srv, trackA)

	n.post(func() { n.onAction(id, "love") })
	lfm.loveGate <- struct{}{}
	settle()
	if srv.count() != 1 {
		t.Errorf("failed love must not re-render: %+v", srv.notifs)
	}
}

func TestNotifierDismissed(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)
	id := showTrack(t, n, lfm, srv, trackA)

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

	showTrack(t, n, lfm, srv, trackB)
	if got := srv.last(); got.ReplacesID != 0 {
		t.Errorf("next play must start a fresh notification, got replaces %d", got.ReplacesID)
	}
}

func TestNotifierDismissedDuringFetch(t *testing.T) {
	lfm := newFakeLastfm()
	n, srv := newTestNotifier(t, lfm)
	id := showTrack(t, n, lfm, srv, trackA)

	// Track change: B's fetch is pending while A's notification is still up.
	n.Playing(trackB)
	n.post(func() { n.onClosed(id, notify.ReasonDismissed) })
	lfm.infoGate <- struct{}{}
	settle()
	if srv.count() != 1 {
		t.Errorf("info arriving after a dismissal must not re-post: %+v", srv.notifs)
	}
}

func TestNotifierEndedCloses(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	id := showTrack(t, n, nil, srv, trackA)
	n.Ended(trackA)
	waitFor(t, func() bool { return len(srv.closedIDs()) == 1 })
	if srv.closedIDs()[0] != id {
		t.Errorf("closed %v, shown %v", srv.closedIDs(), id)
	}
	n.Qualified(trackA) // late event for an ended play
	settle()
	if srv.count() != 1 {
		t.Error("events after Ended must be ignored")
	}
}

func TestNotifierTrackChangeReplacesInPlace(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	id := showTrack(t, n, nil, srv, trackA)
	// Track change without an explicit Ended (e.g. a second player).
	showTrack(t, n, nil, srv, trackB)
	if got := srv.last(); got.ReplacesID != id {
		t.Errorf("expected in-place update, got %+v", got)
	}
}

func TestNotifierNotifyFailureDropsPlay(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	srv.setFail(true)
	n.Playing(trackA)
	n.Qualified(trackA)
	settle()
	srv.setFail(false)
	showTrack(t, n, nil, srv, trackB)
	if got := srv.last(); got.ReplacesID != 0 || got.Summary != "B — Beta" {
		t.Errorf("a failed play must be forgotten, got %+v", got)
	}
}

func TestNotifierCloseRemovesNotification(t *testing.T) {
	n, srv := newTestNotifier(t, nil)
	id := showTrack(t, n, nil, srv, trackA)
	n.close()
	if ids := srv.closedIDs(); !slices.Equal(ids, []uint32{id}) {
		t.Errorf("close must remove the notification, closed %v", ids)
	}
}

func TestInstallIcon(t *testing.T) {
	dir := t.TempDir()
	outdated := filepath.Join(dir, "lastfm-00000000-symbolic.svg")
	os.WriteFile(outdated, []byte("x"), 0o644)

	path, err := installIcon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), "lastfm-") ||
		!strings.HasSuffix(path, "-symbolic.svg") {
		t.Errorf("unexpected icon path %q", path)
	}
	if got, _ := os.ReadFile(path); string(got) != string(lastfmIcon) {
		t.Error("icon content not written")
	}
	if _, err := os.Stat(outdated); !os.IsNotExist(err) {
		t.Error("outdated icon not removed")
	}
	if again, _ := installIcon(dir); again != path {
		t.Errorf("second install returned %q, want %q", again, path)
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
		{"qualified, loved, plural", shown{track: base, qualified: true, count: 2, loved: true}, true,
			"✓ Simon & Garfunkel — <Sound>", "<i>A &amp; B</i> · 2 scrobbles · ♥ Loved", []string{"unlove", "♥ Unlove", "open", "Track page"}},
		{"singular", shown{track: base, count: 1}, true,
			"Simon & Garfunkel — <Sound>", "<i>A &amp; B</i> · 1 scrobble", []string{"love", "♡ Love", "open", "Track page"}},
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
		if !slices.Equal(got.Actions, tc.actions) {
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
