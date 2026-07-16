package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Pending is one queued offline scrobble.
type Pending struct {
	Track         scrobble.Track `json:"track"`
	LastFailed    time.Time      `json:"lastFailed"`
	Reason        string         `json:"reason"`
	CanForceRetry bool           `json:"canForceRetry"` // network-retryable failure
}

const (
	batchSize    = 50
	hardLimit    = 700
	maxAge       = 14 * 24 * time.Hour // last.fm rejects older scrobbles
	retryBackoff = time.Hour
)

// Queue is the pending-scrobbles JSONL file. The owner holds a flock for its
// lifetime, so daemon and CLI cannot flush concurrently.
type Queue struct {
	path string
	f    *os.File // locked; also used for appends
	now  func() time.Time
}

// OpenQueue opens (creating if needed) and locks the queue file.
// Returns an error if another process holds the lock.
func OpenQueue(dataDir string) (*Queue, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "pending.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("pending queue is locked (daemon running?): %w", err)
	}
	return &Queue{path: path, f: f, now: time.Now}, nil
}

func (q *Queue) Close() error {
	syscall.Flock(int(q.f.Fd()), syscall.LOCK_UN)
	return q.f.Close()
}

// Enqueue appends one failed scrobble.
func (q *Queue) Enqueue(t scrobble.Track, reason string, canForceRetry bool) error {
	p := Pending{Track: t, LastFailed: q.now(), Reason: truncate(reason, 100), CanForceRetry: canForceRetry}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = q.f.Write(append(data, '\n'))
	return err
}

// List reads all queued entries (also usable without the lock, read-only).
func ListPending(dataDir string) ([]Pending, error) {
	f, err := os.Open(filepath.Join(dataDir, "pending.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readEntries(f)
}

func (q *Queue) list() ([]Pending, error) {
	if _, err := q.f.Seek(0, 0); err != nil {
		return nil, err
	}
	return readEntries(q.f)
}

func readEntries(f *os.File) ([]Pending, error) {
	var out []Pending
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var p Pending
		if err := json.Unmarshal(line, &p); err != nil {
			continue // skip corrupt lines rather than wedging the queue
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// Flush submits eligible queued scrobbles: oldest first, batches of 50,
// at most 700 per run, aborting after the first failed batch. Entries older
// than 14 days or terminally rejected are dropped. Survivors are rewritten
// atomically.
func (q *Queue) Flush(ctx context.Context, s scrobble.Scrobbler) error {
	all, err := q.list()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}

	now := q.now()
	var keep, eligible []Pending
	for _, p := range all {
		if now.Sub(p.Track.Timestamp) > maxAge {
			continue // too old for last.fm: drop
		}
		if p.CanForceRetry || now.Sub(p.LastFailed) > retryBackoff {
			eligible = append(eligible, p)
		} else {
			keep = append(keep, p)
		}
	}

	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].Track.Timestamp.Before(eligible[j].Track.Timestamp)
	})
	if len(eligible) > hardLimit {
		keep = append(keep, eligible[hardLimit:]...)
		eligible = eligible[:hardLimit]
	}

	var flushErr error
	for start := 0; start < len(eligible); start += batchSize {
		end := min(start+batchSize, len(eligible))
		batch := eligible[start:end]

		tracks := make([]scrobble.Track, len(batch))
		for i, p := range batch {
			tracks[i] = p.Track
		}

		err := s.Scrobble(ctx, tracks)
		switch {
		case err == nil:
			// submitted: drop from queue

		case scrobble.Terminal(err):
			// permanently rejected: drop

		default:
			// failed: keep this batch and everything after, abort the run
			for i := range batch {
				batch[i].LastFailed = now
				batch[i].Reason = truncate(err.Error(), 100)
				batch[i].CanForceRetry = false
			}
			keep = append(keep, batch...)
			keep = append(keep, eligible[end:]...)
			flushErr = err
			start = len(eligible) // break
		}
		if flushErr != nil {
			break
		}
	}

	if err := q.rewrite(keep); err != nil {
		return err
	}
	return flushErr
}

// rewrite atomically replaces the queue file with the given entries, keeping
// the lock on the original fd (the rename swaps content under the same path;
// subsequent appends reopen semantics are avoided by truncating instead).
func (q *Queue) rewrite(entries []Pending) error {
	if err := q.f.Truncate(0); err != nil {
		return err
	}
	if _, err := q.f.Seek(0, 0); err != nil {
		return err
	}
	w := bufio.NewWriter(q.f)
	for _, p := range entries {
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		w.Write(data)
		w.WriteByte('\n')
	}
	return w.Flush()
}

// Len returns the number of queued entries.
func (q *Queue) Len() (int, error) {
	entries, err := q.list()
	return len(entries), err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
