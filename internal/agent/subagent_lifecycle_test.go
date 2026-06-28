package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubagentLifecycleManager_AdoptThenPark(t *testing.T) {
	// No t.Parallel: time.AfterFunc fires asynchronously and we poll.
	childApps := &sync.Map{}
	m := newSubagentLifecycleManager(nil, childApps)

	childApps.Store("child-1", "fake-agent")
	m.Adopt("child-1", "agent-1", 20*time.Millisecond)

	require.True(t, m.IsAdopted("child-1"))
	_, ok := childApps.Load("child-1")
	require.True(t, ok, "agent should stay live during TTL window")

	// Wait for TTL to fire.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && m.IsAdopted("child-1") {
		time.Sleep(5 * time.Millisecond)
	}
	require.False(t, m.IsAdopted("child-1"), "entry should be parked after TTL")

	_, ok = childApps.Load("child-1")
	require.False(t, ok, "childSessionAgents entry removed after park")
}

func TestSubagentLifecycleManager_RevokeCancelsTimer(t *testing.T) {
	childApps := &sync.Map{}
	m := newSubagentLifecycleManager(nil, childApps)

	childApps.Store("child-2", "fake-agent")
	m.Adopt("child-2", "agent-2", 20*time.Millisecond)
	m.Revoke("child-2")

	require.False(t, m.IsAdopted("child-2"))
	// Wait past the original TTL; the agent must survive because Revoke
	// cancelled the timer without parking.
	time.Sleep(60 * time.Millisecond)
	_, ok := childApps.Load("child-2")
	require.True(t, ok, "Revoke must not remove childSessionAgents entry")
}

func TestSubagentLifecycleManager_ParkRemovesEntry(t *testing.T) {
	childApps := &sync.Map{}
	m := newSubagentLifecycleManager(nil, childApps)

	childApps.Store("child-3", "fake-agent")
	m.Adopt("child-3", "agent-3", time.Hour)
	m.Park("child-3")

	require.False(t, m.IsAdopted("child-3"))
	_, ok := childApps.Load("child-3")
	require.False(t, ok, "Park removes childSessionAgents entry")
}

func TestSubagentLifecycleManager_ParkAll(t *testing.T) {
	childApps := &sync.Map{}
	m := newSubagentLifecycleManager(nil, childApps)

	childApps.Store("a", "agent-a")
	childApps.Store("b", "agent-b")
	m.Adopt("a", "agent-a", time.Hour)
	m.Adopt("b", "agent-b", time.Hour)

	m.ParkAll()
	require.False(t, m.IsAdopted("a"))
	require.False(t, m.IsAdopted("b"))
	_, ok := childApps.Load("a")
	require.False(t, ok)
	_, ok = childApps.Load("b")
	require.False(t, ok)
}

func TestSubagentLifecycleManager_ReAdoptReplacesTimer(t *testing.T) {
	childApps := &sync.Map{}
	m := newSubagentLifecycleManager(nil, childApps)

	childApps.Store("child-4", "fake-agent")
	// First adopt with long TTL.
	m.Adopt("child-4", "agent-4", time.Hour)
	// Re-adopt with short TTL must replace the timer.
	m.Adopt("child-4", "agent-4", 20*time.Millisecond)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && m.IsAdopted("child-4") {
		time.Sleep(5 * time.Millisecond)
	}
	require.False(t, m.IsAdopted("child-4"), "re-adopted short TTL should have fired")
}

func TestSubagentLifecycleManager_NilSafe(t *testing.T) {
	var m *subagentLifecycleManager
	// All methods must be nil-safe.
	require.NotPanics(t, func() {
		m.Adopt("x", "y", time.Second)
		m.Revoke("x")
		m.Park("x")
		m.ParkAll()
		_ = m.IsAdopted("x")
	})
}
