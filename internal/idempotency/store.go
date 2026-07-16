// Package idempotency provides bounded replay of mutation outcomes.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrConflict = errors.New("idempotency key reused with different parameters")
	ErrCapacity = errors.New("idempotency store capacity exhausted")
	ErrClosed   = errors.New("idempotency store is closed")
)

const (
	defaultTTL        = 10 * time.Minute
	defaultMaxEntries = 4096
)

type Config struct {
	TTL        time.Duration
	MaxEntries int
	Clock      func() time.Time
}

// Outcome is an application-owned immutable result/error pair.
type Outcome struct {
	Value   any
	Failure any
}

type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	config  Config
	closed  bool
}

type entry struct {
	hash      string
	createdAt time.Time
	done      chan struct{}
	outcome   Outcome
	complete  bool
}

func New(config Config) *Store {
	if config.TTL <= 0 {
		config.TTL = defaultTTL
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = defaultMaxEntries
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Store{entries: make(map[string]*entry), config: config}
}

// Execute runs fn once per scope/request ID and replays the exact outcome to
// concurrent or later duplicates. The request ID must be UUID-like.
func (s *Store) Execute(
	ctx context.Context,
	scope string,
	requestID string,
	payload any,
	fn func() Outcome,
) (Outcome, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return Outcome{}, err
	}
	hash, err := payloadHash(payload)
	if err != nil {
		return Outcome{}, err
	}
	key := scope + "\x00" + requestID

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Outcome{}, ErrClosed
	}
	s.pruneLocked(s.config.Clock())
	if current := s.entries[key]; current != nil {
		if current.hash != hash {
			s.mu.Unlock()
			return Outcome{}, ErrConflict
		}
		done := current.done
		s.mu.Unlock()
		select {
		case <-done:
			return current.outcome, nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	if len(s.entries) >= s.config.MaxEntries {
		s.evictOldestCompletedLocked()
	}
	if len(s.entries) >= s.config.MaxEntries {
		s.mu.Unlock()
		return Outcome{}, ErrCapacity
	}
	current := &entry{hash: hash, createdAt: s.config.Clock(), done: make(chan struct{})}
	s.entries[key] = current
	s.mu.Unlock()

	outcome := fn()
	s.mu.Lock()
	if current.complete {
		outcome = current.outcome
		s.mu.Unlock()
		return outcome, nil
	}
	current.outcome = outcome
	current.complete = true
	close(current.done)
	s.mu.Unlock()
	return outcome, nil
}

func (s *Store) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, current := range s.entries {
		if !current.complete {
			current.outcome = Outcome{Failure: ErrClosed}
			current.complete = true
			close(current.done)
		}
	}
	clear(s.entries)
	s.mu.Unlock()
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-s.config.TTL)
	for key, current := range s.entries {
		if current.complete && current.createdAt.Before(cutoff) {
			delete(s.entries, key)
		}
	}
}

func (s *Store) evictOldestCompletedLocked() {
	var oldestKey string
	var oldest *entry
	for key, current := range s.entries {
		if !current.complete || oldest != nil && !current.createdAt.Before(oldest.createdAt) {
			continue
		}
		oldestKey, oldest = key, current
	}
	if oldest != nil {
		delete(s.entries, oldestKey)
	}
}

func payloadHash(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
