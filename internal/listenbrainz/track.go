package listenbrainz

import (
	"context"
	"fmt"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Compile-time check: *Client implements scrobble.Scrobbler.
var _ scrobble.Scrobbler = (*Client)(nil)

func (c *Client) Name() string { return "listenbrainz" }

// NowPlaying submits a playing_now listen (no timestamp).
func (c *Client) NowPlaying(ctx context.Context, t scrobble.Track) error {
	return c.submitListens(ctx, "playing_now", []payload{c.toPayload(t, false)})
}

// Scrobble submits listens: listen type "single" for one, "import" for a
// batch, mirroring upstream.
func (c *Client) Scrobble(ctx context.Context, ts []scrobble.Track) error {
	if len(ts) == 0 {
		return nil
	}
	if len(ts) > 50 {
		return fmt.Errorf("listenbrainz: batch too large: %d > 50", len(ts))
	}
	listenType := "single"
	if len(ts) > 1 {
		listenType = "import"
	}
	ps := make([]payload, len(ts))
	for i, t := range ts {
		ps[i] = c.toPayload(t, true)
	}
	return c.submitListens(ctx, listenType, ps)
}
