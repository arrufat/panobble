package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/arrufat/panobble/internal/lastfm"
	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/store"
)

// submitter receives tracker output, does the network I/O off the tracker
// goroutine, and feeds failures into the pending queue. Qualified tracks are
// journaled so a crash between qualification and commit loses nothing.
type submitter struct {
	client *lastfm.Client
	queue  *store.Queue
	log    *slog.Logger
	kick   chan struct{} // flush trigger after a successful scrobble
	wg     sync.WaitGroup
}

func newSubmitter(client *lastfm.Client, dataDir string, log *slog.Logger) (*submitter, error) {
	queue, err := store.OpenQueue(dataDir)
	if err != nil {
		return nil, err
	}
	return &submitter{
		client: client,
		queue:  queue,
		log:    log,
		kick:   make(chan struct{}, 1),
	}, nil
}

// close waits for in-flight submissions (bounded) and releases the queue.
func (s *submitter) close() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.log.Warn("timed out waiting for in-flight submissions")
	}
	s.queue.Close()
}

func (s *submitter) NowPlaying(t scrobble.Track) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.client.NowPlaying(ctx, t); err != nil {
			// Now-playing is best-effort; never queued.
			s.log.Warn("now-playing failed", "err", err)
		} else {
			s.log.Debug("now-playing sent", "artist", t.Artist, "title", t.Title)
		}
	}()
}

// Qualified journals a threshold-crossing track; its commit comes later.
func (s *submitter) Qualified(t scrobble.Track) {
	if err := s.queue.AddDeferred(t); err != nil {
		s.log.Error("journaling qualified track failed", "err", err)
	}
}

// Scrobble commits a qualified track: submit, then clear its journal entry
// (a failure converts it into a normal retryable pending instead).
func (s *submitter) Scrobble(t scrobble.Track) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err := s.client.Scrobble(ctx, []scrobble.Track{t})
		switch {
		case err == nil:
			s.log.Info("scrobbled", "artist", t.Artist, "title", t.Title)
			select {
			case s.kick <- struct{}{}:
			default:
			}

		case scrobble.Terminal(err):
			s.log.Warn("scrobble permanently rejected, dropping", "err", err,
				"artist", t.Artist, "title", t.Title)

		default:
			if scrobble.NeedsReauth(err) {
				s.log.Error("session key invalid — run: panobble auth", "err", err)
			} else {
				s.log.Warn("scrobble failed, queuing", "err", err)
			}
			if qerr := s.queue.Enqueue(t, err.Error(), scrobble.Retryable(err)); qerr != nil {
				s.log.Error("enqueue failed", "err", qerr)
			}
		}
		if err := s.queue.ResolveDeferred(t); err != nil {
			s.log.Error("clearing journal entry failed", "err", err)
		}
	}()
}

// flushLoop retries the pending queue: on start, hourly, and after each
// successful live scrobble. On start it also recovers scrobbles a previous
// daemon qualified but never committed.
func (s *submitter) flushLoop(ctx context.Context) {
	if n, err := s.queue.PromoteDeferred(); err != nil {
		s.log.Error("recovering uncommitted scrobbles failed", "err", err)
	} else if n > 0 {
		s.log.Info("recovered uncommitted scrobbles from previous run", "count", n)
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	flush := func() {
		n, _ := s.queue.Len()
		if n == 0 {
			return
		}
		s.log.Info("flushing pending scrobbles", "count", n)
		if err := s.queue.Flush(ctx, s.client); err != nil {
			s.log.Warn("pending flush incomplete", "err", err)
		}
	}

	flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flush()
		case <-s.kick:
			flush()
		}
	}
}
