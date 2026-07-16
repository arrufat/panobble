package store

import "github.com/arrufat/panobble/internal/lastfm"

func sessionFixture() lastfm.Session {
	return lastfm.Session{Username: "user", SessionKey: "key"}
}
