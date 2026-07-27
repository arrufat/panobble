package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/arrufat/panobble/internal/scrobble"
	"github.com/arrufat/panobble/internal/store"
)

// submitter receives tracker output, does the network I/O off the tracker
// goroutine, and feeds failures into the pending queue. Qualified tracks are
// journaled so a crash between qualification and commit loses nothing.
type submitter struct {
	clients []scrobble.Scrobbler
	queue   *store.Queue
	log     *slog.Logger
	kick    chan struct{} // flush trigger after a successful scrobble
	wg      sync.WaitGroup
}

func newSubmitter(clients []scrobble.Scrobbler, dataDir string, log *slog.Logger) (*submitter, error) {
	queue, err := store.OpenQueue(dataDir)
	if err != nil {
		return nil, err
	}
	return &submitter{
		clients: clients,
		queue:   queue,
		log:     log,
		kick:    make(chan struct{}, 1),
	}, nil
}

func (s *submitter) serviceNames() []string {
	names := make([]string, len(s.clients))
	for i, c := range s.clients {
		names[i] = c.Name()
	}
	return names
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
	for _, c := range s.clients {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := c.NowPlaying(ctx, t); err != nil {
				// Now-playing is best-effort; never queued.
				s.log.Warn("now-playing failed", "service", c.Name(), "err", err)
			} else {
				s.log.Debug("now-playing sent", "service", c.Name(),
					"artist", t.Artist, "title", t.Title)
			}
		}()
	}
}

// Qualified journals a threshold-crossing track; its commit comes later.
func (s *submitter) Qualified(t scrobble.Track) {
	if err := s.queue.AddDeferred(t); err != nil {
		s.log.Error("journaling qualified track failed", "err", err)
	}
}

// Scrobble commits a qualified track: submit to every service, then clear
// its journal entry (a failure converts it into a per-service retryable
// pending instead).
func (s *submitter) Scrobble(t scrobble.Track) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		var inner sync.WaitGroup
		for _, c := range s.clients {
			inner.Add(1)
			go func() {
				defer inner.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				s.submitOne(ctx, c, t)
			}()
		}
		inner.Wait()

		if err := s.queue.ResolveDeferred(t); err != nil {
			s.log.Error("clearing journal entry failed", "err", err)
		}
	}()
}

func (s *submitter) submitOne(ctx context.Context, c scrobble.Scrobbler, t scrobble.Track) {
	err := c.Scrobble(ctx, []scrobble.Track{t})
	switch {
	case err == nil:
		s.log.Info("scrobbled", "service", c.Name(), "artist", t.Artist, "title", t.Title)
		select {
		case s.kick <- struct{}{}:
		default:
		}

	case scrobble.Terminal(err):
		s.log.Warn("scrobble permanently rejected, dropping", "service", c.Name(),
			"err", err, "artist", t.Artist, "title", t.Title)

	default:
		if scrobble.NeedsReauth(err) {
			s.log.Error("credentials invalid — fix auth", "service", c.Name(), "err", err)
		} else {
			s.log.Warn("scrobble failed, queuing", "service", c.Name(), "err", err)
		}
		if qerr := s.queue.Enqueue(t, c.Name(), err.Error(), scrobble.Retryable(err)); qerr != nil {
			s.log.Error("enqueue failed", "err", qerr)
		}
	}
}

// flushLoop retries the pending queue: on start, hourly, and after each
// successful live scrobble. On start it also recovers scrobbles a previous
// daemon qualified but never committed.
func (s *submitter) flushLoop(ctx context.Context) {
	if n, err := s.queue.PromoteDeferred(s.serviceNames()); err != nil {
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
		if err := s.queue.Flush(ctx, s.clients); err != nil {
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
