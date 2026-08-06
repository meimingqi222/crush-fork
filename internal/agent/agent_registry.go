package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
)

type AgentStatus string

const (
	AgentStatusRunning AgentStatus = "running"
	AgentStatusIdle    AgentStatus = "idle"
	// AgentStatusParked marks a subagent whose SessionAgent instance has
	// been released (see AgentRegistry.SetParked) but whose registry entry
	// is kept around so it can be cold-revived: the child session's
	// conversation history is still in SQLite, and ProfileName/Role/
	// Isolation/ParentSessionID carry enough of the original spawn contract
	// to rebuild a SessionAgent from it. See
	// docs/refactor-subagent-continuation.md §3.2 for the full state
	// machine (running -> idle -> parked, with idle/parked both revivable
	// by an addressed message).
	AgentStatusParked  AgentStatus = "parked"
	AgentStatusAborted AgentStatus = "aborted"
)

type AgentKind string

const (
	AgentKindMain AgentKind = "main"
	AgentKindSub  AgentKind = "sub"
)

type AgentRef struct {
	ID          string
	DisplayName string
	Kind        AgentKind
	ParentID    string
	Status      AgentStatus
	Agent       SessionAgent
	// SessionID is the subagent's child session ID, populated once it is
	// known (see SetSessionID -- spawn time only knows the parent session;
	// the child session is created inside runSubAgentDirect, after
	// Register). Empty until then. This is what resumeSubagent passes as
	// subAgentParams.ExistingSessionID to warm/cold-revive the subagent.
	SessionID string
	CreatedAt time.Time

	// The following are populated at spawn time (see runSubagents'
	// registration in coordinator.go) and are the runtime contract cold
	// revive needs to rebuild a SessionAgent after the entry is parked and
	// its in-memory instance released -- see
	// docs/refactor-subagent-continuation.md §3.4. They are meaningless for
	// AgentKindMain refs.
	ProfileName     string // Subagent profile ID, passed to buildSubAgentForType on revive.
	ParentSessionID string // Parent session ID, for permission derivation and cost accounting.
	Role            string // Spawn-time role override, preserved across revives.
	Isolation       string // Resolved isolation ("worktree", "session", "none", or "") at spawn time.
}

type RegistryListener func()

type listenerEntry struct {
	id uint64
	fn RegistryListener
}

type AgentRegistry struct {
	mu           sync.RWMutex
	refs         map[string]*AgentRef
	listeners    []listenerEntry
	nextListenID uint64
}

var (
	globalRegistry *AgentRegistry
	registryOnce   sync.Once
)

func GlobalAgentRegistry() *AgentRegistry {
	registryOnce.Do(func() {
		globalRegistry = &AgentRegistry{
			refs: make(map[string]*AgentRef),
		}
	})
	return globalRegistry
}

func (r *AgentRegistry) Register(ref AgentRef) *AgentRef {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now()
	}
	entry := &ref
	r.refs[entry.ID] = entry
	r.mu.Unlock()
	r.fireListeners(snapshot)
	return entry
}

func (r *AgentRegistry) Unregister(id string) {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	delete(r.refs, id)
	r.mu.Unlock()
	r.fireListeners(snapshot)
}

func (r *AgentRegistry) Get(id string) (*AgentRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.refs[id]
	if !ok {
		return nil, false
	}
	return ref, true
}

func (r *AgentRegistry) List() []*AgentRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*AgentRef, 0, len(r.refs))
	for _, ref := range r.refs {
		result = append(result, ref)
	}
	return result
}

// ListVisibleTo returns the peers addressable by id: running, idle, and
// parked entries (docs/refactor-subagent-continuation.md §4 phase 2 item 1).
// Parked peers stay visible -- and addressable, via resumeSubagent's cold
// revive -- because a completed subagent is still a valid send_message/irc
// target; only Aborted (failed/canceled, no revive path) is excluded.
func (r *AgentRegistry) ListVisibleTo(id string) []*AgentRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*AgentRef, 0, len(r.refs))
	for _, ref := range r.refs {
		if ref.ID == id {
			continue
		}
		if !isAddressableStatus(ref.Status) {
			continue
		}
		result = append(result, ref)
	}
	return result
}

// isAddressableStatus reports whether status belongs to a peer that should
// appear in the IRC-visible roster: running, idle, or parked (revivable by
// an addressed message -- see AgentRegistry.SetParked). Aborted entries are
// excluded; they have no revive path (resumeSubagent returns a "cannot be
// resumed" error for them; see coordinator.resolveSubagentRef's caller in
// backgroundAgentMessenger).
func isAddressableStatus(status AgentStatus) bool {
	switch status {
	case AgentStatusRunning, AgentStatusIdle, AgentStatusParked:
		return true
	default:
		return false
	}
}

// agentSnapshot is a value copy of the AgentRef fields needed to describe a
// peer. Taking a copy while the registry lock is held is mandatory for any
// field SetStatus mutates: refs are stored by pointer, so reading Status off
// a *AgentRef returned by Get/List/ListVisibleTo races with a concurrent
// SetStatus write. Agent is set once at Register and never reassigned, so
// carrying it out of the lock is safe -- and necessary, because IsBusy()
// must not be called with r.mu held.
type agentSnapshot struct {
	ID          string
	DisplayName string
	Kind        AgentKind
	ParentID    string
	Status      AgentStatus
	Agent       SessionAgent
}

func snapshotOf(ref *AgentRef) agentSnapshot {
	return agentSnapshot{
		ID:          ref.ID,
		DisplayName: ref.DisplayName,
		Kind:        ref.Kind,
		ParentID:    ref.ParentID,
		Status:      ref.Status,
		Agent:       ref.Agent,
	}
}

// snapshot returns a lock-safe copy of id's peer fields.
func (r *AgentRegistry) snapshot(id string) (agentSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.refs[id]
	if !ok {
		return agentSnapshot{}, false
	}
	return snapshotOf(ref), true
}

// snapshotVisibleTo returns lock-safe copies of the peers visible to id,
// mirroring ListVisibleTo's filter. Prefer this over ListVisibleTo whenever
// the caller reads Status, which ListVisibleTo cannot expose safely.
func (r *AgentRegistry) snapshotVisibleTo(id string) []agentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]agentSnapshot, 0, len(r.refs))
	for _, ref := range r.refs {
		if ref.ID == id {
			continue
		}
		if !isAddressableStatus(ref.Status) {
			continue
		}
		result = append(result, snapshotOf(ref))
	}
	return result
}

// EffectiveStatus returns id's externally observable status, deriving Idle
// vs Running from the attached SessionAgent's busy state when one is
// present. This exists because the registry's own Idle/Running writes can go
// stale for long-lived agents -- most notably the primary agent, which is
// registered once at startup (see coordinator.go's registration of
// "0-Main") and has no code path that ever calls SetStatus on it again, so
// without this it reports Idle forever regardless of whether a turn is
// running. Deriving at query time is preferred over adding a second write
// path (e.g. SetStatus at run start/end) that could drift out of sync with
// the SessionAgent's actual run state.
//
// Terminal/parked statuses (Aborted, Parked) are returned unchanged: a
// finished or parked agent has no meaningful "busy" reading, and this must
// not resurrect it. (There is no AgentStatusCompleted -- see its removal
// note in docs/refactor-subagent-continuation.md §7 C6: the state machine's
// success terminal is Idle, demoted to Parked by the lifecycle TTL, not a
// separate "completed" status.)
func (r *AgentRegistry) EffectiveStatus(id string) (AgentStatus, bool) {
	snap, ok := r.snapshot(id)
	if !ok {
		return "", false
	}
	// IsBusy() runs after the registry lock is released: it may touch the
	// SessionAgent's own locking/state, and holding r.mu across a call into
	// external code risks a lock-ordering deadlock if that code ever calls
	// back into the registry (e.g. via OnChange listeners).
	return effectiveStatus(snap.Status, snap.Agent), true
}

// effectiveStatus derives the observable Idle/Running status for an agent
// given its registry-stored status and (possibly nil) live SessionAgent
// instance. See EffectiveStatus for why this derivation exists.
func effectiveStatus(status AgentStatus, agent SessionAgent) AgentStatus {
	if agent == nil {
		return status
	}
	switch status {
	case AgentStatusIdle, AgentStatusRunning:
		if agent.IsBusy() {
			return AgentStatusRunning
		}
		return AgentStatusIdle
	default:
		return status
	}
}

func (r *AgentRegistry) SetStatus(id string, status AgentStatus) {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	changed := false
	if ref, ok := r.refs[id]; ok && ref.Status != status {
		ref.Status = status
		changed = true
	}
	r.mu.Unlock()
	if changed {
		r.fireListeners(snapshot)
	}
}

// SetParked demotes id to AgentStatusParked and releases its in-memory
// SessionAgent reference. This is the park-as-status replacement for the
// old Unregister-on-park behavior (see subagentLifecycleManager.Park):
// parked entries stay addressable by ID/DisplayName and are cold-revived
// from SQLite by coordinator.resumeSubagent, so the entry must survive, but
// the live SessionAgent instance must not -- keeping it around after park
// would defeat the point of releasing memory for long-lived parent
// sessions, and (more subtly) would leave a SessionAgent reachable through
// the registry that no other code owns or drains. No-op for unknown ids.
func (r *AgentRegistry) SetParked(id string) {
	r.mu.Lock()
	snapshot := r.snapshotListeners()
	changed := false
	if ref, ok := r.refs[id]; ok {
		if ref.Status != AgentStatusParked {
			ref.Status = AgentStatusParked
			changed = true
		}
		if ref.Agent != nil {
			ref.Agent = nil
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.fireListeners(snapshot)
	}
}

// SetSessionID records id's child session ID once it becomes known. No-op
// for unknown ids. Does not fire listeners: the child session ID is
// revival bookkeeping, not an observable status/roster change peers or the
// UI need to react to.
func (r *AgentRegistry) SetSessionID(id, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref, ok := r.refs[id]; ok {
		ref.SessionID = sessionID
	}
}

// FullSnapshot returns a lock-safe copy of id's complete ref, including the
// revival fields (ProfileName, ParentSessionID, Role, Isolation, SessionID)
// that agentSnapshot omits because peer-facing code (the IRC roster) never
// needs them. Callers that read fields beyond the peer-safe subset --
// resumeSubagent and send_message addressing being the motivating examples
// -- must use this rather than Get: refs are stored by pointer and
// SetStatus/SetParked/SetSessionID mutate them in place, so reading fields
// off a *AgentRef returned by Get races with a concurrent writer.
func (r *AgentRegistry) FullSnapshot(id string) (AgentRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.refs[id]
	if !ok {
		return AgentRef{}, false
	}
	return *ref, true
}

// ListSnapshot returns lock-safe copies of every ref, parked entries
// included (unlike ListVisibleTo/snapshotVisibleTo, which stay
// running/idle-only in phase 1 -- see
// docs/refactor-subagent-continuation.md §4 phase 2 item 1). Used for
// addressing: resolving a send_message agent_id/DisplayName needs to find
// parked and aborted subagents too, not just the IRC-visible ones.
func (r *AgentRegistry) ListSnapshot() []AgentRef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]AgentRef, 0, len(r.refs))
	for _, ref := range r.refs {
		result = append(result, *ref)
	}
	return result
}

func (r *AgentRegistry) OnChange(listener RegistryListener) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextListenID
	r.nextListenID++
	r.listeners = append(r.listeners, listenerEntry{id: id, fn: listener})
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, entry := range r.listeners {
			if entry.id == id {
				r.listeners = append(r.listeners[:i], r.listeners[i+1:]...)
				break
			}
		}
	}
}

func (r *AgentRegistry) snapshotListeners() []RegistryListener {
	snapshot := make([]RegistryListener, len(r.listeners))
	for i, entry := range r.listeners {
		snapshot[i] = entry.fn
	}
	return snapshot
}

func (r *AgentRegistry) fireListeners(listeners []RegistryListener) {
	for _, fn := range listeners {
		fn()
	}
}

func (r *AgentRegistry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = make(map[string]*AgentRef)
	r.listeners = nil
	r.nextListenID = 0
}

func (r *AgentRegistry) AsIrcRegistry() *ircRegistryAdapter {
	return &ircRegistryAdapter{registry: r}
}

type ircRegistryAdapter struct {
	registry *AgentRegistry
}

// peerInfo projects a snapshot into the model/tool-facing peer shape,
// deriving the observable status outside the registry lock. Parked peers get
// a Note telling the model a message will bring them back (see
// AgentRegistry.SetParked / coordinator.resumeSubagent): without it a parked
// entry in `irc list` reads exactly like an idle one, and nothing tells the
// model that addressing it triggers a cold revive (rebuilding the SessionAgent
// from its saved profile) rather than an instant delivery.
func peerInfo(snap agentSnapshot) agenttools.IrcPeerInfo {
	status := effectiveStatus(snap.Status, snap.Agent)
	info := agenttools.IrcPeerInfo{
		ID:          snap.ID,
		DisplayName: snap.DisplayName,
		Kind:        string(snap.Kind),
		Status:      string(status),
		ParentID:    snap.ParentID,
	}
	if status == AgentStatusParked {
		info.Note = "message revives"
	}
	return info
}

func (a *ircRegistryAdapter) Get(id string) (agenttools.IrcPeerInfo, bool) {
	snap, ok := a.registry.snapshot(id)
	if !ok {
		return agenttools.IrcPeerInfo{}, false
	}
	return peerInfo(snap), true
}

func (a *ircRegistryAdapter) ListVisibleTo(id string) []agenttools.IrcPeerInfo {
	snaps := a.registry.snapshotVisibleTo(id)
	peers := make([]agenttools.IrcPeerInfo, 0, len(snaps))
	for _, snap := range snaps {
		peers = append(peers, peerInfo(snap))
	}
	return peers
}

func renderIrcPeerRoster(registry *AgentRegistry, selfID string) string {
	snaps := registry.snapshotVisibleTo(selfID)
	if len(snaps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<irc_peers>\n")
	b.WriteString("You are part of a multi-agent team. The following agents are currently live and reachable via the `irc` tool.\n")
	b.WriteString("Use `irc` with op=send to communicate with any peer. Use op=list to check current availability.\n\n")
	for _, snap := range snaps {
		peer := peerInfo(snap)
		if peer.Note != "" {
			fmt.Fprintf(&b, "- `%s` — %s (%s, %s — %s)\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status, peer.Note)
		} else {
			fmt.Fprintf(&b, "- `%s` — %s (%s, %s)\n", peer.ID, peer.DisplayName, peer.Kind, peer.Status)
		}
	}
	b.WriteString("</irc_peers>")
	return b.String()
}
