package csync

import (
	"iter"
	"sync"
	"sync/atomic"
)

// NewVersionedMap creates a new versioned, thread-safe map.
func NewVersionedMap[K comparable, V any]() *VersionedMap[K, V] {
	return &VersionedMap[K, V]{
		m: NewMap[K, V](),
	}
}

// VersionedMap is a thread-safe map that keeps track of its version.
// Callers can wait for version changes via WaitForChange instead of polling.
type VersionedMap[K comparable, V any] struct {
	m *Map[K, V]
	v atomic.Uint64

	// wakeMu protects wakeC: only the first waiter after a change
	// replaces the channel; subsequent waiters share it.
	wakeMu sync.Mutex
	wakeC  chan struct{}
}

// Get gets the value for the specified key from the map.
func (m *VersionedMap[K, V]) Get(key K) (V, bool) {
	return m.m.Get(key)
}

// Set sets the value for the specified key in the map and increments the version.
func (m *VersionedMap[K, V]) Set(key K, value V) {
	m.m.Set(key, value)
	m.bumpVersion()
}

// Del deletes the specified key from the map and increments the version.
func (m *VersionedMap[K, V]) Del(key K) {
	m.m.Del(key)
	m.bumpVersion()
}

// Seq2 returns an iter.Seq2 that yields key-value pairs from the map.
func (m *VersionedMap[K, V]) Seq2() iter.Seq2[K, V] {
	return m.m.Seq2()
}

// Copy returns a copy of the inner map.
func (m *VersionedMap[K, V]) Copy() map[K]V {
	return m.m.Copy()
}

// Len returns the number of items in the map.
func (m *VersionedMap[K, V]) Len() int {
	return m.m.Len()
}

// Version returns the current version of the map.
func (m *VersionedMap[K, V]) Version() uint64 {
	return m.v.Load()
}

// WaitForChange returns a channel that is closed when the next version
// change occurs after the call. If the version is already different from
// the provided baseline, the returned channel is already closed.
func (m *VersionedMap[K, V]) WaitForChange(baseline uint64) <-chan struct{} {
	// Fast path: version already changed.
	if m.v.Load() != baseline {
		c := make(chan struct{})
		close(c)
		return c
	}
	m.wakeMu.Lock()
	if m.v.Load() != baseline {
		// Version changed while we were waiting for the lock.
		m.wakeMu.Unlock()
		c := make(chan struct{})
		close(c)
		return c
	}
	if m.wakeC == nil {
		m.wakeC = make(chan struct{})
	}
	c := m.wakeC
	m.wakeMu.Unlock()
	return c
}

// bumpVersion increments the version and closes any pending wake channel
// so waiters return immediately.
func (m *VersionedMap[K, V]) bumpVersion() {
	m.v.Add(1)
	m.wakeMu.Lock()
	if m.wakeC != nil {
		close(m.wakeC)
		m.wakeC = nil
	}
	m.wakeMu.Unlock()
}
