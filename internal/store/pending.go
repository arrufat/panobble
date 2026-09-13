package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

// Pending is one queued scrobble: either a failed submission awaiting retry,
// or a qualified track whose commit is deferred until its play ends
// (Deferred). Deferred entries are the crash journal — if the daemon dies
// before committing, the next start promotes them to normal retries.
//
// Retry entries carry the service they failed on; deferred entries are
// service-agnostic (never submitted anywhere) and fan out per service when
// promoted.
type Pending struct {
	Track         scrobble.Track `json:"track"`
	Service       string         `json:"service,omitempty"` // empty on deferred entries
	LastFailed    time.Time      `json:"lastFailed"`
	Reason        string         `json:"reason,omitempty"`
	CanForceRetry bool           `json:"canForceRetry"` // network-retryable failure
	Deferred      bool           `json:"deferred,omitempty"`
}

const (
	batchSize    = 50
	hardLimit    = 700
	maxAge       = 14 * 24 * time.Hour // last.fm rejects older scrobbles
	retryBackoff = time.Hour
)

// Queue is the pending-scrobbles JSONL file. The owner holds a flock for its
// lifetime, so daemon and CLI cannot flush concurrently; the mutex serializes
// the daemon's own goroutines.
type Queue struct {
	mu   sync.Mutex
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

// Enqueue appends one failed scrobble for the given service.
func (q *Queue) Enqueue(t scrobble.Track, service, reason string, canForceRetry bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.append(Pending{Track: t, Service: service, LastFailed: q.now(),
		Reason: truncate(reason, 100), CanForceRetry: canForceRetry})
}

// AddDeferred journals a qualified track whose commit is pending.
func (q *Queue) AddDeferred(t scrobble.Track) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.append(Pending{Track: t, Deferred: true})
}

// ResolveDeferred removes a journaled deferred entry once its commit has been
// handled (submitted, or converted into a normal retry via Enqueue).
func (q *Queue) ResolveDeferred(t scrobble.Track) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.list()
	if err != nil {
		return err
	}
	keep := all[:0]
	for _, p := range all {
		if p.Deferred && sameTrack(p.Track, t) {
			continue
		}
		keep = append(keep, p)
	}
	return q.rewrite(keep)
}

// PromoteDeferred converts leftover deferred entries (a previous daemon died
// before committing them) into immediately-retryable pendings, one per
// configured service. Returns how many tracks were promoted.
func (q *Queue) PromoteDeferred(services []string) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.list()
	if err != nil {
		return 0, err
	}
	n := 0
	var out []Pending
	for _, p := range all {
		if !p.Deferred {
			out = append(out, p)
			continue
		}
		n++
		for _, svc := range services {
			out = append(out, Pending{
				Track:         p.Track,
				Service:       svc,
				LastFailed:    q.now(),
				Reason:        "recovered after restart",
				CanForceRetry: true,
			})
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, q.rewrite(out)
}

func (q *Queue) append(p Pending) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = q.f.Write(append(data, '\n'))
	return err
}

func sameTrack(a, b scrobble.Track) bool {
	return a.Timestamp.Equal(b.Timestamp) && a.Artist == b.Artist && a.Title == b.Title
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
		if !p.Deferred && p.Service == "" {
			p.Service = "lastfm" // entry from before multi-service support
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// Flush submits eligible queued scrobbles to their services: oldest first,
// batches of 50, at most 700 per service per run, aborting a service's run
// after its first failed batch (services fail independently). Entries older
// than 14 days or terminally rejected are dropped; entries for a service
// without a configured scrobbler are kept untouched.
func (q *Queue) Flush(ctx context.Context, scrobblers []scrobble.Scrobbler) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	all, err := q.list()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}

	byName := make(map[string]scrobble.Scrobbler, len(scrobblers))
	for _, s := range scrobblers {
		byName[s.Name()] = s
	}

	now := q.now()
	var keep []Pending
	dirty := false // whether keep differs from the file on disk
	eligible := make(map[string][]Pending)
	for _, p := range all {
		switch {
		case p.Deferred:
			keep = append(keep, p) // held for commit; never flushed
		case now.Sub(p.Track.Timestamp) > maxAge:
			dirty = true // too old to submit: drop
		case byName[p.Service] == nil:
			keep = append(keep, p) // service not configured
		case p.CanForceRetry || now.Sub(p.LastFailed) > retryBackoff:
			eligible[p.Service] = append(eligible[p.Service], p)
		default:
			keep = append(keep, p)
		}
	}

	var errs []error
	for _, s := range scrobblers {
		el := eligible[s.Name()]
		if len(el) > 0 {
			dirty = true // every eligible entry is submitted, dropped or restamped
		}
		sort.Slice(el, func(i, j int) bool {
			return el[i].Track.Timestamp.Before(el[j].Track.Timestamp)
		})
		if len(el) > hardLimit {
			keep = append(keep, el[hardLimit:]...)
			el = el[:hardLimit]
		}

	service:
		for start := 0; start < len(el); start += batchSize {
			end := min(start+batchSize, len(el))
			batch := el[start:end]

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
				// failed: keep this batch and everything after, abort
				// this service's run
				for i := range batch {
					batch[i].LastFailed = now
					batch[i].Reason = truncate(err.Error(), 100)
					batch[i].CanForceRetry = false
				}
				keep = append(keep, batch...)
				keep = append(keep, el[end:]...)
				errs = append(errs, err)
				break service
			}
		}
	}

	if dirty {
		if err := q.rewrite(keep); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// rewrite replaces the queue file's content in place (truncate + write),
// keeping the flock on the original fd.
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

// Len returns the number of retryable (non-deferred) entries.
func (q *Queue) Len() (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := q.list()
	n := 0
	for _, p := range entries {
		if !p.Deferred {
			n++
		}
	}
	return n, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
