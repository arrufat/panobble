package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/notify"
	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/tracker"
	"github.com/godbus/dbus/v5"
)

// A raster icon on purpose: GNOME Shell may recolor SVG icons as symbolic
// (grey), which it cannot do to a PNG. Regenerate with:
//
//	rsvg-convert -w 256 -h 256 icons/lastfm.svg -o icons/lastfm.png
//
//go:embed icons/lastfm.png
var lastfmIcon []byte

// notifyServer is the slice of notify.Client the notifier uses (an
// interface so tests run without D-Bus).
type notifyServer interface {
	Notify(ctx context.Context, n notify.Notification) (uint32, error)
	CloseNotification(ctx context.Context, id uint32) error
}

// lastfmAPI is the slice of lastfm.Client the notifier uses.
type lastfmAPI interface {
	GetTrackInfo(ctx context.Context, artist, title, username string) (lastfm.TrackInfo, error)
	Love(ctx context.Context, artist, title string) error
	Unlove(ctx context.Context, artist, title string) error
}

// notifier shows a resident desktop notification for the track being
// scrobbled and toggles love on Last.fm from its action button. It mirrors
// the now-playing notification of pano-scrobbler's PanoNotifications.android.kt
// and the tray labels of main.kt.
//
// A single worker goroutine (run) owns all state: tracker callbacks, D-Bus
// signal callbacks and network results are posted to it as closures, so the
// tracker goroutine never blocks and Notify calls stay ordered.
type notifier struct {
	client   *notify.Client // nil in tests
	srv      notifyServer
	lfm      lastfmAPI // nil when Last.fm is not configured
	username string
	icon     string                 // app icon: absolute path to the Last.fm logo, or a themed name
	urgency  byte                   // freedesktop urgency: 0 low (no banner on GNOME), 1 normal
	open     func(url string) error // opens a URL in the browser; injectable for tests
	log      *slog.Logger

	cmds   chan func()
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	// owned by run()
	gen uint64 // bumped per play; async results for older generations are dropped
	cur *shown // nil = nothing displayed
}

// shown is the state of the displayed notification.
type shown struct {
	gen       uint64
	id        uint32 // 0 = not posted yet
	dismissed bool   // closed by the user: never re-Notify this play
	warned    bool   // Notify failure already logged for this play
	track     scrobble.Track
	qualified bool
	hasInfo   bool // track.getInfo succeeded
	count     int
	loved     bool
	url       string // last.fm track page
}

const (
	urgencyLow    byte = 0
	urgencyNormal byte = 1

	notifyTimeout = 5 * time.Second
	infoTimeout   = 5 * time.Second
	loveTimeout   = 15 * time.Second
)

var _ tracker.Listener = (*notifier)(nil)

func newNotifier(cfg config.Notifications, lfm *lastfm.Client, dataDir string, log *slog.Logger) (*notifier, error) {
	client, err := notify.Connect()
	if err != nil {
		return nil, err
	}
	n := newNotifierWith(client, nil, log)
	n.client = client
	if cfg.Silent {
		n.urgency = urgencyLow
	}
	if lfm != nil {
		n.lfm = lfm
		n.username = lfm.Username
		if path, err := installIcon(dataDir); err != nil {
			log.Warn("installing notification icon failed", "err", err)
		} else {
			n.icon = path
		}
	}
	client.OnAction = func(id uint32, action string) { n.post(func() { n.onAction(id, action) }) }
	client.OnClosed = func(id uint32, reason uint32) { n.post(func() { n.onClosed(id, reason) }) }
	return n, nil
}

// newNotifierWith builds a notifier around explicit dependencies. lfm may be
// nil (untyped) for a ListenBrainz-only setup.
func newNotifierWith(srv notifyServer, lfm lastfmAPI, log *slog.Logger) *notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &notifier{
		srv:     srv,
		lfm:     lfm,
		icon:    "audio-x-generic",
		urgency: urgencyNormal,
		open:    func(url string) error { return exec.Command("xdg-open", url).Start() },
		log:     log,
		cmds:    make(chan func(), 64),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// installIcon writes the embedded Last.fm logo into dataDir (GNOME loads
// notification icons by absolute path) and returns its path.
func installIcon(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "lastfm.png")
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, lastfmIcon) {
		return path, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	os.Remove(filepath.Join(dataDir, "lastfm.svg")) // earlier builds shipped an SVG
	if err := os.WriteFile(path, lastfmIcon, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// post queues fn for the worker. It never blocks the caller.
func (n *notifier) post(fn func()) {
	select {
	case n.cmds <- fn:
	default:
		n.log.Warn("notification queue full, dropping event")
	}
}

// run is the worker loop.
func (n *notifier) run() {
	defer close(n.done)
	for {
		select {
		case fn := <-n.cmds:
			fn()
		case <-n.ctx.Done():
			return
		}
	}
}

// close removes the notification and stops the worker. The D-Bus connection
// is closed only after the final CloseNotification went out.
func (n *notifier) close() {
	n.post(func() {
		if n.cur != nil && n.cur.id != 0 && !n.cur.dismissed {
			n.closeCurrent()
		}
		n.cur = nil
		n.cancel()
	})
	select {
	case <-n.done:
	case <-time.After(notifyTimeout):
		n.log.Warn("timed out closing the notification")
		n.cancel()
	}
	if n.client != nil {
		n.client.Close()
	}
}

// tracker.Listener — called on the tracker goroutine.

func (n *notifier) Playing(t scrobble.Track)   { n.post(func() { n.startPlay(t) }) }
func (n *notifier) Qualified(t scrobble.Track) { n.post(func() { n.qualify(t) }) }
func (n *notifier) Ended(t scrobble.Track)     { n.post(func() { n.endPlay(t) }) }

// startPlay begins a new play. With Last.fm configured the track info is
// fetched first (bounded) so the notification pops once, complete.
func (n *notifier) startPlay(t scrobble.Track) {
	n.gen++
	var replaces uint32
	if n.cur != nil && !n.cur.dismissed {
		replaces = n.cur.id
	}
	n.cur = &shown{gen: n.gen, id: replaces, track: t}

	if n.lfm == nil {
		n.render()
		return
	}
	gen := n.gen
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, infoTimeout)
		defer cancel()
		info, err := n.lfm.GetTrackInfo(ctx, t.Artist, t.Title, n.username)
		n.post(func() {
			if n.cur == nil || n.cur.gen != gen {
				return // stale: the play already ended or changed
			}
			switch {
			case err == nil:
				n.cur.hasInfo = true
				n.cur.count = info.UserPlayCount
				n.cur.loved = info.UserLoved
				n.cur.url = info.URL
			case lastfm.TrackNotFound(err):
				n.log.Debug("track unknown to last.fm", "artist", t.Artist, "title", t.Title)
			default:
				n.log.Warn("track info fetch failed", "err", err)
			}
			n.render()
		})
	}()
}

func (n *notifier) qualify(t scrobble.Track) {
	if n.cur == nil || !samePlay(n.cur.track, t) {
		return
	}
	n.cur.qualified = true
	if n.cur.id != 0 && !n.cur.dismissed {
		n.render()
	}
}

func (n *notifier) endPlay(t scrobble.Track) {
	if n.cur == nil || !samePlay(n.cur.track, t) {
		return
	}
	if n.cur.id != 0 && !n.cur.dismissed {
		n.closeCurrent()
	}
	n.cur = nil
	n.gen++
}

func (n *notifier) closeCurrent() {
	ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)
	defer cancel()
	if err := n.srv.CloseNotification(ctx, n.cur.id); err != nil {
		n.log.Debug("closing notification failed", "err", err)
	}
}

func (n *notifier) onAction(id uint32, action string) {
	if n.cur == nil || n.cur.id != id || n.lfm == nil {
		return
	}
	var want bool
	switch action {
	case "love":
		want = true
	case "unlove":
		want = false
	case "open":
		url := n.cur.url
		if url == "" {
			url = lastfm.TrackURL(n.cur.track.Artist, n.cur.track.Title)
		}
		if err := n.open(url); err != nil {
			n.log.Warn("opening track page failed", "url", url, "err", err)
		}
		return
	default:
		return
	}
	gen := n.cur.gen
	t := n.cur.track
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, loveTimeout)
		defer cancel()
		var err error
		if want {
			err = n.lfm.Love(ctx, t.Artist, t.Title)
		} else {
			err = n.lfm.Unlove(ctx, t.Artist, t.Title)
		}
		n.post(func() {
			if err != nil {
				n.log.Warn("love/unlove failed", "love", want, "err", err,
					"artist", t.Artist, "title", t.Title)
				return
			}
			n.log.Info("love state changed", "loved", want, "artist", t.Artist, "title", t.Title)
			if n.cur == nil || n.cur.gen != gen {
				return // the track changed meanwhile; the change still applied on last.fm
			}
			n.cur.loved = want
			n.cur.hasInfo = true
			if !n.cur.dismissed {
				n.render()
			}
		})
	}()
}

func (n *notifier) onClosed(id uint32, reason uint32) {
	if n.cur == nil || n.cur.id != id || reason == notify.ReasonClosed {
		return
	}
	n.cur.dismissed = true
}

// render posts the current state via Notify (a new notification the first
// time, an in-place update afterwards).
func (n *notifier) render() {
	ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)
	defer cancel()
	id, err := n.srv.Notify(ctx, buildNotification(n.cur, n.lfm != nil, n.icon, n.urgency))
	if err != nil {
		if !n.cur.warned {
			n.cur.warned = true
			n.log.Warn("showing notification failed", "err", err)
		}
		return
	}
	n.cur.id = id
}

// buildNotification formats the notification. Layout follows upstream's
// Android now-playing notification: "✓" once counted, "Artist — Title",
// "N scrobbles", a Love/Unlove action and a link to the track page.
func buildNotification(s *shown, canLove bool, icon string, urgency byte) notify.Notification {
	summary := s.track.Artist + " — " + s.track.Title
	if s.qualified {
		summary = "✓ " + summary
	}

	// One body line: "Album · N scrobbles · ♥ Loved", parts omitted when empty.
	var parts []string
	if s.track.Album != "" {
		parts = append(parts, "<i>"+notify.EscapeMarkup(s.track.Album)+"</i>")
	}
	if s.hasInfo && s.count > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", s.count, plural(s.count, "scrobble", "scrobbles")))
	}
	if s.hasInfo && s.loved {
		parts = append(parts, "♥ Loved")
	}

	var actions []string
	if canLove {
		if s.loved {
			actions = []string{"unlove", "♥ Unlove"}
		} else {
			actions = []string{"love", "♡ Love"}
		}
		actions = append(actions, "open", "Track page")
	}

	// app_icon only: GNOME shows it as the small source icon in the message
	// header. An image-path hint would add a second, large icon.
	return notify.Notification{
		ReplacesID: s.id,
		AppIcon:    icon,
		Summary:    summary,
		Body:       strings.Join(parts, " · "),
		Actions:    actions,
		Hints: map[string]dbus.Variant{
			"resident": dbus.MakeVariant(true),
			"urgency":  dbus.MakeVariant(urgency),
		},
		Timeout: -1,
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// samePlay matches the tracker's per-play identity: same track, same start.
func samePlay(a, b scrobble.Track) bool {
	return a.Artist == b.Artist && a.Title == b.Title && a.Album == b.Album &&
		a.AppID == b.AppID && a.Timestamp.Equal(b.Timestamp)
}
