package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/notify"
	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/tracker"
	"github.com/godbus/dbus/v5"
)

// Plain black shape: GNOME recolors symbolic icons in the theme foreground.
//
//go:embed icons/lastfm.svg
var lastfmIcon []byte

// notifyServer is the part of notify.Client the notifier uses.
type notifyServer interface {
	Notify(ctx context.Context, n notify.Notification) (uint32, error)
	CloseNotification(ctx context.Context, id uint32) error
}

// lastfmAPI is the part of lastfm.Client the notifier uses.
type lastfmAPI interface {
	GetTrackInfo(ctx context.Context, artist, title string) (lastfm.TrackInfo, error)
	Love(ctx context.Context, artist, title string) error
	Unlove(ctx context.Context, artist, title string) error
}

// notifier shows a resident desktop notification for the track being
// scrobbled, with a Love/Unlove action (after pano-scrobbler's
// PanoNotifications.android.kt and main.kt). A single worker goroutine owns
// all state; callbacks and network results are posted to it as closures.
type notifier struct {
	srv       notifyServer
	closeConn func() error // closes the D-Bus connection; nil in tests
	lfm       lastfmAPI    // nil when Last.fm is not configured
	icon      string       // app icon: absolute path to the Last.fm logo, or a themed name
	urgency   byte         // freedesktop urgency: 0 low (no banner on GNOME), 1 normal
	markup    bool         // the server supports body markup
	open      func(url string) error
	log       *slog.Logger

	cmds   chan func()
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	cur *shown // displayed play; async results compare the pointer for staleness
}

// shown is the state of the displayed notification.
type shown struct {
	id        uint32 // 0 = not posted yet
	track     scrobble.Track
	qualified bool
	count     int // user scrobbles; 0 also when track.getInfo failed
	loved     bool
	url       string // last.fm track page; empty when track.getInfo failed
}

const (
	urgencyLow    byte = 0
	urgencyNormal byte = 1

	// Marks a scrobbled track in the summary; text presentation, never emoji.
	glyphScrobbled = "✓"

	notifyTimeout = 5 * time.Second
	infoTimeout   = 5 * time.Second
	loveTimeout   = 15 * time.Second
)

var _ tracker.Listener = (*notifier)(nil)

// newNotifier connects to the notification server and starts the worker.
// lfm may be nil.
func newNotifier(cfg config.Notifications, lfm *lastfm.Client, dataDir string, log *slog.Logger) (*notifier, error) {
	client, err := notify.Connect()
	if err != nil {
		return nil, err
	}
	n := newNotifierWith(client, nil, log)
	n.closeConn = client.Close
	n.markup = serverMarkup(client, log)
	if cfg.Silent {
		n.urgency = urgencyLow
	}
	if lfm != nil {
		n.lfm = lfm
		if path, err := installIcon(dataDir); err != nil {
			log.Warn("installing notification icon failed", "err", err)
		} else {
			n.icon = path
		}
	}
	client.OnAction = func(id uint32, action string) { n.post(func() { n.onAction(id, action) }) }
	client.OnClosed = func(id uint32, reason uint32) { n.post(func() { n.onClosed(id, reason) }) }
	go client.Run()
	n.start()
	return n, nil
}

// newNotifierWith builds an unstarted notifier; lfm may be nil (untyped).
func newNotifierWith(srv notifyServer, lfm lastfmAPI, log *slog.Logger) *notifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &notifier{
		srv:     srv,
		lfm:     lfm,
		icon:    "audio-x-generic",
		urgency: urgencyNormal,
		markup:  true,
		open:    openBrowser,
		log:     log,
		cmds:    make(chan func(), 64),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// serverMarkup reports whether the server renders markup in the body.
// Servers that do not would show the tags literally. A failed query keeps
// markup on: every common server supports it.
func serverMarkup(c *notify.Client, log *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	caps, err := c.Capabilities(ctx)
	if err != nil {
		log.Debug("querying notification capabilities failed", "err", err)
		return true
	}
	return slices.Contains(caps, "body-markup")
}

// installIcon writes the embedded icon into dataDir and returns its path.
// GNOME Shell caches file icons by URI for the session (hence the content
// hash) and recolors only names ending in -symbolic.svg. Other lastfm*
// files (outdated icons) are removed.
func installIcon(dataDir string) (string, error) {
	sum := sha256.Sum256(lastfmIcon)
	name := fmt.Sprintf("lastfm-%x-symbolic.svg", sum[:4])
	path := filepath.Join(dataDir, name)
	others, _ := filepath.Glob(filepath.Join(dataDir, "lastfm*"))
	for _, f := range others {
		if filepath.Base(f) != name {
			os.Remove(f)
		}
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, lastfmIcon) {
		return path, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, lastfmIcon, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// post queues fn for the worker without blocking.
func (n *notifier) post(fn func()) {
	select {
	case n.cmds <- fn:
	default:
		n.log.Warn("notification queue full, dropping event")
	}
}

// start launches the worker goroutine.
func (n *notifier) start() { go n.run() }

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

// close removes the notification, stops the worker, then closes the
// connection.
func (n *notifier) close() {
	n.post(func() {
		if n.cur != nil {
			n.dropCurrent()
		}
		n.cancel()
	})
	select {
	case <-n.done:
	case <-time.After(notifyTimeout):
		n.log.Warn("timed out closing the notification")
		n.cancel()
	}
	if n.closeConn != nil {
		n.closeConn()
	}
}

// tracker.Listener — called on the tracker goroutine.

func (n *notifier) Playing(t scrobble.Track)   { n.post(func() { n.startPlay(t) }) }
func (n *notifier) Qualified(t scrobble.Track) { n.post(func() { n.qualify(t) }) }
func (n *notifier) Ended(t scrobble.Track)     { n.post(func() { n.endPlay(t) }) }

// startPlay replaces the displayed play. With Last.fm, track info is
// fetched before the first Notify so the notification pops once, complete.
func (n *notifier) startPlay(t scrobble.Track) {
	s := &shown{track: t}
	if n.cur != nil {
		s.id = n.cur.id
	}
	n.cur = s

	if n.lfm == nil {
		n.render()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, infoTimeout)
		defer cancel()
		info, err := n.lfm.GetTrackInfo(ctx, t.Artist, t.Title)
		n.post(func() {
			if n.cur != s {
				return // stale: the play already ended or changed
			}
			switch {
			case err == nil:
				s.count = info.UserPlayCount
				s.loved = info.UserLoved
				s.url = info.URL
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
	if n.cur == nil || n.cur.track != t {
		return
	}
	n.cur.qualified = true
	if n.cur.id != 0 {
		n.render() // otherwise the pending first render picks it up
	}
}

func (n *notifier) endPlay(t scrobble.Track) {
	if n.cur != nil && n.cur.track == t {
		n.dropCurrent()
	}
}

// dropCurrent forgets the displayed play and removes its notification.
func (n *notifier) dropCurrent() {
	if n.cur.id != 0 {
		ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)
		defer cancel()
		if err := n.srv.CloseNotification(ctx, n.cur.id); err != nil {
			n.log.Debug("closing notification failed", "err", err)
		}
	}
	n.cur = nil
}

func (n *notifier) onAction(id uint32, action string) {
	if n.cur == nil || n.cur.id != id || n.lfm == nil {
		return
	}
	s := n.cur
	var want bool
	switch action {
	case "love":
		want = true
	case "unlove":
		want = false
	case "open":
		url := s.url
		if url == "" {
			url = lastfm.TrackURL(s.track.Artist, s.track.Title)
		}
		if err := n.open(url); err != nil {
			n.log.Warn("opening track page failed", "url", url, "err", err)
		}
		return
	default:
		return
	}
	t := s.track
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
			if n.cur != s {
				return // the track changed meanwhile; the change still applied on last.fm
			}
			s.loved = want
			n.render()
		})
	}()
}

// onClosed forgets a play whose notification the user dismissed.
func (n *notifier) onClosed(id uint32, reason uint32) {
	if n.cur != nil && n.cur.id == id && reason != notify.ReasonClosed {
		n.cur = nil
	}
}

// render posts the current state; a failure drops the play.
func (n *notifier) render() {
	ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)
	defer cancel()
	id, err := n.srv.Notify(ctx, n.build(n.cur))
	if err != nil {
		n.log.Warn("showing notification failed", "err", err)
		n.cur = nil
		return
	}
	n.cur.id = id
}

// build formats the notification after upstream's Android layout:
// "✓ Title · ♥" as the summary (the server renders it bold) over
// "by Artist on Album · N scrobbles", with the actions. The check marks a
// scrobbled track; a playing one is unmarked. Only the body takes markup:
// the summary is plain text.
func (n *notifier) build(s *shown) notify.Notification {
	summary := s.track.Title
	if s.qualified {
		summary = glyphScrobbled + " " + summary
	}
	if s.loved {
		summary += " · ♥"
	}

	line := "by " + n.bold(s.track.Artist)
	if s.track.Album != "" {
		line += " on " + n.italic(s.track.Album)
	}
	parts := []string{line}
	if s.count > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", s.count, plural(s.count, "scrobble", "scrobbles")))
	}
	body := strings.Join(parts, " · ")

	var actions []string
	if n.lfm != nil {
		if s.loved {
			actions = []string{"unlove", "♥ Unlove"}
		} else {
			actions = []string{"love", "♡ Love"}
		}
		actions = append(actions, "open", "Track page")
	}

	// app_icon only; an image-path hint would add a second, large icon.
	return notify.Notification{
		ReplacesID: s.id,
		AppIcon:    n.icon,
		Summary:    summary,
		Body:       body,
		Actions:    actions,
		Hints: map[string]dbus.Variant{
			"resident": dbus.MakeVariant(true),
			"urgency":  dbus.MakeVariant(n.urgency),
		},
		Timeout: -1,
	}
}

// bold and italic mark up body text, escaping it; on a server without
// markup support the text is left as it is.
func (n *notifier) bold(s string) string { return n.wrap(s, "b") }

func (n *notifier) italic(s string) string { return n.wrap(s, "i") }

func (n *notifier) wrap(s, tag string) string {
	if !n.markup {
		return s
	}
	return "<" + tag + ">" + notify.EscapeMarkup(s) + "</" + tag + ">"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
