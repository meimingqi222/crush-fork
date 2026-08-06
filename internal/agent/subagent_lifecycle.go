package agent

import (
	"log/slog"
	"sync"
	"time"
)

// defaultSubagentAdoptTTL is how long a successfully completed subagent's
// SessionAgent instance is kept live (adoptable) after it returns. Within
// this window a follow-up addressed to the subagent (via send_message,
// currently -- see coordinator.resumeSubagent) can reuse the in-memory
// SessionAgent (warm revive) instead of rebuilding one from the SQLite
// history (cold revive). After the TTL fires the entry is parked: the
// childSessionAgents entry is deleted and the AgentRegistry entry is
// demoted to AgentStatusParked (its live SessionAgent reference cleared,
// but the entry itself kept -- see AgentRegistry.SetParked), leaving the
// persisted session messages plus enough of the original spawn contract
// (ProfileName/Role/Isolation/ParentSessionID on the AgentRef) for a future
// cold revive to rebuild the SessionAgent from scratch.
const defaultSubagentAdoptTTL = 5 * time.Minute

// subagentLifecycleManager manages the keep-alive window for completed
// subagents. It is the crush equivalent of oh-my-pi's AgentLifecycleManager:
//
//   - Adopt: a successfully completed subagent is kept live for a TTL window.
//     During this window the SessionAgent instance stays in childSessionAgents
//     so ModelForSession lookups keep working, and the AgentRegistry entry
//     stays Idle so IRC peers can still address it.
//   - Revoke: cancels the pending TTL timer without cleaning up the tracked
//     entries. Used when the same child session is about to be reused by a
//     new agent tool call (runSubAgentDirect re-entry) so the stale timer
//     does not later delete the freshly-stored SessionAgent.
//   - Park: cancels the timer, removes the childSessionAgents entry, and
//     demotes the AgentRegistry entry to AgentStatusParked (see
//     AgentRegistry.SetParked) rather than unregistering it. Invoked when
//     the TTL fires, or proactively when the parent session is torn down.
//     The registry entry survives so the subagent stays addressable and
//     cold-revivable; only its in-memory SessionAgent is released.
//
// The manager itself does not own the SessionAgent instances; it only
// schedules their removal. childSessionAgents (a sync.Map on the coordinator)
// remains the source of truth for live agents.
type subagentLifecycleManager struct {
	mu               sync.Mutex
	entries          map[string]*lifecycleEntry
	registry         *AgentRegistry
	childSessionApps *sync.Map // *coordinator.childSessionAgents (childSessionID -> SessionAgent)
}

type lifecycleEntry struct {
	agentID string
	timer   *time.Timer
}

func newSubagentLifecycleManager(registry *AgentRegistry, childSessionApps *sync.Map) *subagentLifecycleManager {
	return &subagentLifecycleManager{
		entries:          make(map[string]*lifecycleEntry),
		registry:         registry,
		childSessionApps: childSessionApps,
	}
}

// Adopt arms a TTL timer for childSessionID. When the timer fires the entry is
// parked. If an existing entry is already armed it is replaced. Safe to call
// concurrently with Revoked/Park.
func (m *subagentLifecycleManager) Adopt(childSessionID, agentID string, ttl time.Duration) {
	if m == nil || childSessionID == "" {
		return
	}
	if ttl <= 0 {
		ttl = defaultSubagentAdoptTTL
	}
	m.mu.Lock()
	// Cancel any prior timer for this session.
	if old := m.entries[childSessionID]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	entry := &lifecycleEntry{agentID: agentID}
	entry.timer = time.AfterFunc(ttl, func() {
		m.Park(childSessionID)
	})
	m.entries[childSessionID] = entry
	m.mu.Unlock()
}

// Revoke cancels the TTL timer for childSessionID without removing the tracked
// childSessionAgents / AgentRegistry entries. Use this before reusing the
// child session for a new run, so the stale timer cannot later evict the
// freshly-stored SessionAgent.
func (m *subagentLifecycleManager) Revoke(childSessionID string) {
	if m == nil || childSessionID == "" {
		return
	}
	m.mu.Lock()
	if entry := m.entries[childSessionID]; entry != nil {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		delete(m.entries, childSessionID)
	}
	m.mu.Unlock()
}

// Park cancels the timer, removes the childSessionAgents entry, and demotes
// the AgentRegistry entry to AgentStatusParked (releasing its in-memory
// SessionAgent reference, but keeping the entry itself so the subagent
// stays addressable and cold-revivable -- see AgentRegistry.SetParked).
// This is the normal eviction path when the TTL fires.
func (m *subagentLifecycleManager) Park(childSessionID string) {
	if m == nil || childSessionID == "" {
		return
	}
	m.mu.Lock()
	entry := m.entries[childSessionID]
	delete(m.entries, childSessionID)
	m.mu.Unlock()
	if entry == nil {
		return
	}
	if m.childSessionApps != nil {
		m.childSessionApps.Delete(childSessionID)
	}
	if m.registry != nil && entry.agentID != "" {
		m.registry.SetParked(entry.agentID)
	}
	slog.Debug("Subagent lifecycle TTL expired, parked agent",
		"child_session_id", childSessionID,
		"agent_id", entry.agentID,
	)
}

// ParkAll parks every tracked entry. Used during coordinator shutdown.
func (m *subagentLifecycleManager) ParkAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Park(id)
	}
}

// IsAdopted reports whether childSessionID is within its keep-alive window.
func (m *subagentLifecycleManager) IsAdopted(childSessionID string) bool {
	if m == nil || childSessionID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[childSessionID]
	return ok
}
