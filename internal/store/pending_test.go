package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
)

type fakeScrobbler struct {
	batches [][]scrobble.Track
	errs    []error // per call; nil-padded
}

func (f *fakeScrobbler) Name() string                                           { return "fake" }
func (f *fakeScrobbler) NowPlaying(ctx context.Context, t scrobble.Track) error { return nil }
func (f *fakeScrobbler) Scrobble(ctx context.Context, ts []scrobble.Track) error {
	call := len(f.batches)
	f.batches = append(f.batches, ts)
	if call < len(f.errs) {
		return f.errs[call]
	}
	return nil
}

func openTestQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := OpenQueue(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func track(artist string, ts time.Time) scrobble.Track {
	return scrobble.Track{Artist: artist, Title: "T", Timestamp: ts}
}

func TestEnqueueListFlush(t *testing.T) {
	q := openTestQueue(t)
	now := time.Now()

	q.Enqueue(track("A", now.Add(-2*time.Hour)), "net down", true)
	q.Enqueue(track("B", now.Add(-1*time.Hour)), "net down", true)

	n, _ := q.Len()
	if n != 2 {
		t.Fatalf("len = %d, want 2", n)
	}

	fs := &fakeScrobbler{}
	if err := q.Flush(context.Background(), fs); err != nil {
		t.Fatal(err)
	}
	if len(fs.batches) != 1 || len(fs.batches[0]) != 2 {
		t.Fatalf("expected one batch of 2, got %v", fs.batches)
	}
	// oldest first
	if fs.batches[0][0].Artist != "A" {
		t.Errorf("expected oldest first, got %s", fs.batches[0][0].Artist)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue should be empty after flush, len = %d", n)
	}
}

func TestFlushKeepsFailedAndAborts(t *testing.T) {
	q := openTestQueue(t)
	now := time.Now()
	for i := 0; i < 60; i++ { // 2 batches
		q.Enqueue(track("A", now.Add(time.Duration(i-100)*time.Minute)), "x", true)
	}

	fs := &fakeScrobbler{errs: []error{errors.New("boom")}}
	err := q.Flush(context.Background(), fs)
	if err == nil {
		t.Fatal("expected flush error")
	}
	if len(fs.batches) != 1 {
		t.Fatalf("run should abort after first failed batch, got %d calls", len(fs.batches))
	}
	if n, _ := q.Len(); n != 60 {
		t.Errorf("all entries should survive, len = %d", n)
	}

	// The failed batch (50) gets a fresh LastFailed and loses force-retry;
	// the never-attempted tail (10) stays force-retryable (pano only marks
	// rows it actually submitted). A second flush thus submits only the tail.
	fs2 := &fakeScrobbler{}
	if err := q.Flush(context.Background(), fs2); err != nil {
		t.Fatal(err)
	}
	if len(fs2.batches) != 1 || len(fs2.batches[0]) != 10 {
		t.Fatalf("second flush should submit only the unattempted 10, got %v calls", fs2.batches)
	}
	if n, _ := q.Len(); n != 50 {
		t.Errorf("failed batch should remain in backoff, len = %d", n)
	}
}

func TestFlushDropsTerminalAndOld(t *testing.T) {
	q := openTestQueue(t)
	now := time.Now()

	q.Enqueue(track("old", now.Add(-15*24*time.Hour)), "x", true) // > 14 days
	q.Enqueue(track("bad", now.Add(-time.Hour)), "x", true)

	fs := &fakeScrobbler{errs: []error{&scrobble.APIError{Code: 6, Message: "invalid params"}}}
	if err := q.Flush(context.Background(), fs); err != nil {
		t.Fatal(err)
	}
	// old dropped without submission; bad submitted once, terminal, dropped
	if len(fs.batches) != 1 || len(fs.batches[0]) != 1 || fs.batches[0][0].Artist != "bad" {
		t.Fatalf("unexpected batches: %v", fs.batches)
	}
	if n, _ := q.Len(); n != 0 {
		t.Errorf("queue should be empty, len = %d", n)
	}
}

func TestDeferredJournalLifecycle(t *testing.T) {
	q := openTestQueue(t)
	now := time.Now()
	tr := track("A", now)

	// Journaled entries are invisible to flush and Len.
	q.AddDeferred(tr)
	if n, _ := q.Len(); n != 0 {
		t.Fatalf("deferred entries must not count as retryable, len = %d", n)
	}
	fs := &fakeScrobbler{}
	if err := q.Flush(context.Background(), fs); err != nil {
		t.Fatal(err)
	}
	if len(fs.batches) != 0 {
		t.Fatal("flush must not submit deferred entries")
	}

	// Resolving removes the journal entry.
	if err := q.ResolveDeferred(tr); err != nil {
		t.Fatal(err)
	}
	entries, _ := ListPending(filepath.Dir(q.path))
	if len(entries) != 0 {
		t.Fatalf("journal entry should be gone, got %v", entries)
	}
}

func TestPromoteDeferredRecoversAfterCrash(t *testing.T) {
	q := openTestQueue(t)
	q.AddDeferred(track("A", time.Now().Add(-time.Minute)))

	n, err := q.PromoteDeferred()
	if err != nil || n != 1 {
		t.Fatalf("promoted %d, err %v", n, err)
	}

	// Promoted entries flush immediately (canForceRetry).
	fs := &fakeScrobbler{}
	if err := q.Flush(context.Background(), fs); err != nil {
		t.Fatal(err)
	}
	if len(fs.batches) != 1 || fs.batches[0][0].Artist != "A" {
		t.Fatalf("promoted entry not flushed: %v", fs.batches)
	}
}

func TestQueueLocking(t *testing.T) {
	dir := t.TempDir()
	q1, err := OpenQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q1.Close()

	if _, err := OpenQueue(dir); err == nil {
		t.Error("second OpenQueue should fail while locked")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSession(dir); err == nil {
		t.Error("expected error for missing session")
	}
	// write + read back via the lastfm.Session type used in store
	err := SaveSession(dir, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Username != "user" || s.SessionKey != "key" {
		t.Errorf("unexpected session: %+v", s)
	}
}
