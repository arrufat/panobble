package tracker

import (
	"hash/fnv"
	"time"

	"github.com/arrufat/panobble/internal/mpris"
	"github.com/arrufat/panobble/internal/scrobble"
)

// scrobbledState is the per-track submission progress.
type scrobbledState int

const (
	stateNone scrobbledState = iota
	statePrepared
	stateNowPlayingSubmitted
	stateQualified // passed the threshold; commit deferred to track end
	stateScrobbleSubmitted
	stateCancelled
)

// player is the per-player tracker state. All fields are owned by the
// Tracker goroutine.
type player struct {
	busName string
	appID   string

	md      mpris.Metadata // current raw metadata
	cleaned scrobble.Track // filled by the meta debounce step
	hash    uint64
	urlHost string

	lastScrobbleHash uint64
	scrobbled        scrobbledState
	isPlaying        bool
	lastPlayback     mpris.PlaybackStatus
	lastDuration     time.Duration

	// startedAt is when this play began (the scrobble timestamp).
	// playStartTime is the start of the current unpaused segment, the
	// baseline for timePlayed — kept separate so pause gaps never count
	// as played time.
	startedAt     time.Time
	playStartTime time.Time     // zero = paused / unset
	timePlayed    time.Duration // accumulated across pauses

	// now-playing resend suppression
	npSentHash uint64
	npSentAt   time.Time

	metaTimer     *time.Timer
	scrobbleTimer *time.Timer
	timerGen      uint64 // invalidates stale timer callbacks
}

func trackHash(md mpris.Metadata, appID, busName string) uint64 {
	h := fnv.New64a()
	for _, s := range []string{md.AlbumArtist, md.Artist, md.Album, md.Title, appID, busName} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *player) rawTrack(timestamp time.Time) scrobble.Track {
	return scrobble.Track{
		Artist:      p.md.Artist,
		Title:       p.md.Title,
		Album:       p.md.Album,
		AlbumArtist: p.md.AlbumArtist,
		TrackNumber: p.md.TrackNumber,
		Duration:    p.md.Duration,
		Timestamp:   timestamp,
		AppID:       p.appID,
	}
}

func (p *player) stopTimers() {
	p.timerGen++
	if p.metaTimer != nil {
		p.metaTimer.Stop()
		p.metaTimer = nil
	}
	if p.scrobbleTimer != nil {
		p.scrobbleTimer.Stop()
		p.scrobbleTimer = nil
	}
}

// scrobbleDelay computes the time until submission: delayPercent% of the
// duration, capped at delayMax, floored just under minDuration, minus time
// already played. unknownDelay is 30s and floor 2s in production
// (overridable in tests).
func scrobbleDelay(duration, timePlayed time.Duration, delayPercent int,
	delayMax, minDuration, unknownDelay, floor time.Duration) time.Duration {
	var delay time.Duration
	if duration > 0 {
		delay = duration * time.Duration(delayPercent) / 100
		if delay > delayMax {
			delay = delayMax
		}
		if f := minDuration - 600*time.Millisecond; delay < f {
			delay = f
		}
	} else {
		delay = unknownDelay
	}
	delay -= timePlayed
	if delay < floor {
		delay = floor
	}
	return delay
}
