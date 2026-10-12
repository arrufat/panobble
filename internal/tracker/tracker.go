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
	"slices"
	"time"

	"github.com/arrufat/panobble/internal/clean"
	"github.com/arrufat/panobble/internal/config"
	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/scrobble"
)

const (
	metaWait          = 1 * time.Second         // debounce before cleanup + now-playing
	startPosLimit     = 1500 * time.Millisecond // "position is at track start" threshold
	durationTolerance = 1 * time.Second         // slack before a duration change counts as an update
)

// Submitter receives the tracker's output. Implementations do network I/O on
// their own goroutines; they must not call back into the tracker.
// Qualified fires when a track passes the threshold (journal it), Scrobble
// when its play ends (commit it).
type Submitter interface {
	NowPlaying(t scrobble.Track)
	Qualified(t scrobble.Track)
	Scrobble(t scrobble.Track)
}

// Listener observes plays (for the desktop notification). Playing fires once
// per play once the track is cleaned, regardless of now_playing; Qualified
// at the threshold; Ended when the play ends. Calls run on the tracker
// goroutine and must not block.
type Listener interface {
	Playing(t scrobble.Track)
	Qualified(t scrobble.Track)
	Ended(t scrobble.Track)
}

type nopListener struct{}

func (nopListener) Playing(scrobble.Track)   {}
func (nopListener) Qualified(scrobble.Track) {}
func (nopListener) Ended(scrobble.Track)     {}

const (
	timerMeta     = iota // meta debounce: clean + now-playing
	timerScrobble        // threshold crossed: qualify, hold the commit until track end
)

type timerEvent struct {
	busName string
	gen     uint64
	kind    int
}

type Tracker struct {
	cfg       config.Config
	pipeline  *clean.Pipeline
	submitter Submitter
	listener  Listener
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
		listener:      nopListener{},
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

// SetListener installs a play observer. Call before Run.
func (tr *Tracker) SetListener(l Listener) {
	if l == nil {
		l = nopListener{}
	}
	tr.listener = l
}

// Run consumes events until ctx is cancelled or the channel closes, then
// commits any qualified-but-uncommitted scrobbles before returning.
func (tr *Tracker) Run(ctx context.Context, events <-chan mpris.Event) {
	defer tr.drain()
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

// drain commits all held scrobbles (daemon shutdown).
func (tr *Tracker) drain() {
	for _, p := range tr.players {
		tr.commit(p)
		p.stopTimers()
	}
}

// commit ends the play: scrobbles it if qualified, and notifies the listener.
func (tr *Tracker) commit(p *player) {
	if p.scrobbled == stateQualified {
		p.scrobbled = stateScrobbleSubmitted
		tr.log.Info("scrobbling", "artist", p.cleaned.Artist, "title", p.cleaned.Title,
			"album", p.cleaned.Album)
		tr.submitter.Scrobble(p.cleaned)
	}
	tr.endPlay(p)
}

// announce emits Listener.Playing once per play, keeping the track so
// Qualified and Ended carry the identical value.
func (tr *Tracker) announce(p *player) {
	if p.announced != (scrobble.Track{}) {
		return
	}
	p.announced = p.cleaned
	tr.listener.Playing(p.announced)
}

// endPlay emits Listener.Ended if the play was announced.
func (tr *Tracker) endPlay(p *player) {
	if p.announced == (scrobble.Track{}) {
		return
	}
	t := p.announced
	p.announced = scrobble.Track{}
	tr.listener.Ended(t)
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
		tr.commit(p)
		tr.pause(p)
		p.stopTimers()
		delete(tr.players, ev.BusName)

	case ev.Metadata != nil:
		tr.metadataChanged(p, *ev.Metadata)

	case ev.Status != nil:
		tr.playbackStateChanged(p, *ev.Status, ev.Position, ev.CanGoNext)

	case ev.SeekedTo != nil:
		// A seek back to the start is a replay: commit the finished play
		// and allow rescrobbling.
		if *ev.SeekedTo < tr.startPosLimit && p.isPlaying &&
			p.scrobbled >= stateQualified && p.scrobbled != stateCancelled {
			tr.replay(p)
		}
	}
}

func (tr *Tracker) metadataChanged(p *player, md mpris.Metadata) {
	sameAsOld := md.Artist == p.md.Artist && md.Title == p.md.Title &&
		md.Album == p.md.Album && md.AlbumArtist == p.md.AlbumArtist
	// Tolerance: some players re-publish a track with the duration drifting
	// by tens of milliseconds, which would restart the play clock forever.
	onlyDurationUpdated := sameAsOld && absDuration(md.Duration-p.lastDuration) > durationTolerance

	// A late duration update must not restart an already-qualified track
	// (it would journal and commit a second scrobble).
	if onlyDurationUpdated && p.scrobbled >= stateQualified {
		p.md.Duration = md.Duration
		p.lastDuration = md.Duration
		return
	}

	if !sameAsOld || (onlyDurationUpdated && md.Duration > 0) {
		if !sameAsOld {
			tr.commit(p)
			p.cleaned = scrobble.Track{} // never announce the previous track's data
		}
		p.md = md
		p.urlHost = md.URLHost
		p.hash = trackHash(md, p.appID, p.busName)
		p.scrobbled = stateNone
		if !sameAsOld {
			p.timePlayed = 0 // a new track starts its own clock
		}
		if p.isPlaying {
			p.playStartTime = tr.now()
			p.startedAt = p.playStartTime
		} else {
			p.playStartTime = time.Time{}
			p.startedAt = time.Time{}
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
	if md.URLHost != "" && slices.Contains(tr.cfg.Players.BlockedHostnames, md.URLHost) {
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

	if status == mpris.StatusPlaying && p.md.IsSpotifyAdPlayback(p.appID, canGoNext) {
		tr.log.Info("ignoring spotify ad (playback)")
		tr.cancel(p)
		return
	}

	isPossiblyAtStart := p.lastDuration > 0 && position >= 0 && position <= tr.startPosLimit
	stateChanged := p.lastPlayback != status

	if !stateChanged && !(status == mpris.StatusPlaying && isPossiblyAtStart) {
		return
	}

	switch status {
	case mpris.StatusStopped:
		tr.commit(p) // a stop ends the play; a pause only holds it
		tr.pause(p)

	case mpris.StatusPaused, mpris.StatusUnknown:
		tr.pause(p)

	case mpris.StatusPlaying:
		// Resuming at the start of a qualified play is a replay too, e.g.
		// seeked back while paused, where Seeked was ignored.
		if isPossiblyAtStart && p.scrobbled == stateQualified && p.hash == p.lastScrobbleHash {
			tr.replay(p)
			break
		}
		if p.scrobbled < stateCancelled {
			p.isPlaying = true
			if p.playStartTime.IsZero() {
				p.playStartTime = tr.now() // resume: new accumulation segment
			}
			if p.startedAt.IsZero() {
				p.startedAt = tr.now()
			}

			if p.hash != p.lastScrobbleHash || isPossiblyAtStart {
				p.resetPlayClock(tr.now())
			}

			armed := p.scrobbleTimer != nil
			positionSpam := !stateChanged && p.lastScrobbleHash == p.hash &&
				tr.now().Sub(p.playStartTime) < tr.startPosLimit*2

			if !armed &&
				((isPossiblyAtStart && p.scrobbled != stateQualified) ||
					(p.scrobbled < stateQualified && !positionSpam)) {
				tr.arm(p)
			}
		}
	}

	p.lastPlayback = status
}

// replay commits the finished play and re-arms the same track from scratch,
// re-running cleanup and now-playing.
func (tr *Tracker) replay(p *player) {
	tr.commit(p)
	p.cleaned = scrobble.Track{}
	p.resetPlayClock(tr.now())
	p.scrobbled = stateNone
	tr.arm(p)
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
	p.playStartTime = time.Time{} // close the accumulation segment
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
	tr.endPlay(p)
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
	if p.startedAt.IsZero() {
		p.startedAt = p.playStartTime
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
		tr.timerCh <- timerEvent{busName: bus, gen: gen, kind: timerMeta}
	})
	p.scrobbleTimer = time.AfterFunc(delay, func() {
		tr.timerCh <- timerEvent{busName: bus, gen: gen, kind: timerScrobble}
	})

	tr.log.Debug("armed", "title", p.md.Title, "delay", delay, "timePlayed", p.timePlayed)
}

func (tr *Tracker) handleTimerEvent(te timerEvent) {
	p := tr.players[te.busName]
	if p == nil || te.gen != p.timerGen {
		return // stale timer
	}

	switch te.kind {
	case timerMeta:
		if p.scrobbled != statePrepared {
			return
		}
		cleaned, ok := tr.cleanCurrent(p)
		if !ok {
			return
		}
		p.cleaned = cleaned
		tr.announce(p)

		if tr.cfg.Scrobble.NowPlaying && tr.shouldSendNowPlaying(p) {
			p.npSentHash = p.hash
			p.npSentAt = tr.now()
			tr.submitter.NowPlaying(cleaned)
		}
		p.scrobbled = stateNowPlayingSubmitted

	case timerScrobble:
		if p.scrobbled != statePrepared && p.scrobbled != stateNowPlayingSubmitted {
			return
		}
		t := p.cleaned
		if t.Title == "" {
			// meta debounce hasn't run (sub-second delay); clean now.
			cleaned, ok := tr.cleanCurrent(p)
			if !ok {
				return
			}
			t = cleaned
		}
		p.cleaned = t
		tr.announce(p)
		p.scrobbled = stateQualified
		p.scrobbleTimer = nil // spent
		tr.log.Info("qualified", "artist", t.Artist, "title", t.Title)
		tr.submitter.Qualified(t)
		tr.listener.Qualified(p.announced)
	}
}

// cleanCurrent runs the cleanup pipeline on the player's current track,
// cancelling the play when a rule blocks it or the title parse fails.
func (tr *Tracker) cleanCurrent(p *player) (scrobble.Track, bool) {
	cleaned, res := tr.pipeline.Clean(p.rawTrack(p.startedAt), p.urlHost)
	switch {
	case res.Blocked:
		tr.log.Info("blocked by rule", "rule", res.BlockReason, "title", p.md.Title)
		tr.cancel(p)
		return scrobble.Track{}, false
	case res.ParseFailed:
		tr.log.Info("title parse failed, skipping", "title", p.md.Title)
		tr.cancel(p)
		return scrobble.Track{}, false
	}
	return cleaned, true
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
