// Package scrobble defines the core Track type and the Scrobbler interface.
//
// The error taxonomy (Retryable/Terminal/NeedsReauth) is ported from
// pano-scrobbler's PendingScrobblesDao.isNetworkRetryable and
// PendingScrobblesWorker; SafeDuration from ScrobbleData.kt.
package scrobble

import (
	"context"
	"errors"
	"net"
	"time"
)

// Track is one played track, cleaned and ready for submission.
type Track struct {
	Artist      string        `json:"artist"`
	Title       string        `json:"title"`
	Album       string        `json:"album,omitempty"`
	AlbumArtist string        `json:"albumArtist,omitempty"`
	TrackNumber int           `json:"trackNumber,omitempty"`
	Duration    time.Duration `json:"duration,omitempty"` // 0 = unknown
	Timestamp   time.Time     `json:"timestamp"`          // play start, wall clock
	AppID       string        `json:"appId,omitempty"`    // normalized; not submitted
}

// SafeDuration reports the duration only when it is plausible (30s to 1h).
func (t Track) SafeDuration() (time.Duration, bool) {
	if t.Duration >= 30*time.Second && t.Duration <= time.Hour {
		return t.Duration, true
	}
	return 0, false
}

// Scrobbler submits tracks to one service.
type Scrobbler interface {
	Name() string
	NowPlaying(ctx context.Context, t Track) error
	// Scrobble submits up to 50 tracks. An "ignored" response is success.
	Scrobble(ctx context.Context, ts []Track) error
}

// Class is a service client's own classification of an API error. Service
// packages set it from their protocol knowledge (error codes, HTTP statuses);
// ClassUnknown falls back to generic HTTP-status heuristics here.
type Class int

const (
	ClassUnknown Class = iota
	ClassRetryable
	ClassTerminal
	ClassNeedsReauth
)

// APIError is a service API error with the service's own error code.
type APIError struct {
	Class      Class
	Code       int // service error code, 0 if none
	HTTPStatus int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

// Retryable reports whether the scrobble may succeed later and should be
// queued.
func Retryable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Class != ClassUnknown {
			return apiErr.Class == ClassRetryable
		}
		switch apiErr.HTTPStatus {
		case 403, 429, 500, 502, 503:
			return true
		}
		return false
	}
	// Unknown non-API error (DNS failure wrapped, connection refused, ...):
	// assume transient.
	return true
}

// Terminal reports whether the scrobble is permanently rejected and should
// be dropped.
func Terminal(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Class == ClassTerminal
	}
	return false
}

// NeedsReauth reports whether the credentials are invalid and the user must
// re-authenticate.
func NeedsReauth(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Class == ClassNeedsReauth
	}
	return false
}
