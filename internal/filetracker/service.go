// Package filetracker provides functionality to track file reads in sessions.
package filetracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Service defines the interface for tracking file reads in sessions.
type Service interface {
	// RecordRead records when a file was read.
	RecordRead(ctx context.Context, sessionID, path string)

	// LastReadTime returns when a file was last read.
	// Returns zero time if never read.
	LastReadTime(ctx context.Context, sessionID, path string) time.Time

	// ListReadFiles returns the paths of all files read in a session.
	ListReadFiles(ctx context.Context, sessionID string) ([]string, error)

	// Close flushes pending writes and stops the background goroutine.
	// It is safe to call multiple times.
	Close()
}

type service struct {
	q   *db.Queries
	cwd string

	// writeCh buffers RecordRead calls so they don't block callers.
	// The channel is drained by a single background goroutine. It is never
	// closed: closing it would make any late RecordRead panic on send, so
	// shutdown is signaled via stop instead.
	writeCh chan readRecord
	// flushWake wakes the write loop to drain and write all pending
	// records before a reader queries the database.
	flushWake chan struct{}
	// stop requests the write loop to drain, flush and exit. It is closed
	// by Close; writeCh is never closed.
	stop chan struct{}
	closeOnce sync.Once
	done      chan struct{}

	// flushGen counts completed flushes. Readers wait for it to advance
	// after sending flushWake. It is an atomic counter polled with
	// runtime.Gosched instead of a channel/cond because the write loop
	// lives outside any synctest bubble a reader may run in, and waking
	// a bubble goroutine from outside is fatal.
	flushGen atomic.Uint64
}

type readRecord struct {
	sessionID string
	path      string
}

// NewService creates a new file tracker service.
func NewService(q *db.Queries) Service {
	cwd, err := os.Getwd()
	if err != nil {
		slog.Warn("Error getting current working directory in NewService", "error", err)
	}
	s := &service{
		q:         q,
		cwd:       cwd,
		writeCh:   make(chan readRecord, 256),
		flushWake: make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

func (s *service) writeLoop() {
	defer close(s.done)

	// batchTimer accumulates records for batch writes.
	// Instead of writing one-by-one, we collect up to 64 records or
	// flush after 50ms of idle time, whichever comes first.
	const (
		maxBatch     = 64
		batchTimeout = 50 * time.Millisecond
	)

	batch := make([]readRecord, 0, maxBatch)
	timer := time.NewTimer(batchTimeout)
	defer timer.Stop()

	flush := func() {
		for _, rec := range batch {
			if err := s.q.RecordFileRead(context.Background(), db.RecordFileReadParams{
				SessionID: rec.sessionID,
				Path:      s.relpath(rec.path),
			}); err != nil {
				slog.Error("Error recording file read", "error", err, "file", rec.path)
			}
		}
		batch = batch[:0]
		s.flushGen.Add(1)
	}

	// drain moves all queued records from writeCh into the batch so a
	// flush writes everything enqueued so far, not just the current batch.
	drain := func() {
		for {
			select {
			case rec, ok := <-s.writeCh:
				if !ok {
					return
				}
				batch = append(batch, rec)
			default:
				return
			}
		}
	}

	for {
		select {
		case rec, ok := <-s.writeCh:
			if !ok {
				// Defensive: writeCh is never closed today, but drain
				// whatever is left if it ever is.
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= maxBatch {
				flush()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(batchTimeout)
			}
		case <-s.flushWake:
			drain()
			flush()
		case <-s.stop:
			drain()
			flush()
			return
		case <-timer.C:
			if len(batch) > 0 {
				flush()
			}
			timer.Reset(batchTimeout)
		}
	}
}

// RecordRead records when a file was read. This is non-blocking: the record
// is sent to a buffered channel and written to SQLite by a background goroutine.
// If the channel is full (extremely unlikely), the write is dropped to avoid
// blocking the tool caller.
func (s *service) RecordRead(ctx context.Context, sessionID, path string) {
	rec := readRecord{sessionID: sessionID, path: path}
	select {
	case s.writeCh <- rec:
	case <-ctx.Done():
	default:
		// Channel full - drop the record rather than blocking.
		slog.Debug("File tracker write channel full, dropping record", "file", path)
	}
}

// syncFlush blocks until all records enqueued so far are written to the
// database. It is called before reads so callers never observe stale data.
func (s *service) syncFlush() {
	select {
	case <-s.done:
		// writeLoop has exited; Close already flushed everything.
		return
	default:
	}

	// Record the baseline BEFORE waking the loop. If we read it after, a
	// fast flush could complete first and bump flushGen past our baseline,
	// making us wait for a flush that will never come (and timeout).
	baseline := s.flushGen.Load()

	// Wake the write loop. If the wake slot is already full, a previous
	// request is still pending and the loop will flush anyway.
	select {
	case s.flushWake <- struct{}{}:
	default:
	}

	// Spin without blocking primitives: the write loop runs outside any
	// synctest bubble, so channel/cond waits would need cross-bubble
	// wakeups, which are fatal. Flushes are fast; this settles in a few
	// iterations. The iteration cap is a last-resort guard against a
	// wedged write loop.
	const maxSpins = 1_000_000
	for i := 0; i < maxSpins; i++ {
		if s.flushGen.Load() > baseline {
			return
		}
		select {
		case <-s.done:
			return
		default:
		}
		runtime.Gosched()
	}
	slog.Warn("File tracker flush timed out")
}

// LastReadTime returns when a file was last read.
// Returns zero time if never read.
func (s *service) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	s.syncFlush()
	readFile, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
	})
	if err != nil {
		return time.Time{}
	}

	return time.Unix(readFile.ReadAt, 0)
}

func (s *service) relpath(path string) string {
	path = filepath.Clean(path)
	if s.cwd == "" {
		return path
	}
	relpath, err := filepath.Rel(s.cwd, path)
	if err != nil {
		slog.Warn("Error getting relpath", "error", err)
		return path
	}
	return relpath
}

// ListReadFiles returns the paths of all files read in a session.
func (s *service) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	s.syncFlush()
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing read files: %w", err)
	}

	basepath := s.cwd
	if basepath == "" {
		var err error
		basepath, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getting working directory: %w", err)
		}
	}

	paths := make([]string, 0, len(readFiles))
	for _, rf := range readFiles {
		paths = append(paths, filepath.Join(basepath, rf.Path))
	}
	return paths, nil
}

// Close flushes pending writes and stops the background goroutine.
// It is safe to call multiple times.
func (s *service) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		// Wait for the write loop to drain and exit.
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			slog.Warn("File tracker close timed out after 5s")
		}
	})
}
