// Package store persists panobble's state: the Last.fm session and the
// pending-scrobbles queue.
//
// The queue semantics (batching, backoff, expiry, terminal drops) are ported
// from pano-scrobbler's PendingScrobblesWorker.kt and PendingScrobblesDao.kt.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arrufat/panobble/internal/lastfm"
)

// SaveSession writes the session atomically with 0600 permissions.
func SaveSession(dataDir string, s lastfm.Session) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dataDir, "session.json"), data, 0o600)
}

// LoadSession reads the stored session. A missing file returns an error the
// caller should surface as "run panobble auth first".
func LoadSession(dataDir string) (lastfm.Session, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "session.json"))
	if err != nil {
		return lastfm.Session{}, err
	}
	var s lastfm.Session
	if err := json.Unmarshal(data, &s); err != nil {
		return lastfm.Session{}, fmt.Errorf("corrupt session.json: %w", err)
	}
	return s, nil
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
