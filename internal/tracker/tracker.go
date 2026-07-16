// Package tracker is the scrobbling state machine: it consumes normalized
// MPRIS events and decides when to send now-playing and scrobbles. A single
// event loop owns all mutable state.
//
// Ported from pano-scrobbler: MediaListener.kt (state machine, timing,
// pause/resume), PlayingTrackInfo.kt (track state, hash), and
// ScrobbleQueue.kt (meta debounce, now-playing suppression).
package tracker

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/scrobble"
)

const (
	metaWait      = 1 * time.Second         // debounce before cleanup + now-playing
	startPosLimit = 1500 * time.Millisecond // "position is at track start" threshold
)

// Submitter receives the tracker's output. Implementations do network I/O on
// their own goroutines; they must not call back into the tracker.
type Submitter interface {
	NowPlaying(t scrobble.Track)
	Scrobble(t scrobble.Track)
}

type timerEvent struct {
	busName string
	gen     uint64
	kind    int // 0 = meta debounce, 1 = scrobble
}

type Tracker struct {
	cfg       config.Config
	pipeline  *clean.Pipeline
	submitter Submitter
	log       *slog.Logger

	now func() time.Time // injectable for tests

	players map[string]*player
	timerCh chan timerEvent

	// test hooks: overridable timings
	metaWait      time.Duration
	startPosLimit time.Duration
	unknownDelay  time.Duration // scrobble delay when duration is unknown
	minDelayFloor time.Duration
}

func New(cfg config.Config, pipeline *clean.Pipeline, submitter Submitter, log *slog.Logger) *Tracker {
	return &Tracker{
		cfg:           cfg,
		pipeline:      pipeline,
		submitter:     submitter,
		log:           log,
		now:           time.Now,
		players:       make(map[string]*player),
		timerCh:       make(chan timerEvent, 16),
		metaWait:      metaWait,
		startPosLimit: startPosLimit,
		unknownDelay:  30 * time.Second,
		minDelayFloor: 2 * time.Second,
	}
}

// Run consumes events until ctx is cancelled or the channel closes.
func (tr *Tracker) Run(ctx context.Context, events <-chan mpris.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			tr.handleMprisEvent(ev)
		case te := <-tr.timerCh:
			tr.handleTimerEvent(te)
		}
	}
}

func (tr *Tracker) handleMprisEvent(ev mpris.Event) {
	if !mpris.MatchesApp(ev.AppID, tr.cfg.Players.Allowed) {
		return
	}

	p := tr.players[ev.BusName]
	if p == nil {
		if ev.Removed {
			return
		}
		p = &player{busName: ev.BusName, appID: ev.AppID}
		tr.players[ev.BusName] = p
	}

	switch {
	case ev.Removed:
		tr.pause(p)
		p.stopTimers()
		delete(tr.players, ev.BusName)

	case ev.Metadata != nil:
		tr.metadataChanged(p, *ev.Metadata)

	case ev.Status != nil:
		tr.playbackStateChanged(p, *ev.Status, ev.Position, ev.CanGoNext)

	case ev.SeekedTo != nil:
		// A seek back to the start is a replay: allow rescrobbling.
		if *ev.SeekedTo < tr.startPosLimit && p.isPlaying &&
			p.scrobbled >= stateScrobbleSubmitted {
			p.timePlayed = 0
			p.playStartTime = tr.now()
			p.scrobbled = stateNone
			tr.arm(p)
		}
	}
}

func (tr *Tracker) metadataChanged(p *player, md mpris.Metadata) {
	sameAsOld := md.Artist == p.md.Artist && md.Title == p.md.Title &&
		md.Album == p.md.Album && md.AlbumArtist == p.md.AlbumArtist
	onlyDurationUpdated := sameAsOld && md.Duration != p.md.Duration

	if !sameAsOld || (onlyDurationUpdated && md.Duration > 0) {
		p.md = md
		p.urlHost = md.URLHost
		p.hash = trackHash(md, p.appID, p.busName)
		p.scrobbled = stateNone
		if p.isPlaying {
			p.playStartTime = tr.now()
		} else {
			p.playStartTime = time.Time{}
		}

		armedForThis := p.scrobbleTimer != nil && p.lastScrobbleHash == p.hash

		switch {
		case tr.shouldIgnore(p, md):
			tr.cancel(p)

		case (!armedForThis || onlyDurationUpdated) && p.lastPlayback == mpris.StatusPlaying:
			// meta sent after play, or gapless playback
			tr.arm(p)
		}
	}

	p.lastDuration = md.Duration
}

// shouldIgnore reports tracks that must never scrobble: empty artist/title,
// blocked hostname, Spotify ad, or a missing album when require_album is on.
func (tr *Tracker) shouldIgnore(p *player, md mpris.Metadata) bool {
	if md.Artist == "" || md.Title == "" {
		return true
	}
	if md.URLHost != "" && contains(tr.cfg.Players.BlockedHostnames, md.URLHost) {
		tr.log.Info("ignoring blocked hostname", "host", md.URLHost)
		return true
	}
	if md.IsSpotifyAd() {
		tr.log.Info("ignoring spotify ad (metadata)")
		return true
	}
	if md.Album == "" && tr.cfg.Players.RequireAlbum {
		tr.log.Info("ignoring track without album", "app", p.appID, "title", md.Title)
		return true
	}
	return false
}

func (tr *Tracker) playbackStateChanged(p *player, status mpris.PlaybackStatus, position time.Duration, canGoNext bool) {
	// Desktop quirk: drop state=Playing pos=0 events while duration unknown.
	if p.lastDuration <= 0 && p.isPlaying &&
		status == mpris.StatusPlaying && position == 0 {
		return
	}

	// Spotify ad heuristic (playback variant).
	if status == mpris.StatusPlaying && !canGoNext &&
		strings.Contains(p.appID, "spotify") &&
		p.md.Album == "" && p.md.TrackNumber == 0 {
		tr.log.Info("ignoring spotify ad (playback)")
		tr.cancel(p)
		return
	}

	isPossiblyAtStart := position >= 0 && position < tr.startPosLimit
	stateChanged := p.lastPlayback != status

	if !stateChanged && !(status == mpris.StatusPlaying && isPossiblyAtStart) {
		return
	}

	switch status {
	case mpris.StatusPaused, mpris.StatusStopped, mpris.StatusUnknown:
		tr.pause(p)

	case mpris.StatusPlaying:
		if p.scrobbled < stateCancelled {
			p.isPlaying = true
			if p.playStartTime.IsZero() {
				p.playStartTime = tr.now()
			}

			if p.hash != p.lastScrobbleHash || (position >= 0 && isPossiblyAtStart) {
				p.timePlayed = 0
				p.playStartTime = tr.now()
			}

			armed := p.scrobbleTimer != nil
			positionSpam := !stateChanged && p.lastScrobbleHash == p.hash &&
				tr.now().Sub(p.playStartTime) < tr.startPosLimit*2

			if !armed &&
				((position >= 0 && isPossiblyAtStart) ||
					(p.scrobbled < stateScrobbleSubmitted && !positionSpam)) {
				tr.arm(p)
			}
		}
	}

	p.lastPlayback = status
}

// pause accumulates played time if a scrobble was pending and cancels the
// timers.
func (tr *Tracker) pause(p *player) {
	if p.lastPlayback == mpris.StatusPlaying {
		if p.scrobbleTimer != nil && p.hash == p.lastScrobbleHash && !p.playStartTime.IsZero() {
			p.timePlayed += tr.now().Sub(p.playStartTime)
		} else {
			p.timePlayed = 0
		}
	}
	p.isPlaying = false
	p.stopTimers()
}

// cancel marks the current track as never-to-scrobble. lastPlayback is kept:
// players like Chromium publish metadata incrementally, and a later update
// that completes the track (e.g. the album arriving after a require_album
// ignore) must still see Playing to arm.
func (tr *Tracker) cancel(p *player) {
	p.scrobbled = stateCancelled
	p.stopTimers()
}

// arm schedules the track: debounce metaWait, then clean + now-playing, and
// fire the scrobble at the computed delay.
func (tr *Tracker) arm(p *player) {
	if p.md.Title == "" {
		return
	}

	p.stopTimers()
	p.lastScrobbleHash = p.hash
	p.scrobbled = statePrepared
	p.isPlaying = true
	if p.playStartTime.IsZero() {
		p.playStartTime = tr.now()
	}

	delay := scrobbleDelay(
		p.md.Duration,
		p.timePlayed,
		tr.cfg.Scrobble.DelayPercent,
		time.Duration(tr.cfg.Scrobble.DelayMaxSecs)*time.Second,
		time.Duration(tr.cfg.Scrobble.MinDurationSecs)*time.Second,
		tr.unknownDelay,
		tr.minDelayFloor,
	)

	gen := p.timerGen
	bus := p.busName
	p.metaTimer = time.AfterFunc(tr.metaWait, func() {
		tr.timerCh <- timerEvent{busName: bus, gen: gen, kind: 0}
	})
	p.scrobbleTimer = time.AfterFunc(delay, func() {
		tr.timerCh <- timerEvent{busName: bus, gen: gen, kind: 1}
	})

	tr.log.Debug("armed", "title", p.md.Title, "delay", delay, "timePlayed", p.timePlayed)
}

func (tr *Tracker) handleTimerEvent(te timerEvent) {
	p := tr.players[te.busName]
	if p == nil || te.gen != p.timerGen {
		return // stale timer
	}

	switch te.kind {
	case 0: // meta debounce: clean + now-playing
		if p.scrobbled != statePrepared {
			return
		}
		cleaned, res := tr.pipeline.Clean(p.rawTrack(p.playStartTime), p.urlHost)
		switch {
		case res.Blocked:
			tr.log.Info("blocked by rule", "rule", res.BlockReason, "title", p.md.Title)
			tr.cancel(p)
			return
		case res.ParseFailed:
			tr.log.Info("title parse failed, skipping", "title", p.md.Title)
			tr.cancel(p)
			return
		}
		p.cleaned = cleaned

		if tr.cfg.Scrobble.NowPlaying && tr.shouldSendNowPlaying(p) {
			p.npSentHash = p.hash
			p.npSentAt = tr.now()
			tr.submitter.NowPlaying(cleaned)
		}
		p.scrobbled = stateNowPlayingSubmitted

	case 1: // scrobble
		if p.scrobbled != statePrepared && p.scrobbled != stateNowPlayingSubmitted {
			return
		}
		t := p.cleaned
		if t.Title == "" {
			// meta debounce hasn't run (sub-second delay); clean now.
			cleaned, res := tr.pipeline.Clean(p.rawTrack(p.playStartTime), p.urlHost)
			if res.Blocked || res.ParseFailed {
				tr.cancel(p)
				return
			}
			t = cleaned
		}
		p.scrobbled = stateScrobbleSubmitted
		tr.log.Info("scrobbling", "artist", t.Artist, "title", t.Title, "album", t.Album)
		tr.submitter.Scrobble(t)
	}
}

// shouldSendNowPlaying suppresses the resend when the same track resumed
// recently (within min(duration*3/4, 4min)).
func (tr *Tracker) shouldSendNowPlaying(p *player) bool {
	if p.npSentHash != p.hash || p.npSentAt.IsZero() {
		return true
	}
	window := 4 * time.Minute
	if p.md.Duration > 0 {
		if w := p.md.Duration * 3 / 4; w < window {
			window = w
		}
	}
	return tr.now().Sub(p.npSentAt) > window
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
